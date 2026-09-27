package mapping_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/tracepad/tracepad/internal/mapping"
)

// Taking an erased trace's spans out of a stored export (spec 044 #2).

const (
	erasedA = "a0000000000000000000000000000001" // user A's trace
	keptB   = "b0000000000000000000000000000002" // user B's
	keptC   = "c0000000000000000000000000000003" // user B's too
)

func scrubSpan(t *testing.T, trace, id, name string, attrs ...*commonpb.KeyValue) *tracepb.Span {
	t.Helper()
	traceID, err := hex.DecodeString(trace)
	if err != nil {
		t.Fatal(err)
	}
	spanID, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	return &tracepb.Span{TraceId: traceID, SpanId: spanID, Name: name,
		StartTimeUnixNano: 1, EndTimeUnixNano: 2, Attributes: attrs}
}

func scrubAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

// scrubExport is three ResourceSpans over three traces of two users:
//
//	0: scope "mixed" [A a1, B b1], scope "only-a" [A a2]
//	1: scope "c"     [C c1]
//	2: scope "a"     [A a3]
//
// Erasing A leaves 0 with its first scope holding b1, 1 as it was, and 2 gone.
func scrubExport(t *testing.T) []*tracepb.ResourceSpans {
	t.Helper()
	b1 := scrubSpan(t, keptB, "00000000000000b1", "b1", scrubAttr("in", "user b says hi"))
	// A field this decoder does not know, on a span that stays: a
	// re-marshalled ResourceSpans keeps it.
	b1.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 7))
	resource := func(name string) *resourcepb.Resource {
		return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{scrubAttr("service.name", name)}}
	}
	return []*tracepb.ResourceSpans{
		{Resource: resource("svc-0"), ScopeSpans: []*tracepb.ScopeSpans{
			{Scope: &commonpb.InstrumentationScope{Name: "mixed"}, Spans: []*tracepb.Span{
				scrubSpan(t, erasedA, "00000000000000a1", "a1", scrubAttr("in", "user a secret one")),
				b1,
			}},
			{Scope: &commonpb.InstrumentationScope{Name: "only-a"}, Spans: []*tracepb.Span{
				scrubSpan(t, erasedA, "00000000000000a2", "a2", scrubAttr("in", "user a secret two")),
			}},
		}},
		{Resource: resource("svc-1"), ScopeSpans: []*tracepb.ScopeSpans{
			{Scope: &commonpb.InstrumentationScope{Name: "c"}, Spans: []*tracepb.Span{
				scrubSpan(t, keptC, "00000000000000c1", "c1", scrubAttr("in", "user b again")),
			}},
		}},
		{Resource: resource("svc-2"), ScopeSpans: []*tracepb.ScopeSpans{
			{Scope: &commonpb.InstrumentationScope{Name: "a"}, Spans: []*tracepb.Span{
				scrubSpan(t, erasedA, "00000000000000a3", "a3", scrubAttr("in", "user a secret three")),
			}},
		}},
	}
}

// spanNames lists what a body holds, as scope/span pairs, in order.
func spanNames(export []*tracepb.ResourceSpans) []string {
	var out []string
	for _, rs := range export {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				out = append(out, ss.GetScope().GetName()+"/"+span.GetName())
			}
		}
	}
	return out
}

func TestWithoutTakesTheErasedSpansOut(t *testing.T) {
	erased := map[string]bool{erasedA: true}
	want := []string{"mixed/b1", "c/c1"}

	t.Run("protobuf", func(t *testing.T) {
		export := scrubExport(t)
		encoded, err := mapping.EncodeExportRequest(export)
		if err != nil {
			t.Fatal(err)
		}
		// Between the second and the third block: one that does not
		// decode (field 1, length 3, junk). After the last: a field of
		// the envelope this decoder does not know.
		first, second := len(mustMarshal(t, export[0])), len(mustMarshal(t, export[1]))
		cut := plainLenField(first) + plainLenField(second)
		junk := []byte{0x0a, 0x03, 0xff, 0xff, 0xff}
		unknown := protowire.AppendVarint(protowire.AppendTag(nil, 7, protowire.VarintType), 42)
		source := slices.Concat(encoded[:cut], junk, encoded[cut:], unknown)
		untouched := encoded[plainLenField(first):cut]

		decoded, err := mapping.DecodeExportBody(source, false)
		if err != nil {
			t.Fatal(err)
		}
		out, removed, err := decoded.Without(erased)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 3 {
			t.Errorf("removed %d spans, want 3", removed)
		}
		if got := spanNames(decoded.ResourceSpans); !slices.Equal(got, want) {
			t.Errorf("the tree holds %v, want %v", got, want)
		}
		again, err := mapping.DecodeExportBody(out, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := spanNames(again.ResourceSpans); !slices.Equal(got, want) {
			t.Errorf("the body decodes to %v, want %v", got, want)
		}
		if len(again.ResourceSpans) != 2 || len(again.ResourceSpans[0].ScopeSpans) != 1 {
			t.Errorf("the emptied scope or resource is still there: %d resources, %d scopes in the first",
				len(again.ResourceSpans), len(again.ResourceSpans[0].ScopeSpans))
		}
		if again.Unreadable != 1 || !bytes.Contains(out, junk) {
			t.Error("the block that did not decode was not kept as it was")
		}
		if !bytes.Contains(out, untouched) {
			t.Error("the untouched ResourceSpans did not keep its bytes")
		}
		if !bytes.HasSuffix(out, unknown) {
			t.Error("the envelope's unknown field was lost")
		}
		if len(again.ResourceSpans[0].ScopeSpans[0].Spans[0].ProtoReflect().GetUnknown()) == 0 {
			t.Error("the kept span lost the field the decoder does not know")
		}
		if bytes.Contains(out, []byte("user a")) {
			t.Error("the erased user's text is still in the body")
		}
	})

	t.Run("json", func(t *testing.T) {
		export := scrubExport(t)
		encoded, err := mapping.EncodeExportRequestJSON(export)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(envelope["resourceSpans"], &elements); err != nil {
			t.Fatal(err)
		}
		// The client's own spelling: indentation, a key the decoder does
		// not know on a kept span, markup, a block that does not decode.
		first := strings.Replace(string(elements[0]), `"name":"b1"`,
			`"name":"b1", "futureField": {"x": true}`, 1)
		untouched := strings.Replace(string(elements[1]), `user b again`, `<b>user b</b> & again`, 1)
		head := "{\n  \"zz\": 1,\n  \"resourceSpans\": [\n    "
		source := []byte(head + first + ",\n    " + untouched + ",\n    " +
			`{"scopeSpans": 7}` + ",\n    " + string(elements[2]) + "\n  ]\n}\n")

		decoded, err := mapping.DecodeExportBody(source, true)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Unreadable != 1 {
			t.Fatalf("the junk block decoded: %d unreadable", decoded.Unreadable)
		}
		out, removed, err := decoded.Without(erased)
		if err != nil {
			t.Fatal(err)
		}
		if removed != 3 {
			t.Errorf("removed %d spans, want 3", removed)
		}
		again, err := mapping.DecodeExportBody(out, true)
		if err != nil {
			t.Fatalf("the spliced body does not decode: %v\n%s", err, out)
		}
		if got := spanNames(again.ResourceSpans); !slices.Equal(got, want) {
			t.Errorf("the body decodes to %v, want %v", got, want)
		}
		for _, kept := range []string{head, untouched, `{"scopeSpans": 7}`, `"futureField": {"x": true}`} {
			if !bytes.Contains(out, []byte(kept)) {
				t.Errorf("the body lost the client's %q:\n%s", kept, out)
			}
		}
		if bytes.Contains(out, []byte("user a")) || bytes.Contains(out, []byte("only-a")) {
			t.Errorf("the erased user's spans or their emptied scope are still there:\n%s", out)
		}
		if again.Unreadable != 1 {
			t.Error("the block that did not decode was not kept")
		}
	})
}

// A removal leaves a valid array wherever the removed elements sit: first,
// middle, last, a run at the end, every one.
func TestWithoutKeepsTheArrayValid(t *testing.T) {
	names := func(traces ...string) []*tracepb.ResourceSpans {
		var spans []*tracepb.Span
		for i, trace := range traces {
			spans = append(spans, scrubSpan(t, trace, "000000000000000"+string(rune('1'+i)), "s"+string(rune('1'+i))))
		}
		return []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}
	}
	for _, c := range []struct {
		name   string
		traces []string
		left   int
	}{
		{"first", []string{erasedA, keptB, keptC}, 2},
		{"middle", []string{keptB, erasedA, keptC}, 2},
		{"last", []string{keptB, keptC, erasedA}, 2},
		{"a run at the end", []string{keptB, erasedA, erasedA}, 1},
		{"a run at the start", []string{erasedA, erasedA, keptB}, 1},
		{"every other", []string{erasedA, keptB, erasedA, keptC, erasedA}, 2},
		{"all", []string{erasedA, erasedA}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			source, err := mapping.EncodeExportRequestJSON(names(c.traces...))
			if err != nil {
				t.Fatal(err)
			}
			// Spaced out, so the separators are the client's.
			spaced := bytes.ReplaceAll(source, []byte(`},{`), []byte("} ,\n {"))
			decoded, err := mapping.DecodeExportBody(spaced, true)
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := decoded.Without(map[string]bool{erasedA: true})
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(out) {
				t.Fatalf("not JSON:\n%s", out)
			}
			again, err := mapping.DecodeExportBody(out, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := mapping.SpanCount(again.ResourceSpans); got != c.left {
				t.Errorf("%d spans left, want %d:\n%s", got, c.left, out)
			}
		})
	}
}

// A body with no span of the traces is the source itself.
func TestWithoutNothingToRemoveIsTheSource(t *testing.T) {
	source, err := mapping.EncodeExportRequest(scrubExport(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := mapping.DecodeExportBody(source, false)
	if err != nil {
		t.Fatal(err)
	}
	out, removed, err := decoded.Without(map[string]bool{"ffffffffffffffffffffffffffffffff": true})
	if err != nil || removed != 0 || !bytes.Equal(out, source) {
		t.Errorf("removed %d, err %v, same body %v", removed, err, bytes.Equal(out, source))
	}
}

// The references a body carries are its stored ones, once each: a
// placeholder names no body.
func TestMediaReferences(t *testing.T) {
	ref := func(sha string, stored bool) string {
		return `{"tracepad_media":"` + sha + `","mime_type":"image/png","size":9000,"stored":` +
			map[bool]string{true: "true", false: "false"}[stored] + `}`
	}
	one, two, three := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	export := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
		scrubSpan(t, keptB, "0000000000000001", "x",
			scrubAttr("gen_ai.input.messages", `[{"content":[`+ref(one, true)+`,`+ref(two, true)+`]}]`),
			scrubAttr("gen_ai.output.messages", `[{"content":[`+ref(one, true)+`,`+ref(three, false)+`]}]`)),
	}}}}}
	if got := mapping.MediaReferences(export); !slices.Equal(got, []string{one, two}) {
		t.Errorf("references = %v, want the two stored ones once each", got)
	}
	if got := mapping.MediaReferences(nil); len(got) != 0 {
		t.Errorf("an empty export references %v", got)
	}
	// The walk rewrites nothing.
	before := proto.Clone(export[0])
	mapping.MediaReferences(export)
	if !proto.Equal(before, export[0]) {
		t.Error("listing the references changed the export")
	}
}
