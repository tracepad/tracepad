package mapping_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// picture is n bytes that are not all one value, so two sizes never share a
// hash by accident.
func picture(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7) ^ seed
	}
	return out
}

func shaOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func b64(body []byte) string { return base64.StdEncoding.EncodeToString(body) }

// inputOf maps an export and answers its one observation's input.
func inputOf(t *testing.T, export []*tracepb.ResourceSpans) any {
	t.Helper()
	result := mapping.Map(export)
	if len(result.Observations) != 1 {
		t.Fatalf("mapped %d observations, want 1", len(result.Observations))
	}
	return result.Observations[0].Input
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The four shapes of spec 041 #1, each at the floor and one byte under it.
func TestExtractMediaShapes(t *testing.T) {
	shapes := []struct {
		name string
		part func(mime, data string) any
	}{
		{"data URL", func(mime, data string) any {
			return map[string]any{"type": "image_url",
				"image_url": map[string]any{"url": "data:" + mime + ";base64," + data}}
		}},
		{"Anthropic source", func(mime, data string) any {
			return map[string]any{"type": "image",
				"source": map[string]any{"type": "base64", "media_type": mime, "data": data}}
		}},
		{"Gemini inline_data", func(mime, data string) any {
			return map[string]any{"inline_data": map[string]any{"mime_type": mime, "data": data}}
		}},
		{"Gemini inlineData", func(mime, data string) any {
			return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
		}},
		{"GenAI blob", func(mime, data string) any {
			return map[string]any{"type": "blob", "modality": "image", "mime_type": mime, "content": data}
		}},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			at := picture(mapping.MediaMinSize, 1)
			under := picture(mapping.MediaMinSize-1, 2)
			messages := []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				shape.part("image/png", b64(at)),
				shape.part("image/png", b64(under)),
			}}}
			export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, messages))

			found := mapping.ExtractMedia(export, mapping.MediaOptions{})
			if len(found.Bodies) != 1 {
				t.Fatalf("extracted %d bodies, want the one at %d bytes", len(found.Bodies), mapping.MediaMinSize)
			}
			if got := found.Bodies[0]; got.SHA256 != shaOf(at) || got.MimeType != "image/png" ||
				!bytes.Equal(got.Body, at) {
				t.Fatalf("body = %s %s %d bytes, want the 4 KiB picture", got.SHA256, got.MimeType, len(got.Body))
			}
			if len(found.Refs) != 1 || found.Refs[0].TraceID != "00112233445566778899aabbccddeeff" {
				t.Fatalf("refs = %+v, want one for the span's trace", found.Refs)
			}

			stored := mustJSON(t, inputOf(t, export))
			if strings.Contains(stored, b64(at)) {
				t.Errorf("the 4 KiB body is still inline: %.200s", stored)
			}
			if !strings.Contains(stored, b64(under)) {
				t.Errorf("the body under the floor was extracted; it must stay inline")
			}
			ref := `{"mime_type":"image/png","size":4096,"tracepad_media":"` + shaOf(at) + `"}`
			if !strings.Contains(stored, ref) {
				t.Errorf("input does not carry the reference %s:\n%.400s", ref, stored)
			}
		})
	}
}

// In the three object shapes the reference takes the slot the bytes were in,
// and every other field of the object stays the client's (Decision 19): an
// Anthropic source keeps its type, a blob part its modality, a Gemini part
// what rides beside its inline data.
func TestExtractMediaKeepsTheObject(t *testing.T) {
	body := picture(8000, 3)
	ref := `{"mime_type":"image/jpeg","size":8000,"tracepad_media":"` + shaOf(body) + `"}`
	cases := []struct {
		name string
		part any
		want string
	}{
		{"Anthropic source",
			map[string]any{"type": "image", "cache_control": map[string]any{"type": "ephemeral"},
				"source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": b64(body)}},
			`{"cache_control":{"type":"ephemeral"},"source":{"data":` + ref + `,"media_type":"image/jpeg","type":"base64"},"type":"image"}`},
		{"GenAI blob",
			map[string]any{"type": "blob", "modality": "image", "mime_type": "image/jpeg", "content": b64(body)},
			`{"content":` + ref + `,"mime_type":"image/jpeg","modality":"image","type":"blob"}`},
		{"Gemini part",
			map[string]any{"thought_signature": "c2ln", "inline_data": map[string]any{"mime_type": "image/jpeg", "data": b64(body), "display_name": "cat"}},
			`{"inline_data":{"data":` + ref + `,"display_name":"cat","mime_type":"image/jpeg"},"thought_signature":"c2ln"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, []any{
				map[string]any{"role": "user", "content": []any{c.part}}}))
			mapping.ExtractMedia(export, mapping.MediaOptions{})
			got := mustJSON(t, inputOf(t, export))
			want := `[{"content":[` + c.want + `],"role":"user"}]`
			if got != want {
				t.Errorf("input =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A data URL inside a longer string is text, not media (#1).
func TestExtractMediaWholeStringOnly(t *testing.T) {
	body := picture(6000, 4)
	text := "see data:image/png;base64," + b64(body) + " for details"
	export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, []any{
		map[string]any{"role": "user", "content": text}}))
	found := mapping.ExtractMedia(export, mapping.MediaOptions{})
	if len(found.Bodies) != 0 || found.Any() {
		t.Fatalf("a data URL inside a sentence was extracted: %d bodies", len(found.Bodies))
	}
}

// A string attribute that is, whole, a data URL becomes an object attribute,
// and the mapper reads it as the reference.
func TestExtractMediaWholeAttribute(t *testing.T) {
	body := picture(5000, 5)
	export := otlptest.SpanWith("langfuse.observation.input", "data:application/pdf;base64,"+b64(body))
	mapping.ExtractMedia(export, mapping.MediaOptions{})
	got := mustJSON(t, inputOf(t, export))
	want := `{"mime_type":"application/pdf","size":5000,"tracepad_media":"` + shaOf(body) + `"}`
	if got != want {
		t.Errorf("input = %s, want %s", got, want)
	}
}

// Two generations sending one image are one body and two refs (#2).
func TestExtractMediaDeduplicates(t *testing.T) {
	body := picture(9000, 6)
	url := "data:image/png;base64," + b64(body)
	first := otlptest.ProbeSpan("gen_ai.input.messages", mustJSON(t, []any{map[string]any{"content": url}}))
	second := otlptest.ProbeSpan("gen_ai.input.messages", mustJSON(t, []any{map[string]any{"content": url}}))
	second.TraceId = bytes.Repeat([]byte{0xab}, 16)
	second.SpanId = bytes.Repeat([]byte{0xcd}, 8)
	found := mapping.ExtractMedia(otlptest.Export(first, second), mapping.MediaOptions{})
	if len(found.Bodies) != 1 {
		t.Fatalf("%d bodies, want 1", len(found.Bodies))
	}
	if len(found.Refs) != 2 {
		t.Fatalf("%d refs, want one per trace: %+v", len(found.Refs), found.Refs)
	}
}

// The placeholder setting keeps nothing and says so in the reference (#6).
func TestExtractMediaPlaceholder(t *testing.T) {
	body := picture(7000, 7)
	export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, []any{
		map[string]any{"content": "data:image/webp;base64," + b64(body)}}))
	found := mapping.ExtractMedia(export, mapping.MediaOptions{Placeholder: true})
	if len(found.Bodies) != 0 || len(found.Refs) != 0 {
		t.Fatalf("placeholder kept %d bodies and %d refs", len(found.Bodies), len(found.Refs))
	}
	got := mustJSON(t, inputOf(t, export))
	want := `[{"content":{"mime_type":"image/webp","size":7000,"stored":false,"tracepad_media":"` + shaOf(body) + `"}}]`
	if got != want {
		t.Errorf("input = %s, want %s", got, want)
	}
}

// A recognised shape whose data is not base64 stays as it came, with a
// warning naming the trace (edge cases).
func TestExtractMediaMalformedStaysInline(t *testing.T) {
	junk := strings.Repeat("not base64! ", 600)
	export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, []any{
		map[string]any{"type": "base64", "media_type": "image/png", "data": junk}}))
	var warned []string
	found := mapping.ExtractMedia(export, mapping.MediaOptions{
		Warn: func(traceID, reason string) { warned = append(warned, traceID) },
	})
	if found.Any() || len(found.Bodies) != 0 {
		t.Fatal("a malformed body was extracted")
	}
	if len(warned) != 1 || warned[0] != "00112233445566778899aabbccddeeff" {
		t.Errorf("warnings = %v, want one naming the trace", warned)
	}
}

// The Langfuse SDK's reference string is rewritten when its body is held, and
// left as the client wrote it otherwise (#9) — whole-string and inside the
// Anthropic object the SDK leaves it in.
func TestExtractMediaLangfuseReference(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	held := "@@@langfuseMedia:type=image/png|id=HELD|source=base64_data_uri@@@"
	missing := "@@@langfuseMedia:type=image/png|id=GONE|source=base64_data_uri@@@"
	messages := []any{map[string]any{"content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": held}},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": held}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": missing}},
	}}}
	export := otlptest.SpanWith("langfuse.observation.input", mustJSON(t, messages))
	asked := map[string]int{}
	found := mapping.ExtractMedia(export, mapping.MediaOptions{
		Resolve: func(id string) (string, int64, bool) {
			asked[id]++
			return sha, 1234, id == "HELD"
		},
	})
	if len(found.Bodies) != 0 || len(found.Refs) != 1 {
		t.Fatalf("bodies %d refs %+v, want no body and one ref", len(found.Bodies), found.Refs)
	}
	// One lookup per id however often a batch names it.
	if asked["HELD"] != 1 || asked["GONE"] != 1 {
		t.Errorf("the store was asked %v, want once per id", asked)
	}
	got := mustJSON(t, inputOf(t, export))
	for _, want := range []string{
		`"image_url":{"url":{"mime_type":"image/png","size":1234,"tracepad_media":"` + sha + `"}}`,
		`"source":{"data":{"mime_type":"image/jpeg","size":1234,"tracepad_media":"` + sha + `"},"media_type":"image/jpeg","type":"base64"}`,
		`"url":"` + missing + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("input lacks %s:\n%s", want, got)
		}
	}
}

// InlineMedia is the way back (#8): each reference becomes what it replaced
// (Decision 19) — the base64 of an object shape's slot, the data URL of a
// whole string — so an export of the inlined body is the client's payload
// again and maps to the rows the first delivery produced.
func TestInlineMediaRoundTrip(t *testing.T) {
	body := picture(12000, 8)
	url := "data:image/png;base64," + b64(body)
	messages := mustJSON(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64(body)}},
		map[string]any{"type": "blob", "modality": "image", "mime_type": "image/png", "content": b64(body)},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
	}}})
	// A string attribute holding, whole, one Gemini part: the document's
	// root is an object shape.
	part := mustJSON(t, map[string]any{"inline_data": map[string]any{"mime_type": "image/png", "data": b64(body)}})
	span := otlptest.ProbeSpan("gen_ai.input.messages", messages)
	span.Attributes = append(span.Attributes,
		&commonpb.KeyValue{Key: "langfuse.observation.output",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: url}}},
		&commonpb.KeyValue{Key: "langfuse.observation.metadata.part",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: part}}})
	export := otlptest.Export(span)
	sent := cloneExport(export)
	found := mapping.ExtractMedia(export, mapping.MediaOptions{})
	if len(found.Bodies) != 1 {
		t.Fatalf("extracted %d bodies, want 1", len(found.Bodies))
	}
	first := mapping.Map(cloneExport(export))

	changed := mapping.InlineMedia(export, func(sha string) (string, []byte, bool) {
		if sha != found.Bodies[0].SHA256 {
			return "", nil, false
		}
		return "image/png", body, true
	})
	if !changed.Any() {
		t.Fatal("nothing was inlined")
	}
	for i, kv := range export[0].ScopeSpans[0].Spans[0].Attributes {
		want := sent[0].ScopeSpans[0].Spans[0].Attributes[i]
		got, _ := kv.Value.GetValue().(*commonpb.AnyValue_StringValue)
		if got == nil {
			t.Fatalf("%s came back as %T, want a string", kv.Key, kv.Value.GetValue())
		}
		if !sameJSON(got.StringValue, want.Value.GetStringValue()) {
			t.Errorf("%s came back as\n%.300s\nwant\n%.300s", kv.Key, got.StringValue, want.Value.GetStringValue())
		}
	}
	again := mapping.ExtractMedia(export, mapping.MediaOptions{})
	if len(again.Bodies) != 1 || again.Bodies[0].SHA256 != found.Bodies[0].SHA256 {
		t.Fatalf("re-extraction found %d bodies", len(again.Bodies))
	}
	second := mapping.Map(export)
	if a, b := mustJSON(t, first.Observations[0].Input), mustJSON(t, second.Observations[0].Input); a != b {
		t.Errorf("input after the round trip =\n%s\nwant\n%s", b, a)
	}
	if a, b := mustJSON(t, first.Observations[0].Output), mustJSON(t, second.Observations[0].Output); a != b {
		t.Errorf("output after the round trip = %s, want %s", b, a)
	}
}

// sameJSON compares two strings as JSON documents when both are one, and as
// text otherwise: a rewritten document is re-encoded with its keys in order.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	return mustMarshalAny(x) == mustMarshalAny(y)
}

func mustMarshalAny(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

func cloneExport(export []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	out := make([]*tracepb.ResourceSpans, len(export))
	for i, rs := range export {
		out[i] = proto.Clone(rs).(*tracepb.ResourceSpans)
	}
	return out
}

// The splice (#5): a rewritten ResourceSpans is re-encoded where it stood, and
// an untouched one, an unreadable one and the rest of the envelope keep their
// bytes — in both encodings.
func TestExportBodySplice(t *testing.T) {
	body := picture(5000, 9)
	plain := otlptest.SpanWith("gen_ai.input.messages", `[{"content":"hello"}]`)[0]
	media := otlptest.SpanWith("gen_ai.input.messages",
		mustJSON(t, []any{map[string]any{"content": "data:image/png;base64," + b64(body)}}))[0]

	t.Run("protobuf", func(t *testing.T) {
		encoded, err := mapping.EncodeExportRequest([]*tracepb.ResourceSpans{plain, media})
		if err != nil {
			t.Fatal(err)
		}
		// An unreadable block between the two: field 1, length 3, junk.
		plainLen := len(mustMarshal(t, plain))
		prefix := encoded[:plainLenField(plainLen)]
		junk := []byte{0x0a, 0x03, 0xff, 0xff, 0xff}
		source := append(append(append([]byte{}, prefix...), junk...), encoded[len(prefix):]...)

		decoded, err := mapping.DecodeExportBody(source, false)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Unreadable != 1 || len(decoded.ResourceSpans) != 2 {
			t.Fatalf("decoded %d spans, %d unreadable", len(decoded.ResourceSpans), decoded.Unreadable)
		}
		found := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
		if found.Rewrites.Changed()[0] || !found.Rewrites.Changed()[1] {
			t.Fatalf("changed = %v, want only the second", found.Rewrites.Changed())
		}
		out, err := decoded.Encode(found.Rewrites)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(out, append(append([]byte{}, prefix...), junk...)) {
			t.Error("the untouched block and the unreadable one did not keep their bytes")
		}
		if bytes.Contains(out, []byte(b64(body))) {
			t.Error("the factored body still carries the picture")
		}
		again, err := mapping.DecodeExportBody(out, false)
		if err != nil || again.Unreadable != 1 || len(again.ResourceSpans) != 2 {
			t.Fatalf("the spliced body does not decode the same: %v", err)
		}
	})

	t.Run("json", func(t *testing.T) {
		encoded, err := otlptest.JSONBody([]*tracepb.ResourceSpans{plain, media})
		if err != nil {
			t.Fatal(err)
		}
		var elements []json.RawMessage
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(envelope["resourceSpans"], &elements); err != nil {
			t.Fatal(err)
		}
		// The client's own spelling: indentation, a key before the array,
		// and a prompt with markup, none of which a re-marshal would keep.
		untouched := strings.Replace(string(elements[0]), `hello`, `<b>hello</b> & bye`, 1)
		untouched = strings.Replace(untouched, `{`, "{\n    ", 1)
		head := "{\n  \"zz\": 1,\n  \"resourceSpans\": [\n    " + untouched + ",\n    "
		tail := "\n  ]\n}\n"
		source := []byte(head + string(elements[1]) + tail)
		decoded, err := mapping.DecodeExportBody(source, true)
		if err != nil {
			t.Fatal(err)
		}
		found := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
		out, err := decoded.Encode(found.Rewrites)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(b64(body))) {
			t.Error("the factored JSON body still carries the picture")
		}
		if !bytes.HasPrefix(out, []byte(head)) || !bytes.HasSuffix(out, []byte(tail)) {
			t.Errorf("the envelope and the untouched element did not keep their bytes:\n%.400s", out)
		}
		spans, unreadable, err := mapping.DecodeExportRequestJSON(out)
		if err != nil || unreadable != 0 || len(spans) != 2 {
			t.Fatalf("the spliced JSON body does not decode: %v", err)
		}
		if got := mustJSON(t, mapping.Map(spans).Observations); !strings.Contains(got, mapping.MediaRefKey) {
			t.Errorf("the spliced JSON body lost the reference: %.300s", got)
		}
	})
}

func mustMarshal(t *testing.T, rs *tracepb.ResourceSpans) []byte {
	t.Helper()
	out, err := proto.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// plainLenField is the length of the first envelope field: its tag, its
// length prefix and its bytes.
func plainLenField(n int) int {
	prefix := 1
	for v := n; v >= 0x80; v >>= 7 {
		prefix++
	}
	return 1 + prefix + n
}

// The archive and the way out differ from what the client sent by the media
// and nothing else (#5, Decision 19): in a JSON body the rewritten element
// keeps the fields the decoder does not know, its markup and its key order,
// and the JSON document inside an attribute keeps its own; extracted and put
// back, the body is the client's, byte for byte.
func TestExportBodyJSONKeepsTheClientsBytes(t *testing.T) {
	body := picture(6000, 11)
	url := "data:image/png;base64," + b64(body)
	document := `{"role": "user",  "content": [{"type": "text", "text": "<b>look</b> & tell"}, ` +
		`{"type": "image_url", "image_url": {"url": "` + url + `", "detail": "high"}}], "a_last": 1}`
	// The client's own escaping: a JSON string with no HTML escapes.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		t.Fatal(err)
	}
	quoted := bytes.TrimSpace(buf.Bytes())
	source := []byte(`{"resourceSpans": [{"resource": {"attributes": [], "entityRefs": [{"type": "service"}]},
  "scopeSpans": [{"scope": {"name": "s"}, "spans": [{"traceId": "00112233445566778899aabbccddeeff",
    "spanId": "0011223344556677", "name": "gen <1>", "startTimeUnixNano": "1", "endTimeUnixNano": "2",
    "futureField": {"x": true},
    "attributes": [{"key": "gen_ai.input.messages", "value": {"stringValue": ` + string(quoted) + `}},
                   {"key": "whole", "value": {"stringValue": "` + url + `"}}]}]}]},
  {"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "a & b"}}]}}]}`)

	decoded, err := mapping.DecodeExportBody(source, true)
	if err != nil {
		t.Fatal(err)
	}
	found := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
	factored, err := decoded.Encode(found.Rewrites)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(factored, []byte(b64(body))) {
		t.Fatal("the archive still carries the picture")
	}
	for _, kept := range []string{`"entityRefs": [{"type": "service"}]`, `"futureField": {"x": true}`,
		`"name": "gen <1>"`, `{"stringValue": "a & b"}`} {
		if !bytes.Contains(factored, []byte(kept)) {
			t.Errorf("the archive lost %s", kept)
		}
	}
	// The document inside the attribute keeps its order and spacing: only
	// the URL became the reference.
	again, err := mapping.DecodeExportBody(factored, true)
	if err != nil {
		t.Fatal(err)
	}
	stored := again.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes[0].Value.GetStringValue()
	ref := `{"mime_type":"image/png","size":6000,"tracepad_media":"` + shaOf(body) + `"}`
	if want := strings.Replace(document, `"`+url+`"`, ref, 1); stored != want {
		t.Errorf("the attribute's document =\n%.300s\nwant\n%.300s", stored, want)
	}

	inlined := mapping.InlineMedia(again.ResourceSpans, func(string) (string, []byte, bool) {
		return "image/png", body, true
	})
	whole, err := again.Encode(inlined)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustReformat(t, whole); got != mustReformat(t, source) {
		t.Errorf("extracted and put back, the body differs from the client's:\n%.600s", whole)
	}
	if !bytes.Contains(whole, []byte(`"futureField": {"x": true}`)) || !bytes.Contains(whole, quoted) {
		t.Error("the way out is not the client's bytes where nothing was media")
	}
}

// mustReformat compacts a JSON document without re-escaping it: the splice
// writes the values it replaced compactly, and that is the only difference
// it is allowed.
func mustReformat(t *testing.T, doc []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, doc); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Under the placeholder setting a Langfuse string is not resolved, even to a
// body the project already holds: nothing new is kept alive (#6).
func TestExtractMediaPlaceholderLeavesLangfuseStrings(t *testing.T) {
	reference := "@@@langfuseMedia:type=image/png|id=HELD|source=base64_data_uri@@@"
	export := otlptest.SpanWith("langfuse.observation.input", mustJSON(t, []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}}}))
	found := mapping.ExtractMedia(export, mapping.MediaOptions{
		Placeholder: true,
		Resolve:     func(string) (string, int64, bool) { return strings.Repeat("ab", 32), 10, true },
	})
	if found.Any() || len(found.Refs) != 0 || len(found.Resolved) != 0 {
		t.Fatalf("placeholder resolved a Langfuse string: refs %v, resolved %v", found.Refs, found.Resolved)
	}
	if got := mustJSON(t, inputOf(t, export)); !strings.Contains(got, reference) {
		t.Errorf("input = %s, want the SDK's string as sent", got)
	}
}

// A data URL's scheme and `;base64`, and a MIME type, are case-insensitive:
// the match is extracted, and the reference carries the lower-case type the
// interface and the serving headers test.
func TestExtractMediaCaseInsensitive(t *testing.T) {
	body := picture(5000, 12)
	export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, []any{
		map[string]any{"content": "DATA:Image/PNG;BASE64," + b64(body)},
		map[string]any{"type": "base64", "media_type": "IMAGE/JPEG", "data": b64(picture(5000, 13))},
	}))
	found := mapping.ExtractMedia(export, mapping.MediaOptions{})
	if len(found.Bodies) != 2 {
		t.Fatalf("extracted %d bodies, want both", len(found.Bodies))
	}
	for _, body := range found.Bodies {
		if body.MimeType != strings.ToLower(body.MimeType) {
			t.Errorf("stored type %q, want it lower-case", body.MimeType)
		}
	}
	got := mustJSON(t, inputOf(t, export))
	if !strings.Contains(got, `"mime_type":"image/png"`) || !strings.Contains(got, `"mime_type":"image/jpeg"`) {
		t.Errorf("input = %.400s, want lower-case types in the references", got)
	}
}
