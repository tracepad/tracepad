package mapping

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// The OTLP/JSON encoding (spec 019 #7). Every OpenTelemetry SDK can emit it —
// `OTEL_EXPORTER_OTLP_PROTOCOL=http/json` — and `docs/ingest.md` had said "not
// implemented" since spec 002. The encoding is transport, not meaning: what
// comes out of here is the same `ResourceSpans` the protobuf path produces, and
// the mapper below cannot tell which door a span came in by.
//
// One rule separates OTLP/JSON from `protojson`, and it is the rule every
// collector implements: the OTLP specification prescribes **hex** for
// `traceId`, `spanId`, `parentSpanId` and the ids inside a link, where
// protojson would write and read base64. So the ids are rewritten on the way
// in and on the way out, and protojson does the rest of the work — which is
// what keeps "the same mapper over the same decoded message" true rather than
// aspirational.
//
// Rewritten by name over the parsed document rather than by walking the
// generated types: the names below are the only `bytes` fields in the trace
// message that carry an id, and a value's own encoding (`bytesValue` in an
// attribute) stays base64 exactly as the specification says it should.

// ContentTypeJSON is the media type of the OTLP/JSON encoding.
const ContentTypeJSON = "application/json"

// idFields are the keys whose values are hex in OTLP/JSON and base64 in
// protobuf-JSON. Both spellings of each: protojson accepts the proto field
// name beside the lowerCamelCase one, and an SDK may emit either.
var idFields = map[string]bool{
	"traceId": true, "trace_id": true,
	"spanId": true, "span_id": true,
	"parentSpanId": true, "parent_span_id": true,
}

// resourceSpansKeys are the two spellings of the envelope's one field.
var resourceSpansKeys = []string{"resourceSpans", "resource_spans"}

// unmarshalJSON is how a message is read. Unknown fields are discarded rather
// than refused, which is what the protobuf path does too (`DecodeExportRequest`
// skips them, as protobuf requires): an exporter on a newer OTLP version must
// keep ingesting.
var unmarshalJSON = protojson.UnmarshalOptions{DiscardUnknown: true}

// DecodeExportRequestJSON unpacks an OTLP/JSON ExportTraceServiceRequest into
// its ResourceSpans, mirroring DecodeExportRequest's bargain: a ResourceSpans
// that does not decode is skipped and counted rather than failing the export,
// because one unusable part must not destroy the usable rest (spec 002 #13).
//
// An id that is not hex is the exception, and it is an error naming the field:
// it is the one mistake a client makes by sending protobuf-JSON in place of
// OTLP/JSON, and silently reading base64 as well would make a body mean two
// things (spec 019 #7).
func DecodeExportRequestJSON(body []byte) ([]*tracepb.ResourceSpans, int, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		// An empty body is a valid, empty export, exactly as it is on
		// the protobuf path.
		return nil, 0, nil
	}
	var envelope map[string]json.RawMessage
	if err := decodeJSONNumbers(body, &envelope); err != nil {
		return nil, 0, fmt.Errorf("%w: the body is not a JSON object", ErrMalformedBody)
	}

	var raw json.RawMessage
	for _, key := range resourceSpansKeys {
		if value, found := envelope[key]; found {
			raw = value
			break
		}
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, nil
	}
	var elements []json.RawMessage
	if err := decodeJSONNumbers(raw, &elements); err != nil {
		return nil, 0, fmt.Errorf("%w: resourceSpans is not an array", ErrMalformedBody)
	}

	var out []*tracepb.ResourceSpans
	var malformed int
	for i, element := range elements {
		converted, err := hexIDsToBase64(element, fmt.Sprintf("resourceSpans[%d]", i))
		if err != nil {
			return nil, malformed, err
		}
		rs := &tracepb.ResourceSpans{}
		if err := unmarshalJSON.Unmarshal(converted, rs); err != nil {
			malformed++
			continue
		}
		out = append(out, rs)
	}
	return out, malformed, nil
}

// EncodeExportResponseJSON renders an ExportTraceServiceResponse in the
// encoding the request arrived in (spec 019 #7). A fully accepted export
// answers with the empty message — `{}` rather than the protobuf path's zero
// bytes, because zero bytes is not JSON and a client that parses the response
// would have to special-case it.
func EncodeExportResponseJSON(rejectedSpans int64, errorMessage string) []byte {
	if rejectedSpans == 0 && errorMessage == "" {
		return []byte("{}")
	}
	partial := map[string]any{}
	if rejectedSpans != 0 {
		// A string, which is what protojson writes for a 64-bit integer
		// and what every OTLP client accepts; the decoder here takes
		// either (#7).
		partial["rejectedSpans"] = fmt.Sprint(rejectedSpans)
	}
	if errorMessage != "" {
		partial["errorMessage"] = errorMessage
	}
	encoded, err := json.Marshal(map[string]any{"partialSuccess": partial})
	if err != nil {
		// Two strings and a map; unreachable, and an empty message is
		// still a true statement about an export that was accepted.
		return []byte("{}")
	}
	return encoded
}

// DecodeExportResponseJSON is DecodeExportResponse for a receiver that
// answered in the JSON encoding. `rejectedSpans` may be a string or a number,
// as 64-bit integers may be throughout the encoding.
func DecodeExportResponseJSON(body []byte) (rejectedSpans int64, errorMessage string) {
	var response struct {
		PartialSuccess struct {
			// Raw rather than a number: our own server writes the
			// protojson spelling, which is a quoted integer, and
			// another receiver may write a bare one. Both are the
			// encoding, so both are read.
			RejectedSpans json.RawMessage `json:"rejectedSpans"`
			ErrorMessage  string          `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, ""
	}
	rejected, err := strconv.ParseInt(
		strings.Trim(string(response.PartialSuccess.RejectedSpans), `"`), 10, 64)
	if err != nil {
		rejected = 0
	}
	return rejected, response.PartialSuccess.ErrorMessage
}

// EncodeExportRequestJSON is the inverse of DecodeExportRequestJSON. Like its
// protobuf twin it exists for tests and fixture generation — the server never
// encodes a request — and it is what turns a built `.pb` corpus into the same
// corpus in the JSON encoding, ids and all (spec 019, Testing).
func EncodeExportRequestJSON(resourceSpans []*tracepb.ResourceSpans) ([]byte, error) {
	elements := make([]json.RawMessage, 0, len(resourceSpans))
	for _, rs := range resourceSpans {
		encoded, err := protojson.Marshal(rs)
		if err != nil {
			return nil, err
		}
		converted, err := base64IDsToHex(encoded)
		if err != nil {
			return nil, err
		}
		elements = append(elements, converted)
	}
	return json.Marshal(map[string]any{"resourceSpans": elements})
}

// hexIDsToBase64 rewrites the id fields of one document from the OTLP/JSON
// encoding into the one protojson reads. `path` names where the document sits
// in the request, so a refusal points at the field a client has to fix.
func hexIDsToBase64(document json.RawMessage, path string) (json.RawMessage, error) {
	var value any
	if err := decodeJSONNumbers(document, &value); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON object", ErrMalformedBody, path)
	}
	if err := rewriteIDs(value, path, hexToBase64); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// base64IDsToHex is the other direction, for the encoder.
func base64IDsToHex(document json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := decodeJSONNumbers(document, &value); err != nil {
		return nil, err
	}
	if err := rewriteIDs(value, "resourceSpans", base64ToHex); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// rewriteIDs walks a decoded document and applies `convert` to every id field
// it finds, whatever its depth: a span's three, and the two inside each link.
func rewriteIDs(value any, path string, convert func(string) (string, error)) error {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			where := path + "." + key
			if idFields[key] {
				text, ok := child.(string)
				if !ok {
					return fmt.Errorf("%w: %s must be a string", ErrMalformedBody, where)
				}
				converted, err := convert(text)
				if err != nil {
					return fmt.Errorf("%w: %s %s", ErrMalformedBody, where, err)
				}
				node[key] = converted
				continue
			}
			if err := rewriteIDs(child, where, convert); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range node {
			if err := rewriteIDs(child, fmt.Sprintf("%s[%d]", path, i), convert); err != nil {
				return err
			}
		}
	}
	return nil
}

// hexToBase64 reads one id as OTLP/JSON writes it.
//
// The length is not checked here. A span id of two bytes is not an id, but it
// is a *readable* one, and refusing the whole export over it would cost the
// batch the spans that were fine — the mapper skips such a span and reports it
// through partial success, which is where that judgement belongs (spec 002
// #13). What this refuses is an id that is not hex at all, which is a client
// sending protobuf-JSON.
func hexToBase64(value string) (string, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf(
			"must be hex-encoded as the OTLP/JSON encoding prescribes, got %q", elide(value))
	}
	return base64.StdEncoding.EncodeToString(decoded), nil
}

// base64ToHex is the encoder's direction: protojson wrote base64, OTLP/JSON
// wants hex.
func base64ToHex(value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("is not base64: %w", err)
	}
	return hex.EncodeToString(decoded), nil
}

// decodeJSONNumbers unmarshals with numbers kept as their literal text.
// `startTimeUnixNano` sent as a JSON number is a nanosecond instant well past
// what a float64 holds exactly, and reading it through `any` the ordinary way
// would round it by a few hundred nanoseconds — a value that still looks right.
func decodeJSONNumbers(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("the body must carry exactly one JSON value")
	}
	return nil
}

// elide shortens a value for an error message: a base64 trace id is 24
// characters and worth quoting whole, a pasted document is not.
func elide(value string) string {
	const limit = 48
	if len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit]) + "…"
}
