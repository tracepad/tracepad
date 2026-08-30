package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The search query language and the snippet a matching row carries (spec 011
// #4, #6). Nothing here touches the database: this is the translation between
// what somebody typed and what FTS5 is asked, and back again.

const (
	// SearchIndexCap is how much of each payload is indexed. The rest is
	// stored and readable but not searched (spec 011 #2): a 500 KB document
	// on one observation's input would otherwise cost the index as much as a
	// hundred ordinary traces.
	SearchIndexCap = 64 << 10
	// SearchSnippetLength bounds the window a row carries around its hit,
	// ellipses included.
	SearchSnippetLength = 160
	// MaxSearchQueryLength bounds the text `q` accepts.
	MaxSearchQueryLength = 512
)

// The fields an entry can name. `FieldTraceName` is the one that belongs to
// the trace itself, and its entry carries no observation id.
const (
	FieldInput         = "input"
	FieldOutput        = "output"
	FieldMetadata      = "metadata"
	FieldName          = "name"
	FieldStatusMessage = "status_message"
	FieldTraceName     = "trace_name"
)

// ErrEmptySearch is a `q` with no word in it — punctuation, or nothing at all.
// The caller answers it with a 400 rather than with an empty listing, because
// "nothing matched" would be a lie about a query that was never asked.
var ErrEmptySearch = errors.New("search text must contain at least one word")

// SearchTerm is one thing the user asked for: a bare word, a quoted phrase, or
// a word with a trailing `*`. `Words` are folded the way the tokenizer folds
// them, which is what lets the snippet and the interface find the hit again.
type SearchTerm struct {
	Words  []string
	Prefix bool
}

// SearchQuery is a parsed `q`: the FTS5 expression to hand to MATCH, and the
// terms behind it.
type SearchQuery struct {
	// Text is what the user typed, kept for the error messages and the
	// empty state that name the query back.
	Text string
	// Match is the FTS5 expression. It is built here from words the parser
	// extracted, never from the user's string: FTS5's own syntax is an
	// internal interface, and a stray parenthesis in a sentence is a syntax
	// error rather than a search (spec 011 #4).
	Match string
	Terms []SearchTerm
}

// ParseSearch turns the user's text into a query FTS5 cannot fail to parse.
//
// Bare words are quoted tokens joined by AND; text inside double quotes is a
// phrase; a bare word ending in `*` is a prefix. Everything else — `AND`, `OR`,
// `NOT`, parentheses, `:`, `^`, `-` — is literal text or is dropped by the
// tokenizer, because every word this produces is passed to FTS5 inside quotes
// and every character in it is one the tokenizer accepts.
func ParseSearch(raw string) (*SearchQuery, error) {
	if utf8.RuneCountInString(raw) > MaxSearchQueryLength {
		return nil, fmt.Errorf("search text must be at most %d characters", MaxSearchQueryLength)
	}
	query := &SearchQuery{Text: raw}
	var expressions []string
	for _, token := range searchTokens(raw) {
		words := tokenWords(token.text)
		if len(words) == 0 {
			// Punctuation, or a lone `*`: it asked for nothing, so it
			// narrows nothing.
			continue
		}
		// The words go back into the expression as their original runes:
		// FTS5 folds case and diacritics with the same tokenizer that
		// built the index, so folding them here as well would only be a
		// second, less accurate opinion.
		phrase := `"` + strings.Join(words, " ") + `"`
		if token.prefix {
			phrase += "*"
		}
		expressions = append(expressions, phrase)
		query.Terms = append(query.Terms, SearchTerm{
			Words: foldWords(words), Prefix: token.prefix,
		})
	}
	if len(expressions) == 0 {
		return nil, ErrEmptySearch
	}
	query.Match = strings.Join(expressions, " AND ")
	return query, nil
}

// searchToken is one thing the user separated with a space or with quotes.
type searchToken struct {
	text   string
	prefix bool
}

// searchTokens splits the raw text into quoted phrases and bare words. An
// unclosed quote runs to the end of the input, which is what somebody typing a
// phrase and pressing Enter meant.
func searchTokens(raw string) []searchToken {
	var out []searchToken
	for i := 0; i < len(raw); {
		r, width := utf8.DecodeRuneInString(raw[i:])
		switch {
		case unicode.IsSpace(r):
			i += width
		case r == '"':
			i += width
			end := strings.IndexByte(raw[i:], '"')
			if end < 0 {
				out = append(out, searchToken{text: raw[i:]})
				return out
			}
			out = append(out, searchToken{text: raw[i : i+end]})
			i += end + 1
		default:
			start := i
			for i < len(raw) {
				r, width := utf8.DecodeRuneInString(raw[i:])
				if unicode.IsSpace(r) || r == '"' {
					break
				}
				i += width
			}
			word := raw[start:i]
			// Only a trailing `*` on a bare word is a prefix; one in
			// the middle is punctuation the tokenizer drops.
			trimmed := strings.TrimRight(word, "*")
			out = append(out, searchToken{text: trimmed, prefix: trimmed != word})
		}
	}
	return out
}

// tokenWords splits one token the way `unicode61` does: maximal runs of letters
// and digits, with combining marks kept attached to the letter they modify, so
// that a decomposed "é" stays one word rather than becoming two.
func tokenWords(text string) []string {
	var out []string
	var word strings.Builder
	for _, r := range text {
		if isSearchRune(r) {
			word.WriteRune(r)
			continue
		}
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	if word.Len() > 0 {
		out = append(out, word.String())
	}
	return out
}

func isSearchRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func foldWords(words []string) []string {
	out := make([]string, len(words))
	for i, word := range words {
		out[i] = foldSearch(word)
	}
	return out
}

// foldSearch lower-cases and strips diacritics, the way `remove_diacritics 2`
// does inside the index.
//
// An approximation of SQLite's own table rather than a copy of it, and
// deliberately so: nothing is *matched* by this — FTS5 answers the query — and
// what it is for is finding the hit again inside a payload the index has
// already said contains one. A rune it folds differently costs a snippet that
// starts at the beginning of the field instead of at the hit.
func foldSearch(word string) string {
	var out strings.Builder
	out.Grow(len(word))
	for _, r := range word {
		if unicode.IsMark(r) {
			continue
		}
		r = unicode.ToLower(r)
		if base, folded := diacritics[r]; folded {
			r = base
		}
		out.WriteRune(r)
	}
	return out.String()
}

// diacritics maps the precomposed Latin letters to their base, which is what
// `remove_diacritics 2` leaves of them. Built from the table below so that the
// data reads as data.
var diacritics = func() map[rune]rune {
	table := []string{
		"aàáâãäåāăą",
		"cçćĉċč",
		"dďđ",
		"eèéêëēĕėęě",
		"gĝğġģ",
		"hĥħ",
		"iìíîïĩīĭįı",
		"jĵ",
		"kķ",
		"lĺļľŀł",
		"nñńņňŉ",
		"oòóôõöøōŏő",
		"rŕŗř",
		"sśŝşš",
		"tţťŧ",
		"uùúûüũūŭůűų",
		"wŵ",
		"yýÿŷ",
		"zźżž",
	}
	folded := map[rune]rune{}
	for _, group := range table {
		runes := []rune(group)
		for _, r := range runes[1:] {
			folded[r] = runes[0]
		}
	}
	return folded
}()

// Snippet cuts the window a matching row carries: at most `SearchSnippetLength`
// characters around the first hit of the query's first term, on word
// boundaries, with an ellipsis wherever the text continues.
//
// It is computed here rather than by FTS5's `snippet()`, which a contentless
// index cannot offer — it has no text to cut from (spec 011 #6). Text that is
// not valid UTF-8 is read as runes with the invalid bytes replaced, so the cut
// is always on a rune boundary.
func Snippet(text string, query *SearchQuery) string {
	runes := []rune(text)
	words := indexWords(runes)
	start, end := 0, len(runes)
	if len(query.Terms) > 0 {
		if at, found := findTerm(words, query.Terms[0]); found {
			start, end = words[at].start, words[at].end
		}
	}
	return window(runes, words, start, end)
}

// indexedWord is one word of a text: where it sits, and what it folds to.
type indexedWord struct {
	folded     string
	start, end int
}

func indexWords(runes []rune) []indexedWord {
	var out []indexedWord
	for i := 0; i < len(runes); {
		if !isSearchRune(runes[i]) {
			i++
			continue
		}
		start := i
		for i < len(runes) && isSearchRune(runes[i]) {
			i++
		}
		out = append(out, indexedWord{
			folded: foldSearch(string(runes[start:i])), start: start, end: i,
		})
	}
	return out
}

// findTerm reports where a term first occurs: its words in order and adjacent,
// with the last one a prefix when the term asked for one.
func findTerm(words []indexedWord, term SearchTerm) (int, bool) {
	if len(term.Words) == 0 || len(words) < len(term.Words) {
		return 0, false
	}
	for at := 0; at+len(term.Words) <= len(words); at++ {
		matched := true
		for i, wanted := range term.Words {
			got := words[at+i].folded
			last := i == len(term.Words)-1
			if got != wanted && !(last && term.Prefix && strings.HasPrefix(got, wanted)) {
				matched = false
				break
			}
		}
		if matched {
			return at, true
		}
	}
	return 0, false
}

// window centres the budget on [hit, hitEnd) and pulls both edges out to a word
// boundary, so the snippet never begins or ends inside a word.
func window(runes []rune, words []indexedWord, hit, hitEnd int) string {
	// Two of the budget are reserved for the ellipses; a window that turns
	// out to need neither is simply shorter than it could have been, which
	// nobody reading it can tell.
	budget := SearchSnippetLength - 2
	start, end := hit, hitEnd
	if end-start > budget {
		end = start + budget
	}
	spare := budget - (end - start)
	start = max(0, start-spare/2)
	end = min(len(runes), start+budget)
	start = max(0, end-budget)

	// Snapped to word boundaries, and then checked: a token longer than the
	// budget is a word both edges land inside, so snapping pushes the start
	// past the end and the slice below is a panic (found in review of
	// PR #16). Payload text is full of such tokens — a base64 blob, a JWT,
	// an opaque id — and a search that matches one of them is exactly the
	// search somebody types. There is no window on a word boundary to be
	// had, so the answer is the budget from the hit: cutting inside a word
	// nothing else fits beside is what a reader wanted anyway.
	snapped, snappedEnd := start, end
	if snapped > 0 {
		snapped = wordStart(words, snapped)
	}
	if snappedEnd < len(runes) {
		snappedEnd = wordEnd(words, snappedEnd)
	}
	if snapped < snappedEnd {
		start, end = snapped, snappedEnd
	} else {
		start, end = hit, min(len(runes), hit+budget)
	}
	body := collapseSpace(string(runes[start:end]))
	if start > 0 {
		body = "…" + body
	}
	if end < len(runes) {
		body += "…"
	}
	return body
}

// wordStart moves an edge forward off the middle of a word. Strictly inside:
// an edge already on a word's first character is on a boundary, and skipping
// that whole word would drop text the window had room for.
func wordStart(words []indexedWord, at int) int {
	for _, word := range words {
		if word.start < at && at < word.end {
			return word.end
		}
	}
	return at
}

// wordEnd moves an edge back off the middle of a word.
func wordEnd(words []indexedWord, at int) int {
	for _, word := range words {
		if word.start < at && at < word.end {
			return word.start
		}
	}
	return at
}

// collapseSpace makes the window one line: a status message with newlines in it
// is still a snippet under a row.
func collapseSpace(text string) string {
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " "))
}

// searchableField is what one field contributes to the index, and what the
// snippet is later cut from: the same function on both sides, so that a window
// can never be cut from a text the index never saw.
//
// The three payloads are JSON, and what is searched is the text inside them
// rather than the text `json.Marshal` produced (spec 011 #14). Names and status
// messages are already the text.
func searchableField(field, text string) string {
	switch field {
	case FieldInput, FieldOutput, FieldMetadata:
		return searchable(jsonLeaves(text))
	}
	return searchable(text)
}

// jsonFrame is one open container of the walk: an object alternates keys and
// values, an array is values all the way down.
type jsonFrame struct {
	object bool
	key    bool
}

// jsonLeaves is the text of a JSON payload: its scalar leaves — strings as
// their text, numbers and booleans as their JSON text — in the order the
// document holds them, one per line. Keys, brackets, quotes and `null` are not
// text anybody said, and a search that matches them matches the envelope
// instead of the message (spec 011 #14).
//
// A payload that does not parse is returned whole, which is what it was before
// there was anything to parse: this store writes JSON, but the index is also
// what a hand-written row or an older release left behind, and a payload it
// cannot read is better searched crudely than not at all.
//
// The walk stops once it has the cap's worth of text, because Decision 2
// applies to what is extracted. That bounds what is produced, not what is read:
// a document of megabytes whose leaves never fill the cap is walked to its end
// (20 MiB of `null` is 60 ms). What bounds the reading is the caller — the
// ingest body cap on the way in, and on the way out a payload this process has
// just decompressed to cut a snippet from anyway.
//
// The leaves are joined by a newline, which is a separator to the tokenizer and
// not a token: two neighbouring values are therefore adjacent positions, and a
// phrase can run from the end of one into the start of the next (spec 011 #14).
// The alternative is an FTS row per leaf, which is the index size Decisions 2
// and 3 exist to refuse.
func jsonLeaves(text string) string {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()

	var (
		out    strings.Builder
		frames []jsonFrame
		whole  bool // a complete top-level value has been read
	)
	write := func(leaf string) {
		if leaf == "" {
			return
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(leaf)
	}

	for out.Len() < SearchIndexCap {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		// A syntax error, or a second value after the first: not the one
		// JSON document a payload is.
		if err != nil || whole {
			return text
		}

		key := false
		if top := len(frames) - 1; top >= 0 && frames[top].object {
			key = frames[top].key
			frames[top].key = !key
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				frames = append(frames, jsonFrame{object: true, key: true})
			case '[':
				frames = append(frames, jsonFrame{})
			default:
				frames = frames[:len(frames)-1]
			}
		case string:
			if !key {
				write(value)
			}
		case json.Number:
			write(value.String())
		case bool:
			write(strconv.FormatBool(value))
		}
		whole = len(frames) == 0
	}
	return out.String()
}

// searchable is what of a value goes into the index: its first SearchIndexCap
// bytes, cut on a rune boundary so the tokenizer never sees a half character.
func searchable(text string) string {
	if len(text) <= SearchIndexCap {
		return text
	}
	cut := SearchIndexCap
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
