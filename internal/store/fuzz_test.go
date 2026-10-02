package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Fuzz targets for what the store reads out of a stranger's text: the search
// box, and the usage object an exporter wrote. See internal/mapping/fuzz_test.go
// for how to run them.

// FuzzSearch: whatever is typed in the search box becomes an expression FTS5
// accepts or a refusal that names the reason — never an SQLite syntax error,
// which would be a 500 for a sentence with a stray parenthesis in it
// (spec 011 #4). The snippet cut from any text is one line of at most the
// documented length, on rune boundaries.
func FuzzSearch(f *testing.F) {
	s := openFresh(f)
	for _, seed := range [][2]string{
		{"refund", "the refund was issued"},
		{`"refund order"`, "refund order refund order"},
		{"err*", "error errors"},
		{"cats AND dogs", "cats and dogs"},
		{"a OR b NOT c", "a b c"},
		{`(x) -y ^z col:w NEAR(a b)`, "x y z w"},
		{`"unclosed`, "unclosed quote"},
		{`""`, ""},
		{"*", "*"},
		{"Réfund", "réfund RÉFUND"},
		{strings.Repeat("a", 600), "a"},
		{"\x00\xff", "\xff\xfe"},
		{"x", strings.Repeat("longword", 100)},
		{"user_id_42", `{"user_id_42":"a","b":["x","y"],"n":1.5e3}`},
		// Over the index's cap, with a character that straddles it.
		{"é", strings.Repeat("é", SearchIndexCap)},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, raw, text string) {
		if len(raw) > 4096 || len(text) > 2*SearchIndexCap {
			t.Skip()
		}
		query, err := ParseSearch(raw)
		if err != nil {
			if !errors.Is(err, ErrEmptySearch) && !strings.Contains(err.Error(), "at most") {
				t.Fatalf("ParseSearch(%q): an unexplained refusal: %v", raw, err)
			}
			return
		}
		if len(query.Terms) == 0 || query.Match == "" {
			t.Fatalf("ParseSearch(%q) answered an empty query without refusing it", raw)
		}
		for _, term := range query.Terms {
			if len(term.Words) == 0 {
				t.Fatalf("ParseSearch(%q) answered a term with no word", raw)
			}
		}
		var n int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM search_fts WHERE search_fts MATCH ?`, query.Match).Scan(&n); err != nil {
			t.Fatalf("FTS5 refused %s (from %q): %v", query.Match, raw, err)
		}

		snippet := Snippet(text, query)
		if !utf8.ValidString(snippet) {
			t.Fatalf("Snippet(%q, %q) is not valid UTF-8: %q", text, raw, snippet)
		}
		if got := utf8.RuneCountInString(snippet); got > SearchSnippetLength {
			t.Fatalf("Snippet is %d characters, over the %d budget", got, SearchSnippetLength)
		}
		if strings.ContainsAny(snippet, "\n\r") {
			t.Fatalf("Snippet(%q) is not one line: %q", text, snippet)
		}

		// What the index holds of a payload: a prefix on a rune boundary
		// and, for JSON, its leaves.
		held := searchable(text)
		if len(held) > SearchIndexCap || !strings.HasPrefix(text, held) {
			t.Fatalf("searchable cut %d bytes that are not a prefix within the cap", len(held))
		}
		if utf8.ValidString(text) && !utf8.ValidString(held) {
			t.Fatalf("searchable cut inside a character")
		}
		for _, field := range []string{FieldInput, FieldOutput, FieldMetadata, FieldName, FieldStatusMessage, FieldTraceName} {
			searchableField(field, text)
		}
		jsonLeaves(text)
	})
}

// FuzzUsageTokens is the counting rule over a stored usage text: five classes
// of count, each nil or within 0..10^9, whatever the text holds (spec 043 #4).
func FuzzUsageTokens(f *testing.F) {
	for _, seed := range []string{
		``, `null`, `{}`, `[]`, `"x"`, `{"input":1,"output":2,"total":3}`,
		`{"input_tokens":1e9}`, `{"input_tokens":1000000001}`, `{"input_tokens":-1}`,
		`{"input_tokens":1.9}`, `{"input_tokens":"7"}`, `{"input_tokens":null,"prompt_tokens":5}`,
		`{"input_tokens":1e400}`, `{"cache_read_input_tokens":4,"cache_read_tokens":5}`,
		`{"input_tokens":{"a":1}}`, `{"input_tokens":[1]}`, `{"input_tokens":true}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, text []byte) {
		if len(text) > 1<<16 {
			t.Skip()
		}
		tokens := usageTokens(text)
		for i, count := range tokens.fields() {
			if *count != nil && (**count < 0 || **count > 1e9) {
				t.Fatalf("class %d counted %d from %q, outside 0..10^9", i, **count, text)
			}
		}
		var object map[string]any
		if json.Unmarshal(text, &object) == nil {
			// Same rule, two doors: compare what they answer.
			other := UsageTokens(object)
			for i, count := range other.fields() {
				if a, b := *count, *tokens.fields()[i]; (a == nil) != (b == nil) || a != nil && *a != *b {
					t.Fatalf("UsageTokens and usageTokens disagree on class %d of %q", i, text)
				}
			}
		}
	})
}

// FuzzUserCursorKey: a hand-written cursor key is refused with a message or
// read as the number its sort binds — never a panic, and never a value that
// is not finite, because it is bound into a comparison against the rollup.
func FuzzUserCursorKey(f *testing.F) {
	for _, sortBy := range UserSorts {
		for _, key := range []string{"", "0", "-1", "1.5", "1e400", "NaN", "Inf", "9223372036854775808", "x", " 1"} {
			f.Add(sortBy, key)
		}
	}
	f.Add("nonsense", "1")
	f.Fuzz(func(t *testing.T, sortBy, key string) {
		value, err := ParseUserCursorKey(sortBy, key)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("a refusal with no message")
			}
			return
		}
		if n, ok := value.(float64); ok && (n != n || n > 1.7976931348623157e308 || n < -1.7976931348623157e308) {
			t.Fatalf("ParseUserCursorKey(%q, %q) = %v: not a number a comparison can use", sortBy, key, n)
		}
	})
}
