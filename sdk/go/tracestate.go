package tracepad

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// What UpdateTrace has said on a span so far (spec 033 #22).
//
// OpenTelemetry keeps one value per attribute, so a second call that wrote
// tracepad.trace.tags or tracepad.trace.metadata again replaced the first
// call's. The package remembers what it has written on each span, in memory and
// under a lock, merges each call into that, and writes both attributes whole:
// tags in the order first seen, metadata by top-level key with the later value
// winning. The wire is what it always was — a JSON array and a JSON object.
//
// Only what this package wrote is merged: a span's attributes are not part of
// the OpenTelemetry API, so tags another writer set on the same span are
// replaced by the first call. A span's state is dropped when the span ends (the
// processor Init registers), and the table is bounded for a provider without
// one.

// The server's own bounds (spec 043 #14, spec 002 #32): beyond them it drops
// what it was sent, so the package stops sending it.
const (
	maxTraceTags     = 50
	maxTraceKeys     = 512
	maxTraceBytes    = 1 << 20
	maxTrackedSpans  = 10_000
	traceStateWarned = "trace metadata is bounded; what did not fit was dropped"
)

type spanKey struct {
	trace trace.TraceID
	span  trace.SpanID
}

type traceState struct {
	mu       sync.Mutex
	tags     []string
	metadata map[string]any
}

var traceStates = struct {
	mu     sync.Mutex
	byspan map[spanKey]*traceState
	order  []spanKey
	warned bool
}{byspan: map[spanKey]*traceState{}}

// stateOf is the state of a span, made on first use.
func stateOf(sc trace.SpanContext) *traceState {
	key := spanKey{sc.TraceID(), sc.SpanID()}
	traceStates.mu.Lock()
	defer traceStates.mu.Unlock()
	if state, ok := traceStates.byspan[key]; ok {
		return state
	}
	if len(traceStates.order) >= maxTrackedSpans {
		delete(traceStates.byspan, traceStates.order[0])
		traceStates.order = traceStates.order[1:]
	}
	state := &traceState{metadata: map[string]any{}}
	traceStates.byspan[key] = state
	traceStates.order = append(traceStates.order, key)
	return state
}

// forgetTraceState drops a span's state when the span ends.
func forgetTraceState(sc trace.SpanContext) {
	key := spanKey{sc.TraceID(), sc.SpanID()}
	traceStates.mu.Lock()
	defer traceStates.mu.Unlock()
	if _, ok := traceStates.byspan[key]; ok {
		delete(traceStates.byspan, key)
		if i := slices.Index(traceStates.order, key); i >= 0 {
			traceStates.order = slices.Delete(traceStates.order, i, i+1)
		}
	}
}

// warnTraceBounds says once per process that a bound bit.
func warnTraceBounds() {
	traceStates.mu.Lock()
	first := !traceStates.warned
	traceStates.warned = true
	traceStates.mu.Unlock()
	if first {
		def.log().Warn("tracepad: " + traceStateWarned)
	}
}

// writeTrace merges a call's tags and metadata into the span's state and sets
// both attributes whole. The set happens under the state's lock: two calls on
// one span cannot then leave the attribute of the one that merged first.
func writeTrace(span trace.Span, f *traceFields) {
	state := stateOf(span.SpanContext())
	state.mu.Lock()
	defer state.mu.Unlock()
	var attrs []attribute.KeyValue
	if f.hasTags {
		for _, tag := range f.tags {
			if !slices.Contains(state.tags, tag) && len(state.tags) < maxTraceTags {
				state.tags = append(state.tags, tag)
			}
		}
		attrs = append(attrs, attribute.String(attrTraceTags, dumps(append([]string{}, state.tags...))))
	}
	if f.hasMetadata {
		if encoded, ok := state.mergeMetadata(f.metadata); ok {
			attrs = append(attrs, attribute.String(attrTraceMetadata, encoded))
		}
	}
	span.SetAttributes(attrs...)
}

// mergeMetadata adds a call's keys, replacing the ones it names, within the
// bounds; the object to write, encoded once.
func (s *traceState) mergeMetadata(metadata any) (string, bool) {
	added, ok := objectOf(metadata)
	if !ok {
		def.log().Warn("tracepad: WithTraceMetadata takes a JSON object; this one was ignored")
		return "", false
	}
	fits := func(candidate map[string]any, key string) bool {
		_, held := s.metadata[key]
		return held || len(candidate) < maxTraceKeys
	}
	candidate := maps.Clone(s.metadata)
	for _, key := range slices.Sorted(maps.Keys(added)) {
		if fits(candidate, key) {
			candidate[key] = added[key]
		} else {
			warnTraceBounds()
		}
	}
	encoded := dumps(candidate)
	if len(encoded) > maxTraceBytes {
		candidate = maps.Clone(s.metadata)
		for _, key := range slices.Sorted(maps.Keys(added)) {
			trial := maps.Clone(candidate)
			trial[key] = added[key]
			if fits(candidate, key) && len(dumps(trial)) <= maxTraceBytes {
				candidate = trial
			} else {
				warnTraceBounds()
			}
		}
		encoded = dumps(candidate)
	}
	s.metadata = candidate
	return encoded, true
}

// objectOf reads metadata as a JSON object: a map with string keys as it is,
// anything else as it encodes, numbers as they were spelled so that an int64
// past 2^53 is not rounded through a float64.
func objectOf(metadata any) (map[string]any, bool) {
	v := reflect.ValueOf(metadata)
	_, marshals := metadata.(json.Marshaler)
	if v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String && !marshals {
		out := make(map[string]any, v.Len())
		for _, key := range v.MapKeys() {
			out[key.String()] = v.MapIndex(key).Interface()
		}
		return out, true
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader([]byte(dumps(metadata))))
	decoder.UseNumber()
	return out, decoder.Decode(&out) == nil && out != nil
}
