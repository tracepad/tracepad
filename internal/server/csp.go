package server

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
)

// The policy of an HTML document the server hands out (spec 050). Every
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
// explains (spec 050 #3).

// inlineScript matches a `<script>` element and captures its content. One with
// a `src` has no content, so it matches with an empty body, which is skipped.
var inlineScript = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>(.*?)</script\s*>`)

// htmlComment matches a comment, whose contents the browser never runs and so
// never needs a hash for.
var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// documentPolicy returns the Content-Security-Policy for one HTML document.
//
//   - `default-src 'none'` is the floor: a directive not named here allows
//     nothing, so a new kind of load is a refusal to be decided rather than a
//     permission nobody read.
//   - `script-src 'self'` plus the hash of each inline script: the bundle's
//     own files and the one inline line that starts them, and nothing a
//     payload, a URL or an attribute could smuggle in. No `'unsafe-inline'`,
//     no `'unsafe-eval'`, no `'strict-dynamic'` (spec 050 #1, #2).
//   - `style-src 'self' 'unsafe-inline'`, and only for style (spec 050 #5):
//     the editor writes its stylesheet into a `<style>` element and the
//     dialogs write the page's `style` attribute, neither of which can carry
//     a hash or a nonce a static bundle could know.
//   - `img-src` takes `data:` for the empty icon the document names and
//     `blob:` for the pictures the interface draws from bytes it fetched with
//     the session's credentials (spec 041 #15).
//   - `connect-src 'self'`: the interface talks to the API of the server that
//     served it and to nothing else, which is also what keeps a stolen
//     credential from being posted elsewhere.
//   - `worker-src 'none'`, `base-uri 'none'`, `form-action 'self'` and
//     `frame-ancestors 'none'` close what `default-src` does not cover.
func documentPolicy(document []byte) string {
	scripts := []string{"'self'"}
	for _, match := range inlineScript.FindAllSubmatch(htmlComment.ReplaceAll(document, nil), -1) {
		if len(match[1]) == 0 {
			continue
		}
		sum := sha256.Sum256(match[1])
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
