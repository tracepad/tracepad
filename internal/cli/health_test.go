package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `tracepad health` is the probe (spec 020 #4), and a probe is only worth
// having if it is wrong in the same direction every time: exit 0 when the
// server answered as itself, exit 1 for every other thing that can happen to
// an HTTP request. These tests are that table.

// TestHealthNeedsNoKey is the property the whole command exists for. A
// container's HEALTHCHECK, a systemd unit and a load balancer all run it, and
// none of them should be holding a project secret to ask whether a process is
// up — so the absence of a key is not a usage error here, unlike everywhere
// else on this command line.
func TestHealthNeedsNoKey(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "TRACEPAD_API_KEY")

	got := h.run(t.Context(), true, "health")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != testVersion {
		t.Errorf("stdout = %q, want the server's version %q", got.stdout, testVersion)
	}
}

// TestHealthJSON: the shape a script reads. The version is the server's, and
// `ok` restates the exit code in a field so that a caller parsing stdout does
// not have to know which spellings of `status` count as healthy.
func TestHealthJSON(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), true, "health", "--json")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	var answer struct {
		Version string `json:"version"`
		OK      bool   `json:"ok"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &answer); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, got.stdout)
	}
	if answer.Version != testVersion || !answer.OK {
		t.Errorf("answer = %+v, want version %q and ok", answer, testVersion)
	}
}

// TestHealthIsJSONOffATerminal: the TTY rule holds here as everywhere else
// (spec 004 #12), which is what the container's HEALTHCHECK actually runs —
// it has no terminal, and it must not need `--json` to be machine-readable.
func TestHealthIsJSONOffATerminal(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), false, "health")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if !strings.HasPrefix(strings.TrimSpace(got.stdout), "{") {
		t.Errorf("stdout = %q, want JSON off a terminal", got.stdout)
	}
}

// TestHealthFailures: every way the probe can fail is exit 1 with the reason
// on stderr — never exit 2, because none of these is a typo, and never exit 0,
// because a probe that passes on a 500 is worse than no probe.
func TestHealthFailures(t *testing.T) {
	refused := "http://127.0.0.1:1"

	cases := []struct {
		name    string
		handler http.HandlerFunc
		url     string
		// want is a fragment of what stderr has to say, so that the
		// operator reading a failed probe learns which failure it was.
		want string
	}{
		{
			name:    "a server error",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) },
			want:    "boom",
		},
		{
			// A 200 from something that is not this server: a proxy's
			// splash page, another service on the port. The version is
			// what makes the answer this server's.
			name: "a 200 without a version",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"status":"ok"}`))
			},
			want: "without a version",
		},
		{
			name: "a body that is not JSON at all",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte("<html>hello from the proxy</html>"))
			},
			want: "unexpected",
		},
		{
			// The address, not just "connection refused": a probe
			// that does not say what it could not reach sends the
			// operator to the wrong machine.
			name: "a refused connection",
			url:  refused,
			want: refused,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			target := c.url
			if c.handler != nil {
				stub := httptest.NewServer(c.handler)
				defer stub.Close()
				target = stub.URL
			}

			got := h.run(t.Context(), true, "health", "--url", target)
			if got.code != ExitFailure {
				t.Fatalf("exit = %d, want %d (stdout: %q, stderr: %q)",
					got.code, ExitFailure, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, c.want) {
				t.Errorf("stderr = %q, want it to name %q", got.stderr, c.want)
			}
			if strings.TrimSpace(got.stdout) != "" {
				t.Errorf("stdout = %q, want nothing on a failed probe", got.stdout)
			}
		})
	}
}
