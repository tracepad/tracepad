package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Rendering for the read API (spec 004). Rows are built as ordered key/value
// lists rather than as structs for one reason: `?fields=` selects top-level
// fields of a row, and a struct can only express that by zeroing fields, which
// makes "not selected" indistinguishable from "empty". An ordered list drops
// what was not asked for and keeps what was in the order the row declares it,
// so two responses of the same endpoint always read the same way.

// member is one key/value pair of a rendered object.
type member struct {
	key   string
	value any
}

// object marshals its members in order.
type object []member

// put appends a member. Absent values are dropped by the callers that know
// what absent means for their field; put itself stores whatever it is given.
func (o object) put(key string, value any) object {
	return append(o, member{key: key, value: value})
}

// putSome appends a member only when there is something to say. An empty
// string, an empty collection and a nil pointer all mean "this trace never
// carried the field", which is not the same as carrying an empty one, and a
// key that is always present but usually empty costs every consumer its
// budget for nothing (spec 004 #2).
func (o object) putSome(key string, value any) object {
	switch v := value.(type) {
	case string:
		if v == "" {
			return o
		}
	case []string:
		if len(v) == 0 {
			return o
		}
	case map[string]any:
		if len(v) == 0 {
			return o
		}
	case *float64:
		if v == nil {
			return o
		}
	case *int64:
		if v == nil {
			return o
		}
	case nil:
		return o
	}
	return append(o, member{key: key, value: value})
}

func (o object) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buffer.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		buffer.Write(key)
		buffer.WriteByte(':')
		value, err := json.Marshal(m.value)
		if err != nil {
			return nil, fmt.Errorf("render %q: %w", m.key, err)
		}
		buffer.Write(value)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// keys returns the member names, which is what an unknown-field error needs to
// tell the caller what it could have asked for.
func (o object) keys() []string {
	out := make([]string, 0, len(o))
	for _, m := range o {
		out = append(out, m.key)
	}
	return out
}

// selection is a parsed `?fields=` list; nil selects everything.
type selection []string

// parseSelection reads `?fields=a,b,c`. The known list is the full row shape,
// not the fields this particular row happens to carry: asking for a field a
// row does not have is a legitimate request that answers with the field
// missing, while asking for a field that does not exist is a typo, and a typo
// that silently narrows a response is exactly what spec 003 #21 refuses.
func parseSelection(raw string, known []string) (selection, error) {
	if raw == "" {
		return nil, nil
	}
	var out selection
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, fmt.Errorf("fields contains an empty name (accepted: %s)",
				strings.Join(known, ", "))
		}
		if !slices.Contains(known, field) {
			return nil, fmt.Errorf("unknown field %q in fields (accepted: %s)",
				field, strings.Join(known, ", "))
		}
		if !slices.Contains(out, field) {
			out = append(out, field)
		}
	}
	return out, nil
}

// apply keeps the selected members, in the row's own order rather than the
// order they were asked for: the shape of a row is the API's to fix.
func (s selection) apply(o object) object {
	if s == nil {
		return o
	}
	out := make(object, 0, len(s))
	for _, m := range o {
		if slices.Contains(s, m.key) {
			out = append(out, m)
		}
	}
	return out
}
