package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/model"

	"github.com/tracepad/tracepad/internal/logpace"
)

// Rendering for the read API (spec 004). Rows are built as ordered key/value
// lists rather than as structs for one reason: `?fields=` selects top-level
// fields of a row, and a struct can only express that by zeroing fields, which
// makes "not selected" indistinguishable from "empty". An ordered list drops
// what was not asked for and keeps what was in the order the row declares it,
// so two responses of the same endpoint always read the same way.

// field is one key/value pair of a rendered object. Named `field` rather than
// `member` since spec 028, where a member is a person with a role in a
// project and the word had to mean one thing.
type field struct {
	key   string
	value any
}

// object marshals its fields in order.
type object []field

// put appends a field. Absent values are dropped by the callers that know
// what absent means for their field; put itself stores whatever it is given.
func (o object) put(key string, value any) object {
	return append(o, field{key: key, value: value})
}

// putSome appends a field only when there is something to say. An empty
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
	return append(o, field{key: key, value: value})
}

// MarshalJSON renders the object in one pass over one buffer (spec 043 #20).
// A nested object or list of objects is written in place rather than
// marshalled on its own: the standard encoder re-validates whatever a
// MarshalJSON returns, so marshalling each level separately re-scanned
// everything beneath it, and a deep tree cost the square of its depth. Leaves
// still go through encoding/json, so the bytes are what they always were.
func (o object) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	if err := o.appendJSON(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// appender is a value that writes its own JSON into the response's buffer: a
// rendered object, or anything built of them.
type appender interface {
	appendJSON(buffer *bytes.Buffer) error
}

func (o object) appendJSON(buffer *bytes.Buffer) error {
	buffer.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			buffer.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return err
		}
		buffer.Write(key)
		buffer.WriteByte(':')
		if !finite(m.value) {
			// The backstop for a number nothing upstream kept finite
			// — an aggregate stored before the counting rule, a mean
			// of extreme scores (spec 043 #7). JSON cannot spell an
			// infinity, and `null` is this API's word for "no number"
			// (spec 002 #14), which is the truth about one.
			if skipped, ok := nonFiniteLog.Allow(m.key, time.Now()); ok {
				slog.Warn("rendered a non-finite number as null",
					"field", m.key, "since_last_line", skipped.SameKey)
			}
			buffer.WriteString("null")
			continue
		}
		if err := appendValue(buffer, m.value); err != nil {
			return fmt.Errorf("render %q: %w", m.key, err)
		}
	}
	buffer.WriteByte('}')
	return nil
}

// appendValue writes one field's value: an object or a list of them in place,
// anything else through encoding/json.
func appendValue(buffer *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case appender:
		return v.appendJSON(buffer)
	case []object:
		if v == nil {
			// What encoding/json writes for a nil slice.
			buffer.WriteString("null")
			return nil
		}
		buffer.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if err := item.appendJSON(buffer); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	buffer.Write(encoded)
	return nil
}

// nonFiniteLog paces the warning for a non-finite number rendered as null,
// one line a minute per field.
var nonFiniteLog = &logpace.Keyed{Every: time.Minute}

// finite reports whether a field's value is anything but a NaN or an infinity
// in a float64 or a *float64 — the two shapes a rendered number takes.
func finite(value any) bool {
	switch v := value.(type) {
	case float64:
		return model.Finite(v)
	case *float64:
		return v == nil || model.Finite(*v)
	}
	return true
}

// keys returns the field names, which is what an unknown-field error needs to
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

// wants reports whether a field is in the selection — which every field is when
// there is none. Asked before a field is *computed*, for the one field that
// costs a query to produce (spec 011 #6).
func (s selection) wants(field string) bool {
	return s == nil || slices.Contains(s, field)
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
