package mapping

import (
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
	// The JSON encoding keeps the envelope, which of the two spellings
	// held the array, the array's elements, and which element each decoded
	// ResourceSpans came from.
	envelope  map[string]json.RawMessage
	key       string
	elements  []json.RawMessage
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

func (b *ExportBody) encodeJSON(changed []bool) ([]byte, error) {
	elements := append([]json.RawMessage(nil), b.elements...)
	for i, position := range b.positions {
		if i >= len(changed) || !changed[i] {
			continue
		}
		encoded, err := protojson.Marshal(b.ResourceSpans[i])
		if err != nil {
			return nil, fmt.Errorf("re-encode resource spans: %w", err)
		}
		converted, err := base64IDsToHex(encoded)
		if err != nil {
			return nil, fmt.Errorf("re-encode resource spans: %w", err)
		}
		elements[position] = converted
	}
	array, err := json.Marshal(elements)
	if err != nil {
		return nil, err
	}
	envelope := make(map[string]json.RawMessage, len(b.envelope))
	for key, value := range b.envelope {
		envelope[key] = value
	}
	envelope[b.key] = array
	return json.Marshal(envelope)
}

func anyTrue(values []bool) bool {
	for _, v := range values {
		if v {
			return true
		}
	}
	return false
}
