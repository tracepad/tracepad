package tracepad

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// What UpdateTrace has said on a span so far (spec 033 #22).
//
// OpenTelemetry keeps one value per attribute, so a second call that wrote
// tracepad.trace.tags or tracepad.trace.metadata again replaced the first
// call's. The package remembers what it has written on each span, in memory,
// and merges each call into that: tags in the order first seen, metadata by
// top-level key with the later value winning, both written whole — the wire is
// a JSON array and a JSON object, as it always was.
//
// What is kept is JSON text, taken when the call is made: a value the
// application changes afterwards is not changed in the trace, and a document is
// put together by joining what is already encoded. Each span has a lock of its
// own, held across the merge and the write of the attributes, so that two calls
// cannot leave the attribute of the one that merged first.
//
// Only what this package wrote is merged: a span's attributes are not part of
// the OpenTelemetry API, so tags another writer set on the same span are
// replaced by the first call. A span's state is dropped when the span ends (the
// processor Init registers), and the table is bounded for a provider without
// one.

// The server's own bounds (spec 043 #14, spec 002 #32): beyond them it drops
// what it was sent, so the package stops sending it.
const (
	maxTraceTags    = 50
	maxTraceKeys    = 512
	maxTraceBytes   = 1 << 20
	maxTrackedSpans = 10_000
)

type spanKey struct {
	trace trace.TraceID
	span  trace.SpanID
}

// encoded is a value as JSON text, and its size.
type encoded struct {
	text string
	size int
}

type traceState struct {
	mu       sync.Mutex
	tags     []string
	metadata map[string]encoded // by the JSON text of the key
	size     int                // bytes of the object these make: braces, entries, commas
	element  *list.Element      // where the table keeps it, to drop it in one step
}

var traceStates = struct {
	mu     sync.Mutex
	byspan map[spanKey]*traceState
	order  *list.List // spanKeys, oldest first
	warned map[string]bool
}{byspan: map[spanKey]*traceState{}, order: list.New(), warned: map[string]bool{}}

// resetTraceState forgets what was warned about and every span's state
// (spec 040).
func resetTraceState() {
	traceStates.mu.Lock()
	defer traceStates.mu.Unlock()
	clear(traceStates.byspan)
	clear(traceStates.warned)
	traceStates.order.Init()
}

// stateOf is the state of a span, made on first use.
func stateOf(sc trace.SpanContext) *traceState {
	key := spanKey{sc.TraceID(), sc.SpanID()}
	traceStates.mu.Lock()
	defer traceStates.mu.Unlock()
	if state, ok := traceStates.byspan[key]; ok {
		return state
	}
	if traceStates.order.Len() >= maxTrackedSpans {
		oldest := traceStates.order.Front()
		delete(traceStates.byspan, oldest.Value.(spanKey))
		traceStates.order.Remove(oldest)
	}
	state := &traceState{metadata: map[string]encoded{}, size: 2}
	state.element = traceStates.order.PushBack(key)
	traceStates.byspan[key] = state
	return state
}

// forgetTraceState drops a span's state when the span ends.
func forgetTraceState(sc trace.SpanContext) {
	key := spanKey{sc.TraceID(), sc.SpanID()}
	traceStates.mu.Lock()
	defer traceStates.mu.Unlock()
	if state, ok := traceStates.byspan[key]; ok {
		traceStates.order.Remove(state.element)
		delete(traceStates.byspan, key)
	}
}

// warnTrace says once per process for each kind that a bound bit, or that a
// call was ignored.
func warnTrace(kind, message string) {
	traceStates.mu.Lock()
	first := !traceStates.warned[kind]
	traceStates.warned[kind] = true
	traceStates.mu.Unlock()
	if first {
		def.log().Warn("tracepad: " + message)
	}
}

// jsonText is a value as JSON, a string too — a part of a larger document,
// which dumps is not.
func jsonText(value any) string {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		out.Reset()
		_ = enc.Encode(fmt.Sprintf("<%T: %v>", value, err))
	}
	return strings.TrimRight(out.String(), "\n")
}

// writeTrace merges a call's tags and metadata into the span's state and sets
// both attributes whole. The set happens under the state's lock.
func writeTrace(span trace.Span, f *traceFields) {
	// Encoded before the lock: a value is read once, as the call is made.
	var tags []string
	if f.hasTags {
		tags = slices.Clone(f.tags)
	}
	var added map[string]encoded
	if f.hasMetadata {
		entries, ok := objectOf(f.metadata)
		if !ok {
			warnTrace("shape", "WithTraceMetadata takes a JSON object; this one was ignored")
		}
		added = make(map[string]encoded, len(entries))
		for key, value := range entries {
			text := jsonText(value)
			added[jsonText(key)] = encoded{text, len(text)}
		}
		if !ok {
			added = nil
			f.hasMetadata = false
		}
	}

	state := stateOf(span.SpanContext())
	state.mu.Lock()
	defer state.mu.Unlock()
	var attrs []attribute.KeyValue
	if f.hasTags {
		for _, tag := range tags {
			switch {
			case slices.Contains(state.tags, tag):
			case len(state.tags) < maxTraceTags:
				state.tags = append(state.tags, tag)
			default:
				warnTrace("tags", fmt.Sprintf("trace tags are bounded at %d; the tags past them were dropped", maxTraceTags))
			}
		}
		attrs = append(attrs, attribute.String(attrTraceTags, jsonText(append([]string{}, state.tags...))))
	}
	if f.hasMetadata {
		state.merge(added)
		attrs = append(attrs, attribute.String(attrTraceMetadata, state.object()))
	}
	span.SetAttributes(attrs...)
}

// merge adds a call's keys, replacing the ones it names, within the bounds.
func (s *traceState) merge(added map[string]encoded) {
	for _, key := range slices.Sorted(maps.Keys(added)) {
		value := added[key]
		var total int
		if held, ok := s.metadata[key]; ok {
			total = s.size - held.size + value.size
		} else if len(s.metadata) >= maxTraceKeys {
			warnTrace("keys", fmt.Sprintf("trace metadata is bounded at %d keys; the new keys past it were dropped", maxTraceKeys))
			continue
		} else {
			total = s.size + len(key) + 1 + value.size
			if len(s.metadata) > 0 {
				total++
			}
		}
		if total > maxTraceBytes {
			warnTrace("bytes", fmt.Sprintf("trace metadata is bounded at %d bytes; the keys past it were dropped", maxTraceBytes))
			continue
		}
		s.metadata[key] = value
		s.size = total
	}
}

// object is the metadata as the JSON object it is written as, keys in order.
func (s *traceState) object() string {
	var out strings.Builder
	out.WriteByte('{')
	for i, key := range slices.Sorted(maps.Keys(s.metadata)) {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(key + ":" + s.metadata[key].text)
	}
	out.WriteByte('}')
	return out.String()
}

// objectOf reads metadata as a JSON object: a map with string keys as it is,
// anything else as it encodes, numbers as they were spelled so that an int64
// past 2^53 is not rounded through a float64. A string, a number or an array
// is not an object, whatever text it holds.
func objectOf(metadata any) (map[string]any, bool) {
	v := reflect.ValueOf(metadata)
	if v.Kind() == reflect.String {
		return nil, false
	}
	if _, marshals := metadata.(json.Marshaler); v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String && !marshals {
		out := make(map[string]any, v.Len())
		for _, key := range v.MapKeys() {
			out[key.String()] = v.MapIndex(key).Interface()
		}
		return out, true
	}
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(dumps(metadata)))
	decoder.UseNumber()
	return out, decoder.Decode(&out) == nil && out != nil
}
