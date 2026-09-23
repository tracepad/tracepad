package mapping

import (
	"bytes"
	"encoding/json"
	"fmt"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// ExportBody is a decoded export that remembers the body it came from, so that
// the ResourceSpans a media walk rewrote can be written back into it and
// everything else left byte for byte (spec 041 #5): the blocks that did not
// decode, the envelope's unknown fields, and every ResourceSpans the walk did
// not touch. The raw archive is the client's body with the media factored out,
// and nothing more than that is allowed to change.
type ExportBody struct {
	ResourceSpans []*tracepb.ResourceSpans
	// Unreadable counts the ResourceSpans that did not decode.
	Unreadable int

	source []byte
	asJSON bool
	// spans is, for the protobuf encoding, where each decoded
	// ResourceSpans field sits in source: from its tag to its end.
	spans [][2]int
	// The JSON encoding keeps which of the two spellings held the array,
	// and which of its elements each decoded ResourceSpans came from.
	key       string
	positions []int
}

// DecodeExportBody decodes an export in either encoding.
func DecodeExportBody(body []byte, asJSON bool) (*ExportBody, error) {
	if asJSON {
		return decodeExportJSON(body)
	}
	return decodeExportProto(body)
}

// Encode writes the body back with the changed ResourceSpans re-encoded in
// place. Nothing changed is the source itself.
func (b *ExportBody) Encode(changed []bool) ([]byte, error) {
	if !anyTrue(changed) {
		return b.source, nil
	}
	if b.asJSON {
		return b.encodeJSON(changed)
	}
	var out []byte
	previous := 0
	for i, span := range b.spans {
		if i >= len(changed) || !changed[i] {
			continue
		}
		encoded, err := proto.Marshal(b.ResourceSpans[i])
		if err != nil {
			return nil, fmt.Errorf("re-encode resource spans: %w", err)
		}
		out = append(out, b.source[previous:span[0]]...)
		out = protowire.AppendTag(out, fieldResourceSpans, protowire.BytesType)
		out = protowire.AppendBytes(out, encoded)
		previous = span[1]
	}
	return append(out, b.source[previous:]...), nil
}

// encodeJSON splices the re-encoded elements into the source where the old
// ones stood. The envelope, the whitespace and every other element stay the
// client's bytes: a re-marshalled document would compact them, reorder the
// envelope's keys and escape every `<` of a prompt.
func (b *ExportBody) encodeJSON(changed []bool) ([]byte, error) {
	spans, err := jsonElementSpans(b.source, b.key)
	if err != nil {
		return nil, fmt.Errorf("re-encode resource spans: %w", err)
	}
	var out []byte
	previous := 0
	for i, position := range b.positions {
		if i >= len(changed) || !changed[i] {
			continue
		}
		if position >= len(spans) {
			return nil, fmt.Errorf("re-encode resource spans: element %d is not in the body", position)
		}
		encoded, err := protojson.Marshal(b.ResourceSpans[i])
		if err != nil {
			return nil, fmt.Errorf("re-encode resource spans: %w", err)
		}
		converted, err := base64IDsToHex(encoded)
		if err != nil {
			return nil, fmt.Errorf("re-encode resource spans: %w", err)
		}
		span := spans[position]
		out = append(out, b.source[previous:span[0]]...)
		out = append(out, converted...)
		previous = span[1]
	}
	return append(out, b.source[previous:]...), nil
}

// jsonElementSpans finds, in a JSON export, where each element of the
// resource spans array under key sits: from its first byte to its end. A key
// written twice is read the way decoding reads it, the last one winning.
func jsonElementSpans(body []byte, key string) ([][2]int, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("the body is not a JSON object")
	}
	var spans [][2]int
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if name, _ := token.(string); name != key {
			var skip json.RawMessage
			if err := decoder.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
			return nil, fmt.Errorf("%s is not an array", key)
		}
		spans = spans[:0]
		for decoder.More() {
			var element json.RawMessage
			if err := decoder.Decode(&element); err != nil {
				return nil, err
			}
			end := int(decoder.InputOffset())
			spans = append(spans, [2]int{end - len(element), end})
		}
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
	}
	return spans, nil
}

func anyTrue(values []bool) bool {
	for _, v := range values {
		if v {
			return true
		}
	}
	return false
}
