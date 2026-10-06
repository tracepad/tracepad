//go:build unix

package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// versionAt is a server's /health answering v, on loopback; its address is
// what a server listening there names with --listen.
func versionAt(t *testing.T, v string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, v)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// The upgrade's check of what runs from the binary it replaces refuses every
// server the way back's check of that path would (the review of #223): one
// look at the processes before the stop is enough only because of it, and
// prepare skips the second (spec 054 #43). Over every kind of server that
// can run from the path — older, the same, newer, silent, one whose binary
// or version cannot be read — whatever the way back refuses, the upgrade
// refuses too.
func TestTheUpgradesServerCheckHoldsTheWayBacks(t *testing.T) {
	t.Parallel()
	const from, to = "0.5.0", "0.5.1"
	bin := filepath.Join(t.TempDir(), "tracepad")
	server := func(exe, listen string) Process {
		return Process{PID: 4242, Exe: exe, Cwd: "/", Argv: []string{"tracepad", "serve", "--listen", listen, "--data-dir", filepath.Join(t.TempDir(), "data")}}
	}
	cases := map[string][]Process{
		"none":             nil,
		"older":            {server(bin, versionAt(t, "0.4.0"))},
		"the same":         {server(bin, versionAt(t, from))},
		"newer":            {server(bin, versionAt(t, to))},
		"a development":    {server(bin, versionAt(t, "dev"))},
		"silent":           {server(bin, "127.0.0.1:1")},
		"another address":  {server(bin, "192.0.2.10:4318")},
		"an unread binary": {server("", versionAt(t, from))},
		"another binary":   {server(filepath.Join(t.TempDir(), "tracepad"), versionAt(t, to))},
		"not a server":     {{PID: 4243, Exe: bin, Argv: []string{"tracepad", "mcp"}}},
	}
	// What the way back refuses, so the property is not held vacuously.
	backRefuses := map[string]bool{"newer": true, "a development": true, "silent": true, "another address": true, "an unread binary": true}
	for name, procs := range cases {
		r := &runner{deps: Deps{Sys: listed{procs: procs}, HTTP: &http.Client{Timeout: time.Second}}}
		back := r.serversOn(context.Background(), bin, from, false)
		up := r.serversOn(context.Background(), bin, to, true)
		if (back != nil) != backRefuses[name] {
			t.Errorf("%s: the way back's check says %v", name, back)
		}
		if back != nil && up == nil {
			t.Errorf("%s: the way back refuses (%v), and the upgrade does not", name, back)
		}
	}
}

// A server listening on one address of this machine's that is not a
// loopback one is not asked its version (the review of #223): the plan says
// it was not checked, rather than that it does not say its version, and
// does not call it behind; a binary put under it is refused as one whose
// version could not be checked.
func TestAServerOnOneInterfaceIsSaidNotChecked(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	bin := w.install
	p := Process{PID: 4242, Exe: bin, Cwd: "/", Argv: []string{"tracepad", "serve", "--listen", "192.0.2.10:4318", "--data-dir", filepath.Join(t.TempDir(), "data")}}
	procs, _, _ := w.host.Candidates(context.Background())
	deps := w.deps()
	deps.Sys = listed{procs: append(procs, p)}
	rep, code := runIn(t, context.Background(), deps, "--plan", "--to", fOld)
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, "server pid 4242 was not checked: it listens on 192.0.2.10:4318 only") || strings.Contains(strings.Join(rep.Person, "\n"), "pid 4242") || code != exitOK {
		t.Errorf("%d %s\nnotes: %s\nperson: %q", code, rep.Summary, notes, rep.Person)
	}
	r := &runner{deps: deps}
	err := r.serversOn(context.Background(), bin, fOld, false)
	if err == nil || !strings.Contains(err.Error(), "its version could not be checked: it listens on 192.0.2.10:4318") {
		t.Errorf("the way back's check: %v", err)
	}
}
