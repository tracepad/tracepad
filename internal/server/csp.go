package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// The policy of an HTML document the server hands out (spec 051). Every
// response carries spec 001 #14's `frame-ancestors 'none'`; the two documents
// the interface is made of — the SPA's entry and the stub a build without the
// `ui` tag serves — replace it with the policy below, which is the one that
// decides what script and what style may run on this origin beside the
// session cookie.
//
// It is built from the document, not written beside it. SvelteKit's entry
// carries one inline script, the few lines that import the bundle and mount
// the app, and an inline script is exactly what a policy without
// `'unsafe-inline'` refuses; so the policy names it by hash. The hash is taken
// here, when the server starts, from the bytes it is about to send — never
// from a copy in a config file or a build step, which a rebuild could leave
// stale and the interface would then be a blank page that only a console
// explains (spec 051 #3).

// inlineScripts returns the text of every `<script>` element of an HTML
// document that runs from its own content, as the browser would hash it. It is
// a scan over the markup and not a regular expression because the three things
// a pattern gets wrong are the three a hash cannot afford to:
//
//   - a comment is skipped *outside* a script and is script text *inside* one:
//     `<!--` in a script body is part of its source, and cutting it out before
//     looking for scripts hashes something the browser never sees;
//   - a start tag ends at the first `>` *outside* a quoted attribute value, so
//     `<script data-x="a>b">` is one tag and not two;
//   - a script's text ends at `</script` followed by whitespace, `/` or `>`,
//     and the browser reads it with its line ends normalised: CR LF and a
//     lone CR are both LF before the text is hashed, which a source file
//     checked out with Windows line ends would otherwise turn into a blank
//     page.
//
// An element with a `src` runs the file and ignores its text, so it has no
// hash. What SvelteKit emits today, checked on the built entry, is one bare
// `<script>` of tab-indented lines ending in LF, with no comment and no quote
// in its tag; the scan holds for anything the same builder could emit next.
func inlineScripts(document []byte) [][]byte {
	var found [][]byte
	for i := 0; i < len(document); {
		switch {
		case bytes.HasPrefix(document[i:], []byte("<!--")):
			end := bytes.Index(document[i+4:], []byte("-->"))
			if end < 0 {
				return found // an unterminated comment runs to the end
			}
			i += 4 + end + 3
		case isTagStart(document[i:], "script"):
			tagEnd, hasSrc := scanStartTag(document, i+len("<script"))
			if tagEnd < 0 {
				return found
			}
			text, next := scriptText(document, tagEnd)
			if !hasSrc && len(text) > 0 {
				found = append(found, normalizeNewlines(text))
			}
			i = next
		default:
			i++
		}
	}
	return found
}

// isTagStart reports whether b opens with `<name` and then a character that
// ends a tag name, so `<scripts>` and `<script-x>` are not scripts.
func isTagStart(b []byte, name string) bool {
	return len(b) > 0 && b[0] == '<' && nameAt(b[1:], name)
}

// nameAt reports whether b opens with name, in any case, and then ends a tag
// name.
func nameAt(b []byte, name string) bool {
	if len(b) <= len(name) || !strings.EqualFold(string(b[:len(name)]), name) {
		return false
	}
	return strings.IndexByte(" \t\n\r\f/>", b[len(name)]) >= 0
}

// scanStartTag reads attributes from `from` to the `>` that ends the tag,
// honouring quotes, and reports where the tag ends (the index after the `>`,
// or -1) and whether it carries a `src`.
func scanStartTag(document []byte, from int) (end int, hasSrc bool) {
	i := from
	for i < len(document) {
		switch c := document[i]; {
		case c == '>':
			return i + 1, hasSrc
		case c == '"' || c == '\'':
			close := bytes.IndexByte(document[i+1:], c)
			if close < 0 {
				return -1, hasSrc
			}
			i += close + 2
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/':
			i++
		default:
			start := i
			for i < len(document) && strings.IndexByte(" \t\n\r\f/>=\"'", document[i]) < 0 {
				i++
			}
			if i == start {
				i++ // a stray `=` is not an attribute name
				continue
			}
			if strings.EqualFold(string(document[start:i]), "src") {
				hasSrc = true
			}
		}
	}
	return -1, hasSrc
}

// scriptText returns a script's content, from the end of its start tag to its
// closing tag, and the index to carry on from.
func scriptText(document []byte, from int) (text []byte, next int) {
	for i := from; i < len(document); i++ {
		if document[i] == '<' && i+1 < len(document) && document[i+1] == '/' && nameAt(document[i+2:], "script") {
			return document[from:i], i + 2
		}
	}
	return document[from:], len(document)
}

// normalizeNewlines applies the HTML parser's preprocessing of line ends.
func normalizeNewlines(text []byte) []byte {
	if !bytes.Contains(text, []byte{'\r'}) {
		return text
	}
	text = bytes.ReplaceAll(text, []byte("\r\n"), []byte{'\n'})
	return bytes.ReplaceAll(text, []byte{'\r'}, []byte{'\n'})
}

// documentPolicy returns the Content-Security-Policy for one HTML document.
//
//   - `default-src 'none'` is the floor: a directive not named here allows
//     nothing, so a new kind of load is a refusal to be decided rather than a
//     permission nobody read.
//   - `script-src 'self'` plus the hash of each inline script: the bundle's
//     own files and the one inline line that starts them, and nothing a
//     payload, a URL or an attribute could smuggle in. No `'unsafe-inline'`,
//     no `'unsafe-eval'`, no `'strict-dynamic'` (spec 051 #1, #2).
//   - `style-src 'self' 'unsafe-inline'`, and only for style (spec 051 #5):
//     the editor writes its stylesheet into a `<style>` element and the
//     dialogs write the page's `style` attribute, neither of which can carry
//     a hash or a nonce a static bundle could know.
//   - `img-src` takes `data:` for the logo's tile the stub carries inline
//     (spec 006 #30, spec 051 #10) and `blob:` for the pictures the interface
//     draws from bytes it fetched with the session's credentials (spec 041
//     #15).
//   - `connect-src 'self'`: the interface talks to the API of the server that
//     served it and to nothing else, which is also what keeps a stolen
//     credential from being posted elsewhere.
//   - `worker-src 'none'`, `base-uri 'none'`, `form-action 'self'` and
//     `frame-ancestors 'none'` close what `default-src` does not cover.
func documentPolicy(document []byte) string {
	scripts := []string{"'self'"}
	for _, text := range inlineScripts(document) {
		sum := sha256.Sum256(text)
		scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob:",
		"font-src 'self'",
		"connect-src 'self'",
		"worker-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}
