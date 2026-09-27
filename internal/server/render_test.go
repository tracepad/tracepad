package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

// nestedObject renders an object the way it was rendered before spec 043 #20:
// each field through encoding/json, each nested object by a MarshalJSON of its
// own — the reference whose bytes the one-buffer rendering must keep.
type nestedObject object

func (o nestedObject) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buffer.WriteByte(',')
		}
		key, _ := json.Marshal(m.key)
		buffer.Write(key)
		buffer.WriteByte(':')
		value, err := json.Marshal(nested(m.value))
		if err != nil {
			return nil, err
		}
		buffer.Write(value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// nested swaps every object inside a value for its nestedObject.
func nested(value any) any {
	switch v := value.(type) {
	case object:
		return nestedObject(v)
	case []object:
		if v == nil {
			return []nestedObject(nil)
		}
		out := make([]nestedObject, len(v))
		for i, o := range v {
			out[i] = nestedObject(o)
		}
		return out
	}
	return value
}

// Rendering in one buffer writes the bytes rendering level by level wrote:
// nested objects and lists of them, empty and nil lists, HTML-significant and
// non-ASCII text, maps, markers and numbers (spec 043 #20).
func TestOneBufferRenderingKeepsTheBytes(t *testing.T) {
	cost := 0.25
	leafs := object{}.
		put("text", `a <b> & "c" — ü`).
		put("number", 1.5e-7).
		put("pointer", &cost).
		put("none", nil).
		put("map", map[string]any{"z": 1, "a": []any{"<", map[string]any{"&": true}}}).
		put("marker", truncation{Truncated: true, Size: 9, TraceID: "t", ObservationID: "o", Full: "/x?a=1&b=2"}).
		put("empty", object{}).
		put("none_list", []object(nil)).
		put("empty_list", []object{}).
		put("raw", json.RawMessage(`{"x": "<y>"}`))
	body := leafs
	for depth := range 300 {
		body = object{}.put("level", depth).put("leafs", leafs).put("children", []object{body, leafs})
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(nestedObject(body))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("one-buffer rendering changed the bytes:\n got: %.400s\nwant: %.400s", got, want)
	}
}
