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
// time — after the walk, when the value is final — or, with remove, takes an
// array element out together with the separator that joined it to the rest
// (spec 044 #2).
type jsonEdit struct {
	path   []jsonStep
	value  func() ([]byte, error)
	remove bool
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
// last occurrence. Every path must be found. A removal's path ends at an array
// element.
func spliceJSON(doc []byte, edits []jsonEdit) ([]byte, error) {
	root := &editTrie{}
	for i := range edits {
		node := root
		for _, step := range edits[i].path {
			node = node.child(step)
		}
		node.edit = &edits[i]
	}
	w := &editWalk{decoder: json.NewDecoder(bytes.NewReader(doc)), doc: doc, found: map[*jsonEdit][2]int{}}
	if err := w.walk(root); err != nil {
		return nil, err
	}
	found := w.found
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
		var value []byte
		if !s.edit.remove {
			var err error
			if value, err = s.edit.value(); err != nil {
				return nil, err
			}
		}
		out = append(append(out, doc[previous:s.start]...), value...)
		previous = s.end
	}
	return append(out, doc[previous:]...), nil
}

// editWalk is one pass over a document, finding where each edit's value sits.
type editWalk struct {
	decoder *json.Decoder
	doc     []byte
	found   map[*jsonEdit][2]int
}

func (w *editWalk) walk(node *editTrie) error {
	if node == nil || node.edit != nil {
		var raw json.RawMessage
		if err := w.decoder.Decode(&raw); err != nil {
			return err
		}
		// A removal is placed by its array, which knows the neighbours.
		if node != nil && !node.edit.remove {
			end := int(w.decoder.InputOffset())
			w.found[node.edit] = [2]int{end - len(raw), end}
		}
		return nil
	}
	token, err := w.decoder.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		for w.decoder.More() {
			key, err := w.decoder.Token()
			if err != nil {
				return err
			}
			name, _ := key.(string)
			if err := w.walk(node.members[name]); err != nil {
				return err
			}
		}
	case json.Delim('['):
		removes := node.removesElements()
		var bounds [][2]int
		for i := 0; w.decoder.More(); i++ {
			start := w.valueStart(int(w.decoder.InputOffset()))
			if err := w.walk(node.indexes[i]); err != nil {
				return err
			}
			if removes {
				bounds = append(bounds, [2]int{start, int(w.decoder.InputOffset())})
			}
		}
		if removes {
			w.placeRemovals(node, bounds)
		}
	default:
		// A scalar where the path expected a container: nothing under
		// it to find, which the count after the walk reports.
		return nil
	}
	_, err = w.decoder.Token()
	return err
}

// valueStart skips the whitespace and the comma between the end of the last
// token and the next value.
func (w *editWalk) valueStart(offset int) int {
	for offset < len(w.doc) {
		switch w.doc[offset] {
		case ' ', '\t', '\n', '\r', ',':
			offset++
		default:
			return offset
		}
	}
	return offset
}

// placeRemovals decides the bytes each removed element takes with it, so that
// what is left is still an array: an element followed by one that stays goes
// with the separator after it, up to where that one starts; an element with
// nothing staying after it goes with the separator before it, from where the
// element before it ended. The ranges never overlap, and an array emptied
// whole keeps its brackets and the whitespace inside them.
func (w *editWalk) placeRemovals(node *editTrie, bounds [][2]int) {
	removed := func(i int) bool {
		child := node.indexes[i]
		return child != nil && child.edit != nil && child.edit.remove
	}
	last := -1
	for i := range bounds {
		if !removed(i) {
			last = i
		}
	}
	for i := range bounds {
		if !removed(i) {
			continue
		}
		at := [2]int{bounds[i][0], bounds[i][1]}
		switch {
		case i < last:
			at[1] = bounds[i+1][0]
		case i > 0:
			at[0] = bounds[i-1][1]
		}
		w.found[node.indexes[i].edit] = at
	}
}

// removesElements reports whether an edit removes one of this array's
// elements.
func (t *editTrie) removesElements() bool {
	for _, child := range t.indexes {
		if child.edit != nil && child.edit.remove {
			return true
		}
	}
	return false
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
