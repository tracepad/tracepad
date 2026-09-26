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
// is in front is a warning at start, naming the address — and, in the image,
// where the wildcard bind is the design and the publish decides the reach, one
// INFO line instead (spec 001 #12).
func TestWarnPlainHTTP(t *testing.T) {
	cases := []struct {
		listen, url string
		container   bool
		level       string // "" when nothing is logged
	}{
		{":4318", "", false, "WARN"},
		{":4318", "https://traces.example.com", false, ""},
		{"127.0.0.1:4318", "", false, ""},
		{":4318", "", true, "INFO"},
		{":4318", "https://traces.example.com", true, ""},
	}
	for _, c := range cases {
		var out bytes.Buffer
		warnPlainHTTP(slog.New(slog.NewTextHandler(&out, nil)), c.listen, c.url, c.container)
		logged := out.String()
		if c.level == "" {
			if logged != "" {
				t.Errorf("listen %q, url %q, container %v: want nothing logged, got\n%s",
					c.listen, c.url, c.container, logged)
			}
			continue
		}
		if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "level="+c.level) ||
			!strings.Contains(logged, "listen="+c.listen) {
			t.Errorf("listen %q, url %q, container %v: want one %s line naming the address, got\n%s",
				c.listen, c.url, c.container, c.level, logged)
		}
		fix := "TRACEPAD_URL"
		if c.container {
			fix = "docs/docker.md"
		}
		if !strings.Contains(logged, fix) {
			t.Errorf("the line should point at %s:\n%s", fix, logged)
		}
	}
}
