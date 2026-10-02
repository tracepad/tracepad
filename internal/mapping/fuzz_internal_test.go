package mapping

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The small readers the mapper applies to a stranger's strings. They are
// unexported, so these targets live in the package; the whole-body targets are
// in fuzz_test.go.

// FuzzScalarReaders feeds one string to every reader that turns attribute text
// into a number, an instant or a document, and asserts what each promises
// about its answer.
func FuzzScalarReaders(f *testing.F) {
	for _, seed := range []string{
		"", "0", "-0", "42", " 42 ", "1.5", "-1.5e3", "1e400", "-1e400", "NaN", "Inf", "+Inf", "-inf",
		"0x10", "007", ".5", "+1", "1_000", "9223372036854775807", "9223372036854775808",
		"-9223372036854775808", "-9223372036854775809",
		`"2026-10-02T10:00:00Z"`, "2026-10-02T10:00:00.123456789Z", "2026-10-02T10:00:00+03:00",
		"0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z", `"1787738400000000000"`, `""x""`,
		`{}`, `{"a":1}`, `{"a":1}]`, `[]`, `[1,2]`, `"a"`, `null`, `{"input":7,"output":"8"}`,
		"data:image/png;base64,AAAA", "DATA:image/png;BASE64,", "data:;base64,AAAA", "data:x/y;base64",
		"@@@langfuseMedia:type=image/png|id=abc|source=bytes@@@", "@@@langfuseMedia:@@@", "@@@langfuseMedia:@@",
		"AAAA", "AAA", "AA==", "A", "_-8=", "+/8",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<16 {
			t.Skip()
		}

		// Numbers: finite, or refused. A figure that is NaN or infinite
		// cannot be marshalled and would lose the batch it is in.
		for _, value := range []any{text, strings.TrimSpace(text)} {
			if n, ok := asNumber(value); ok && (math.IsNaN(n) || math.IsInf(n, 0)) {
				t.Fatalf("asNumber(%q) = %v, which is not a number a column can hold", text, n)
			}
		}
		if n, ok := jsonNumberText(text); ok {
			if math.IsNaN(n) || math.IsInf(n, 0) {
				t.Fatalf("jsonNumberText(%q) = %v", text, n)
			}
			if !json.Valid([]byte(strings.Trim(text, " \t\n\r"))) {
				t.Fatalf("jsonNumberText(%q) read a number from text that is not JSON", text)
			}
		}
		if n, ok := asInteger(text); ok && math.Abs(float64(n)) >= 1<<53 {
			t.Fatalf("asInteger(%q) = %d, past what a float holds exactly", text, n)
		}

		// Instants: nanoseconds the clock can represent.
		for _, value := range []any{text, float64(len(text)) * 1e18, math.NaN(), math.Inf(1), int64(len(text))} {
			parseInstant(value)
		}

		// Documents: what the media walk reads as one JSON value is one.
		if doc, ok := decodeDocument(text); ok {
			if !json.Valid([]byte(text)) {
				t.Fatalf("decodeDocument took %q, which is not one JSON value", text)
			}
			if _, ok := encodeDocument(doc); !ok {
				t.Fatalf("a document that decoded does not encode: %q", text)
			}
		}
		if obj, ok := parseJSONObject(text); ok && len(obj) == 0 {
			t.Fatalf("parseJSONObject(%q) answered an empty object as present", text)
		}

		// Media shapes: a recognised string is parsed whole and never
		// answers an empty id or an invalid reference.
		if mime, data, ok := parseDataURL(text); ok {
			if !strings.Contains(mime, "/") {
				t.Fatalf("parseDataURL(%q) answered mime %q", text, mime)
			}
			if !strings.HasSuffix(text, data) {
				t.Fatalf("parseDataURL(%q) answered data %q, which is not the tail", text, data)
			}
		}
		if _, id, ok := parseLangfuseRef(text); ok && id == "" {
			t.Fatalf("parseLangfuseRef(%q) answered an empty id", text)
		}
		if body, ok := decodeBase64(text); ok && len(body) > len(text) {
			t.Fatalf("decodeBase64(%q) made %d bytes from %d", text, len(body), len(text))
		}
		if mayHoldMedia(text) != mayHoldMediaSlow(text) {
			t.Fatalf("mayHoldMedia(%q) disagrees with the plain search", text)
		}
	})
}

// mayHoldMediaSlow is mayHoldMedia without containsFold's index walk: lower
// both sides and search. The two must agree, ASCII case aside.
func mayHoldMediaSlow(s string) bool {
	for _, needle := range []string{`"base64"`, `"blob"`, "inline_data", "inlineData", LangfuseMarker} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(s), ";base64,")
}
