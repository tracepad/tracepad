package termsafe

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"support-chat", "support-chat"},
		{"héllo 日本語 ✓", "héllo 日本語 ✓"},
		// A backslash is text: only what a terminal acts on is changed.
		{`C:\path\x1b`, `C:\path\x1b`},
		{"\x1b]52;c;ZWNobyBwd25lZA==\x07", `\x1b]52;c;ZWNobyBwd25lZA==\x07`},
		{"\x1b[2J\x1b[H", `\x1b[2J\x1b[H`},
		{"a\tb\nc\rd", `a\x09b\x0ac\x0dd`},
		{"\x00\x7f", `\x00\x7f`},
		{"\u0080\u009b\u009f\u00a0", `\u0080\u009b\u009f` + "\u00a0"},
		{"\u202aa\u202eb\u2066c\u2069", `\u202aa\u202eb\u2066c\u2069`},
		// The marks that only hint a direction are not overrides.
		{"\u200e\u200f", "\u200e\u200f"},
		{"ok\xffok\xc2", `ok\xffok\xc2`},
	} {
		if got := String(tc.in); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTextKeepsItsLines(t *testing.T) {
	in := "line one\n\tindented\r\x1b[2Jline two\u009b"
	want := "line one\n\tindented" + `\x0d\x1b[2Jline two\u009b`
	if got := Text(in); got != want {
		t.Errorf("Text(%q) = %q, want %q", in, got, want)
	}
}

func TestAll(t *testing.T) {
	got := All([]string{"a", "b\x1b"})
	if len(got) != 2 || got[0] != "a" || got[1] != `b\x1b` {
		t.Errorf("All = %q", got)
	}
}

// Nothing to escape is the common case, and it allocates nothing.
func TestCleanValuesAreReturnedAsTheyAre(t *testing.T) {
	value := "an ordinary trace name"
	if allocs := testing.AllocsPerRun(100, func() { _ = String(value) }); allocs != 0 {
		t.Errorf("String allocated %v times on a clean value", allocs)
	}
}

func TestWriter(t *testing.T) {
	var out strings.Builder
	w := NewWriter(&out, "\x1b[2m", "\x1b[0m")
	for _, chunk := range []string{
		"plain line\n",
		"\x1b[2mfaint\x1b[0m\tcell\n",
		"name\x1b]52;c;x\x07\x1b[2J\u009b\u202e\r\n",
		"\x1b[2m\x1b[31mred\x1b[0m\n",
	} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	want := "plain line\n" +
		"\x1b[2mfaint\x1b[0m\tcell\n" +
		`name\x1b]52;c;x\x07\x1b[2J\u009b\u202e\x0d` + "\n" +
		"\x1b[2m" + `\x1b[31mred` + "\x1b[0m\n"
	if out.String() != want {
		t.Errorf("wrote %q\nwant  %q", out.String(), want)
	}
}
