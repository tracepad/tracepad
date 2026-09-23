package mapping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// The splice behind "nothing more than the media changes" (spec 041 #5,
// Decision 19): a walk records the path of every value it replaced, and the
// document is rewritten by replacing the bytes of those values and no others.
// A re-marshalled document would drop what the decoder does not know, sort
// the keys of every object, compact the whitespace and re-escape the strings;
// the client's bytes are none of those things.

// jsonStep is one step of a path: an object member, under its name or the
// other spelling the decoder also accepts (protojson reads `scopeSpans` and
// `scope_spans` alike), or an array element.
type jsonStep struct {
	key, alt string
	index    int
}

func member(key string) jsonStep { return jsonStep{key: key} }

func memberOr(key, alt string) jsonStep { return jsonStep{key: key, alt: alt} }

func element(i int) jsonStep { return jsonStep{index: i} }

// jsonEdit replaces the value at a path with an encoding computed at splice
// time — after the walk, when the value is final.
type jsonEdit struct {
	path  []jsonStep
	value func() ([]byte, error)
}

// within prefixes a path, copying it so that sibling paths never share a
// backing array.
func within(path []jsonStep, steps ...jsonStep) []jsonStep {
	out := make([]jsonStep, 0, len(path)+len(steps))
	return append(append(out, path...), steps...)
}

// editSink collects the edits of one document. A nil sink records nothing:
// a caller that reads the rewritten tree has no document to splice.
type editSink struct{ edits []jsonEdit }

func (s *editSink) add(path []jsonStep, value func() ([]byte, error)) {
	if s != nil {
		s.edits = append(s.edits, jsonEdit{path: path, value: value})
	}
}

// editTrie is the edits arranged by their paths, so that one pass over the
// document finds all of them.
type editTrie struct {
	edit    *jsonEdit
	members map[string]*editTrie
	indexes map[int]*editTrie
}

func (t *editTrie) child(step jsonStep) *editTrie {
	if step.key == "" {
		if t.indexes == nil {
			t.indexes = map[int]*editTrie{}
		}
		if t.indexes[step.index] == nil {
			t.indexes[step.index] = &editTrie{}
		}
		return t.indexes[step.index]
	}
	if t.members == nil {
		t.members = map[string]*editTrie{}
	}
	next := t.members[step.key]
	if next == nil {
		next = &editTrie{}
		t.members[step.key] = next
	}
	if step.alt != "" {
		t.members[step.alt] = next
	}
	return next
}

// spliceJSON replaces the value at each edit's path and keeps every other
// byte. A member written twice is spliced where the decoder reads it, at its
// last occurrence. Every path must be found.
func spliceJSON(doc []byte, edits []jsonEdit) ([]byte, error) {
	root := &editTrie{}
	for i := range edits {
		node := root
		for _, step := range edits[i].path {
			node = node.child(step)
		}
		node.edit = &edits[i]
	}
	found := map[*jsonEdit][2]int{}
	decoder := json.NewDecoder(bytes.NewReader(doc))
	if err := walkEdits(decoder, root, found); err != nil {
		return nil, err
	}
	if len(found) != len(edits) {
		return nil, fmt.Errorf("splice: %d of %d rewritten values are not in the document", len(edits)-len(found), len(edits))
	}
	type span struct {
		start, end int
		edit       *jsonEdit
	}
	spans := make([]span, 0, len(found))
	for edit, at := range found {
		spans = append(spans, span{at[0], at[1], edit})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := make([]byte, 0, len(doc))
	previous := 0
	for _, s := range spans {
		value, err := s.edit.value()
		if err != nil {
			return nil, err
		}
		out = append(append(out, doc[previous:s.start]...), value...)
		previous = s.end
	}
	return append(out, doc[previous:]...), nil
}

func walkEdits(decoder *json.Decoder, node *editTrie, found map[*jsonEdit][2]int) error {
	if node == nil || node.edit != nil {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if node != nil {
			end := int(decoder.InputOffset())
			found[node.edit] = [2]int{end - len(raw), end}
		}
		return nil
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			if err := walkEdits(decoder, node.members[name], found); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for i := 0; decoder.More(); i++ {
			if err := walkEdits(decoder, node.indexes[i], found); err != nil {
				return err
			}
		}
	default:
		// A scalar where the path expected a container: nothing under
		// it to find, which the count after the walk reports.
		return nil
	}
	_, err = decoder.Token()
	return err
}

// encodedJSON is an edit's value for a decoded document: written back
// without the HTML escaping `json.Marshal` applies.
func encodedJSON(v any) func() ([]byte, error) {
	return func() ([]byte, error) {
		s, ok := encodeDocument(v)
		if !ok {
			return nil, fmt.Errorf("splice: cannot encode a rewritten value")
		}
		return []byte(s), nil
	}
}
