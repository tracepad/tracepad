package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in      []string
		cmd     string
		numArgs int
	}{
		{nil, "serve", 0},
		{[]string{"--listen", ":9999"}, "serve", 2}, // spec 001 #1: leading flag = default command
		{[]string{"serve", "--listen", ":9999"}, "serve", 2},
		{[]string{"version"}, "version", 0},
	}
	for _, c := range cases {
		cmd, args := splitCommand(c.in)
		if cmd != c.cmd || len(args) != c.numArgs {
			t.Errorf("splitCommand(%v) = %q, %v", c.in, cmd, args)
		}
	}
}

// A secret the operator declared in TRACEPAD_PROJECTS is theirs already; the
// banner names where it is instead of copying it into the log. A generated one
// has no other way out, so it is printed once (spec 001 #9, #12).
func TestPrintStartupPrintsOnlyGeneratedSecrets(t *testing.T) {
	var out bytes.Buffer
	printStartup(&out, &store.BootstrapResult{Created: []store.BootstrapCreated{
		{Project: store.Project{Name: "default"}, Keys: store.KeyPair{PublicKey: "tp-pk-gen", Secret: "tp-sk-generated"}},
		{Project: store.Project{Name: "app"}, Keys: store.KeyPair{PublicKey: "tp-pk-app", Secret: "tp-sk-declared"}, Declared: true},
	}}, ":4318", "")

	got := out.String()
	if strings.Contains(got, "tp-sk-declared") {
		t.Errorf("the declared secret was printed:\n%s", got)
	}
	for _, want := range []string{
		`Project "default" created. Connect your app with either:`,
		"Bearer tp-sk-generated",
		"LANGFUSE_SECRET_KEY=tp-sk-generated",
		`Project "app" created from TRACEPAD_PROJECTS.`,
		"LANGFUSE_PUBLIC_KEY=tp-pk-app",
		"LANGFUSE_SECRET_KEY=<its secret key from TRACEPAD_PROJECTS>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}
}

// Other machines reaching a plain-HTTP listener with nothing saying a TLS proxy
// is in front is a warning at start, naming the address (spec 001 #12).
func TestWarnPlainHTTP(t *testing.T) {
	cases := []struct {
		listen, url string
		warn        bool
	}{
		{":4318", "", true},
		{":4318", "https://traces.example.com", false},
		{"127.0.0.1:4318", "", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		warnPlainHTTP(slog.New(slog.NewTextHandler(&out, nil)), c.listen, c.url)
		logged := out.String()
		if got := strings.Contains(logged, "level=WARN"); got != c.warn {
			t.Errorf("listen %q, url %q: warned = %v, want %v\n%s", c.listen, c.url, got, c.warn, logged)
		}
		if c.warn && (!strings.Contains(logged, "listen="+c.listen) || !strings.Contains(logged, "TRACEPAD_URL")) {
			t.Errorf("the warning should name the address and the fix:\n%s", logged)
		}
	}
}
