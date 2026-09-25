// Package termsafe makes text that came from a trace inert where a person
// reads it: a terminal, or a line of prose an MCP client shows (spec 004 #35).
//
// Everything a trace carries is written by whoever holds the project's key, or
// by the end user whose text the application logged, and a terminal obeys the
// bytes it is given: an OSC 52 rewrites the clipboard, a CSI 2J wipes the rows
// above, a C1 CSI does the same in one character, a bidi override makes text
// read backwards. So every control character — C0, DEL, C1 — and the bidi
// embeddings, overrides and isolates become visible escapes: `\x1b`, `\u009b`,
// `\u202e`. A byte that is not UTF-8 is shown as `\xNN` rather than passed on
// for a terminal to guess at. Everything else, backslashes included, is left
// as it was: a name that needs nothing prints exactly as before.
//
// JSON never comes here. Its encoder escapes C0 already, and a JSON answer is
// the API's bytes, which the clients print verbatim (spec 004 #1).
package termsafe

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// String is for a value printed inside a line — a name, an id, a cell. Tab and
// newline are escaped too: in a one-line field they are layout the data does
// not get to choose, since a tab is a column break to a table and a newline
// would let a name forge a row of its own.
func String(s string) string { return escape(s, false) }

// Text is for the few values that are text by nature and print as a block of
// their own — a prompt, a diff, an error message: their newlines and tabs are
// the text's own, and survive. Carriage return does not, because it rewinds
// the line and lets what follows overpaint it.
func Text(s string) string { return escape(s, true) }

// All is String over a list, for the fields that are one.
func All(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = String(value)
	}
	return out
}

// Writer is Text applied to everything written through it: the backstop
// under a human-readable run, so that a value a renderer forgot to pass
// through String still cannot reach the terminal as a control sequence. It
// keeps newline and tab, which a writer cannot tell from the layout around
// them, and the exact sequences in keep — the one styling its owner writes
// itself.
//
// Each Write is escaped as a whole. A sequence split across two writes is
// escaped rather than joined, which is the safe way to be wrong; fmt and
// tabwriter write a line or a cell at a time, so it does not happen.
type Writer struct {
	w    io.Writer
	keep []string
}

// NewWriter returns a Writer over w that lets the sequences in keep through.
func NewWriter(w io.Writer, keep ...string) *Writer { return &Writer{w: w, keep: keep} }

// Write reports len(p) on success: the caller's bytes were all consumed, even
// though more than that went out.
func (s *Writer) Write(p []byte) (int, error) {
	text := string(p)
	if clean(text, true) {
		return s.w.Write(p)
	}
	var out strings.Builder
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			continue
		}
		for _, kept := range s.keep {
			if strings.HasPrefix(text[i:], kept) {
				out.WriteString(Text(text[start:i]))
				out.WriteString(kept)
				i += len(kept) - 1
				start = i + 1
				break
			}
		}
	}
	out.WriteString(Text(text[start:]))
	if _, err := io.WriteString(s.w, out.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}

func escape(s string, multiline bool) string {
	if clean(s, multiline) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&out, `\x%02x`, s[i])
		case !unsafe(r, multiline):
			out.WriteString(s[i : i+size])
		case r < utf8.RuneSelf:
			fmt.Fprintf(&out, `\x%02x`, r)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
		i += size
	}
	return out.String()
}

// clean is the common case answered without allocating: almost every value
// has nothing to escape.
func clean(s string, multiline bool) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || unsafe(r, multiline) {
			return false
		}
		i += size
	}
	return true
}

func unsafe(r rune, multiline bool) bool {
	switch {
	case multiline && (r == '\n' || r == '\t'):
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}
