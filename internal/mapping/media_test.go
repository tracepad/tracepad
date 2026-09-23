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

// The reference replaces the matched *object* for the three object shapes
// (#4): an Anthropic image keeps its `type: image` wrapper, and its `source`
// is the reference.
func TestExtractMediaReplacesTheWholeObject(t *testing.T) {
	body := picture(8000, 3)
	messages := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": b64(body)}},
	}}}
	export := otlptest.SpanWith("gen_ai.input.messages", mustJSON(t, messages))
	mapping.ExtractMedia(export, mapping.MediaOptions{})
	got := mustJSON(t, inputOf(t, export))
	want := `[{"content":[{"source":{"mime_type":"image/jpeg","size":8000,"tracepad_media":"` +
		shaOf(body) + `"},"type":"image"}],"role":"user"}]`
	if got != want {
		t.Errorf("input =\n%s\nwant\n%s", got, want)
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
	found := mapping.ExtractMedia(export, mapping.MediaOptions{
		Resolve: func(id string) (string, int64, bool) { return sha, 1234, id == "HELD" },
	})
	if len(found.Bodies) != 0 || len(found.Refs) != 1 {
		t.Fatalf("bodies %d refs %+v, want no body and one ref", len(found.Bodies), found.Refs)
	}
	got := mustJSON(t, inputOf(t, export))
	for _, want := range []string{
		`"image_url":{"url":{"mime_type":"image/png","size":1234,"tracepad_media":"` + sha + `"}}`,
		`"source":{"mime_type":"image/jpeg","size":1234,"tracepad_media":"` + sha + `"}`,
		`"url":"` + missing + `"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("input lacks %s:\n%s", want, got)
		}
	}
}

// InlineMedia is the way back (#8): the references become data URLs, and an
// export of the inlined body maps to the rows the first delivery produced.
func TestInlineMediaRoundTrip(t *testing.T) {
	body := picture(12000, 8)
	messages := mustJSON(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": b64(body)}},
	}}})
	export := otlptest.Export(
		otlptest.ProbeSpan("gen_ai.input.messages", messages),
	)
	export[0].ScopeSpans[0].Spans[0].Attributes = append(export[0].ScopeSpans[0].Spans[0].Attributes,
		&commonpb.KeyValue{Key: "langfuse.observation.output",
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "data:image/png;base64," + b64(body)}}})
	found := mapping.ExtractMedia(export, mapping.MediaOptions{})
	first := mapping.Map(cloneExport(export))

	changed := mapping.InlineMedia(export, func(sha string) (string, []byte, bool) {
		if sha != found.Bodies[0].SHA256 {
			return "", nil, false
		}
		return "image/png", body, true
	})
	if !changed[0] {
		t.Fatal("nothing was inlined")
	}
	encoded, _ := proto.Marshal(export[0])
	if !bytes.Contains(encoded, []byte(b64(body))) {
		t.Fatal("the inlined export does not carry the bytes")
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
		if found.Changed[0] || !found.Changed[1] {
			t.Fatalf("changed = %v, want only the second", found.Changed)
		}
		out, err := decoded.Encode(found.Changed)
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
		source, err := otlptest.JSONBody([]*tracepb.ResourceSpans{plain, media})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := mapping.DecodeExportBody(source, true)
		if err != nil {
			t.Fatal(err)
		}
		found := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
		out, err := decoded.Encode(found.Changed)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(b64(body))) {
			t.Error("the factored JSON body still carries the picture")
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
