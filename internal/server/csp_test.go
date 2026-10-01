package server

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/ui"
)

// The policy the interface's documents are served under (spec 050): built from
// the document, so that what it names is what is sent.

func hashOf(code string) string {
	sum := sha256.Sum256([]byte(code))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// directives splits a policy into its directives, by name.
func directives(t *testing.T, policy string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, part := range strings.Split(policy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), " ")
		if _, seen := out[name]; seen {
			t.Errorf("directive %q twice in %q", name, policy)
		}
		out[name] = value
	}
	return out
}

// TestDocumentPolicyNamesTheInlineScripts: the hash of each script written into
// the document, and none for a script that is a file, a comment, or empty.
func TestDocumentPolicyNamesTheInlineScripts(t *testing.T) {
	const boot = "\n{ __sveltekit_x = { base: \"\" }; import(\"/_app/a.js\"); }\n"
	for _, tc := range []struct {
		name, html string
		want       string
	}{
		{"one inline script", `<html><body><script>` + boot + `</script></body></html>`,
			"'self' " + hashOf(boot)},
		{"attributes and capitals", `<SCRIPT type="module" nonce="x">` + boot + `</SCRIPT >`,
			"'self' " + hashOf(boot)},
		{"two scripts, in order", `<script>a()</script><script>b()</script>`,
			"'self' " + hashOf("a()") + " " + hashOf("b()")},
		{"a file is the policy's 'self'", `<script src="/_app/x.js"></script>`, "'self'"},
		{"a comment is never run", `<!-- <script>evil()</script> --><p>`, "'self'"},
		{"no script at all", `<p>hello</p>`, "'self'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := directives(t, documentPolicy([]byte(tc.html)))["script-src"]
			if got != tc.want {
				t.Errorf("script-src = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDocumentPolicyAllowsNothingItWasNotToldTo is the floor: everything not
// named is refused, script is never inline or evaluated, and no page can embed
// this one.
func TestDocumentPolicyAllowsNothingItWasNotToldTo(t *testing.T) {
	policy := documentPolicy([]byte(`<script>x()</script>`))
	got := directives(t, policy)

	if got["default-src"] != "'none'" {
		t.Errorf("default-src = %q, want 'none'", got["default-src"])
	}
	for _, unsafe := range []string{"'unsafe-inline'", "'unsafe-eval'", "'strict-dynamic'", "*", "http:", "https:"} {
		if strings.Contains(" "+got["script-src"]+" ", " "+unsafe+" ") {
			t.Errorf("script-src allows %s: %q", unsafe, got["script-src"])
		}
	}
	for name, want := range map[string]string{
		"style-src":       "'self' 'unsafe-inline'",
		"img-src":         "'self' data: blob:",
		"font-src":        "'self'",
		"connect-src":     "'self'",
		"worker-src":      "'none'",
		"base-uri":        "'none'",
		"form-action":     "'self'",
		"frame-ancestors": "'none'",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	// Nothing else is opened: a directive added here is a decision for the
	// spec's log, not a line in a diff.
	if len(got) != 10 {
		t.Errorf("policy names %d directives, want 10: %q", len(got), policy)
	}
}

// TestInterfaceDocumentsGoOutUnderTheirPolicy: the SPA's entry — however it is
// asked for — carries the policy with the hash of the script it is made of,
// once; a file of the bundle and an API answer keep the header every response
// has.
func TestInterfaceDocumentsGoOutUnderTheirPolicy(t *testing.T) {
	const boot = "{ start(); }"
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.useBundle(fstest.MapFS{
		"index.html":                    {Data: []byte(`<!doctype html><script>` + boot + `</script>`)},
		"_app/immutable/entry/start.js": {Data: []byte(`export const start = 1`)},
	})

	for _, path := range []string{"/", "/traces", "/p/abc/dashboard", "/index.html", "/index.html/", "/nowhere"} {
		rec := h.get(t, path)
		// The entry is the document, whichever way it is spelled: a trailing
		// slash reached the file server, which sent it as a plain file with
		// the base header and no policy.
		expectStatus(t, rec, 200)
		policies := rec.Header().Values("Content-Security-Policy")
		if len(policies) != 1 {
			t.Fatalf("%s: %d Content-Security-Policy headers, want one: %q", path, len(policies), policies)
		}
		if script := directives(t, policies[0])["script-src"]; script != "'self' "+hashOf(boot) {
			t.Errorf("%s: script-src = %q, want 'self' and the hash of the inline script", path, script)
		}
	}

	for _, path := range []string{"/_app/immutable/entry/start.js", "/health", "/api/v1", "/api/v1/nowhere"} {
		rec := h.get(t, path)
		if got := rec.Header().Values("Content-Security-Policy"); len(got) != 1 || got[0] != "frame-ancestors 'none'" {
			t.Errorf("%s: Content-Security-Policy = %q, want only frame-ancestors 'none'", path, got)
		}
	}
}

// TestTheStubIsServedUnderAPolicyToo: a build without the interface hands out
// one page, which has a style of its own and no script, and the policy that
// goes with it is the same function's.
func TestTheStubIsServedUnderAPolicyToo(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.get(t, "/")
	expectStatus(t, rec, 200)
	policies := rec.Header().Values("Content-Security-Policy")
	if len(policies) != 1 || policies[0] != documentPolicy(ui.Stub) {
		t.Fatalf("Content-Security-Policy = %q, want %q", policies, documentPolicy(ui.Stub))
	}
	if got := directives(t, policies[0])["script-src"]; got != "'self'" {
		t.Errorf("script-src = %q: the stub runs no script, so it should name none", got)
	}
}
