package mapping

import (
	"fmt"
	"strings"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// Taking a trace's spans out of a stored export (spec 044 #2). An erasure
// rewrites every raw batch that holds a span of an erased trace, and a batch
// is one exporter flush that interleaves every user the process served: the
// erased spans go, and every other byte stays the client's, by the splice the
// media walk already uses (spec 041 #5).

// Without answers the body with every span of the given traces taken out. A
// ScopeSpans the removal leaves with no span goes with it, and so does a
// ResourceSpans left with none. Protobuf re-marshals only the ResourceSpans it
// changed and splices them where they stood; JSON splices the removals into
// the source text. The blocks that did not decode, the envelope's unknown
// fields and every ResourceSpans holding none of the traces keep their bytes.
// It reports how many spans it removed; with none, the body is the source.
//
// The decoded tree is changed in place into what the new body holds, so the
// caller reads what is left from ResourceSpans. The body is spent: encoding
// it again afterwards would splice into offsets that no longer exist.
func (b *ExportBody) Without(traces map[string]bool) ([]byte, int, error) {
	cuts := make([]traceCut, len(b.ResourceSpans))
	removed := 0
	for i, rs := range b.ResourceSpans {
		cuts[i] = cutTraces(rs, traces)
		removed += cuts[i].spans
	}
	if removed == 0 {
		return b.source, 0, nil
	}
	if b.asJSON {
		return b.withoutJSON(cuts, removed)
	}
	var out []byte
	previous := 0
	for i, at := range b.spans {
		cut := cuts[i]
		if cut.spans == 0 {
			continue
		}
		out = append(out, b.source[previous:at[0]]...)
		previous = at[1]
		if cut.drop {
			continue
		}
		encoded, err := proto.Marshal(b.ResourceSpans[i])
		if err != nil {
			return nil, 0, fmt.Errorf("re-encode resource spans: %w", err)
		}
		out = protowire.AppendTag(out, fieldResourceSpans, protowire.BytesType)
		out = protowire.AppendBytes(out, encoded)
	}
	out = append(out, b.source[previous:]...)
	b.dropEmptied(cuts)
	return out, removed, nil
}

func (b *ExportBody) withoutJSON(cuts []traceCut, removed int) ([]byte, int, error) {
	var edits []jsonEdit
	for i, cut := range cuts {
		if cut.spans == 0 {
			continue
		}
		prefix := []jsonStep{member(b.key), element(b.positions[i])}
		if cut.drop {
			edits = append(edits, jsonEdit{path: prefix, remove: true})
			continue
		}
		for _, path := range cut.removals {
			edits = append(edits, jsonEdit{path: within(prefix, path...), remove: true})
		}
	}
	out, err := spliceJSON(b.source, edits)
	if err != nil {
		return nil, 0, fmt.Errorf("splice the removal: %w", err)
	}
	b.dropEmptied(cuts)
	return out, removed, nil
}

// dropEmptied takes the ResourceSpans the removal emptied out of the decoded
// tree, so that it matches the body written.
func (b *ExportBody) dropEmptied(cuts []traceCut) {
	kept := b.ResourceSpans[:0]
	for i, rs := range b.ResourceSpans {
		if !cuts[i].drop {
			kept = append(kept, rs)
		}
	}
	b.ResourceSpans = kept
}

// traceCut is what the removal did to one ResourceSpans: how many spans it
// took, whether nothing is left of it, and — for the JSON splice — the path of
// each element it removed, relative to the ResourceSpans.
type traceCut struct {
	spans    int
	drop     bool
	removals [][]jsonStep
}

// cutTraces removes the spans of the traces from one ResourceSpans, in place.
// The paths are the elements' positions in the source: protojson decodes a
// repeated field element by element, so a decoded index is the index the
// client's array had.
func cutTraces(rs *tracepb.ResourceSpans, traces map[string]bool) traceCut {
	var cut traceCut
	if rs == nil {
		return cut
	}
	scopes := make([]*tracepb.ScopeSpans, 0, len(rs.ScopeSpans))
	left := 0
	for s, ss := range rs.ScopeSpans {
		if ss == nil {
			scopes = append(scopes, ss)
			continue
		}
		spans := make([]*tracepb.Span, 0, len(ss.Spans))
		var gone []int
		for k, span := range ss.Spans {
			if span != nil && traces[traceID(span.GetTraceId())] {
				gone = append(gone, k)
				continue
			}
			spans = append(spans, span)
		}
		if len(gone) == 0 {
			scopes = append(scopes, ss)
			left += len(ss.Spans)
			continue
		}
		cut.spans += len(gone)
		ss.Spans = spans
		if len(spans) == 0 {
			cut.removals = append(cut.removals, []jsonStep{stepScopeSpans, element(s)})
			continue
		}
		scopes = append(scopes, ss)
		left += len(spans)
		for _, k := range gone {
			cut.removals = append(cut.removals,
				[]jsonStep{stepScopeSpans, element(s), member("spans"), element(k)})
		}
	}
	if cut.spans == 0 {
		return cut
	}
	rs.ScopeSpans = scopes
	cut.drop = left == 0
	return cut
}

// SpanCount counts the spans a decoded export holds.
func SpanCount(resourceSpans []*tracepb.ResourceSpans) int {
	n := 0
	for _, rs := range resourceSpans {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return n
}

// HoldsTrace reports whether any span of a decoded export belongs to one of
// the traces.
func HoldsTrace(resourceSpans []*tracepb.ResourceSpans, traces map[string]bool) bool {
	for _, rs := range resourceSpans {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if traces[traceID(span.GetTraceId())] {
					return true
				}
			}
		}
	}
	return false
}

// MediaReferences lists, once each and in the order first seen, the bodies a
// decoded export points at: every reference with a stored body, which is
// what a raw batch's own refs are (spec 041 Decision 12). A placeholder —
// `"stored": false` — names no body and is not listed.
func MediaReferences(resourceSpans []*tracepb.ResourceSpans) []string {
	refs := &mediaRefs{seen: map[string]bool{}}
	rewriteMedia(resourceSpans, refs)
	return refs.out
}

// mediaRefs is a walk that rewrites nothing and remembers every reference.
type mediaRefs struct {
	seen map[string]bool
	out  []string
}

func (m *mediaRefs) enter([]string) {}

func (m *mediaRefs) whole(string) (map[string]any, bool) { return nil, false }

func (m *mediaRefs) decodes(s string) bool { return strings.Contains(s, MediaRefKey) }

func (m *mediaRefs) object(f fields) (string, bool) {
	if holder, key, _, ok := mediaSlot(f); ok {
		if slot, isObject := holder.nested(key); isObject {
			if sha, _, stored, isRef := refOf(slot); isRef {
				m.add(sha, stored)
				return "", true
			}
		}
	}
	sha, _, stored, isRef := refOf(f)
	if !isRef {
		return "", false
	}
	m.add(sha, stored)
	return "", true
}

func (m *mediaRefs) add(sha string, stored bool) {
	if stored && !m.seen[sha] {
		m.seen[sha] = true
		m.out = append(m.out, sha)
	}
}
