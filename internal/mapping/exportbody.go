package mapping

import (
	"fmt"
	"slices"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
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

// JSON reports whether the body is in the OTLP/JSON encoding.
func (b *ExportBody) JSON() bool { return b.asJSON }

// Encode writes the body back with the rewritten ResourceSpans in place.
// Nothing rewritten is the source itself.
func (b *ExportBody) Encode(rewrites Rewrites) ([]byte, error) {
	changed := rewrites.Changed()
	if !slices.Contains(changed, true) {
		return b.source, nil
	}
	if b.asJSON {
		return b.encodeJSON(rewrites)
	}
	// Protobuf keeps what it does not know: a decoded ResourceSpans carries
	// its unknown fields, and marshals them back.
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

// encodeJSON splices the rewritten values into the source where the old ones
// stood, and nothing else. JSON decoding drops the fields it does not know,
// so a re-encoded element would lose them; spliced, the envelope, the
// whitespace, the unknown fields and every value the walk did not touch stay
// the client's bytes.
func (b *ExportBody) encodeJSON(rewrites Rewrites) ([]byte, error) {
	var edits []jsonEdit
	for i, position := range b.positions {
		if i >= len(rewrites) {
			break
		}
		prefix := []jsonStep{member(b.key), element(position)}
		for _, edit := range rewrites[i] {
			edits = append(edits, jsonEdit{path: within(prefix, edit.path...), value: edit.value})
		}
	}
	out, err := spliceJSON(b.source, edits)
	if err != nil {
		return nil, fmt.Errorf("splice resource spans: %w", err)
	}
	return out, nil
}
