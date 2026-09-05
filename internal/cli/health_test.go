package cli

import (
	"encoding/json"
	"net"
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

// TestListenTarget: the mapping from what the server was told to bind to, to
// what a probe beside it should ask.
func TestListenTarget(t *testing.T) {
	cases := map[string]string{
		":4318":           "http://127.0.0.1:4318",
		":8080":           "http://127.0.0.1:8080",
		"0.0.0.0:8080":    "http://127.0.0.1:8080",
		"[::]:8080":       "http://127.0.0.1:8080",
		"127.0.0.1:9999":  "http://127.0.0.1:9999",
		"myhost:4318":     "http://myhost:4318",
		"[::1]:4318":      "http://[::1]:4318",
		"":                "",
		"8080":            "", // no colon: not an address this can read
		"nonsense":        "",
		"http://host:123": "",
	}
	for in, want := range cases {
		if got := listenTarget(in); got != want {
			t.Errorf("listenTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestHealthFollowsTheListenAddress is the container case (#15): the image sets
// TRACEPAD_LISTEN and nothing else, so an operator moving the port with
// `-e TRACEPAD_LISTEN=:8080` must not end up with a healthy server that its own
// HEALTHCHECK calls dead.
func TestHealthFollowsTheListenAddress(t *testing.T) {
	h := newHarness(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(h.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing points at the server but the listen address.
	delete(h.env, "TRACEPAD_URL")
	delete(h.env, "TRACEPAD_API_KEY")
	h.env["TRACEPAD_LISTEN"] = ":" + port

	got := h.run(t.Context(), true, "health")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", got.code, got.stderr)
	}
	if strings.TrimSpace(got.stdout) != testVersion {
		t.Errorf("stdout = %q, want the version %q", got.stdout, testVersion)
	}
}

// TestHealthPrefersTheURLOverTheListenAddress: the listen address is only the
// fallback. A probe told where to look is not second-guessed — TRACEPAD_URL
// beats TRACEPAD_LISTEN, and `--url` beats both.
func TestHealthPrefersTheURLOverTheListenAddress(t *testing.T) {
	dead := "http://127.0.0.1:1"

	t.Run("TRACEPAD_URL wins", func(t *testing.T) {
		h := newHarness(t)
		h.env["TRACEPAD_URL"] = dead
		h.env["TRACEPAD_LISTEN"] = ":4318"
		got := h.run(t.Context(), true, "health")
		if got.code != ExitFailure || !strings.Contains(got.stderr, dead) {
			t.Errorf("exit = %d, stderr = %q; want the failure to name %s",
				got.code, got.stderr, dead)
		}
	})

	t.Run("--url wins over both", func(t *testing.T) {
		h := newHarness(t)
		h.env["TRACEPAD_LISTEN"] = ":4318"
		got := h.run(t.Context(), true, "health", "--url", dead)
		if got.code != ExitFailure || !strings.Contains(got.stderr, dead) {
			t.Errorf("exit = %d, stderr = %q; want the failure to name %s",
				got.code, got.stderr, dead)
		}
	})

	t.Run("the listen address is used when neither is set", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "TRACEPAD_URL")
		h.env["TRACEPAD_LISTEN"] = ":1"
		got := h.run(t.Context(), true, "health")
		if got.code != ExitFailure || !strings.Contains(got.stderr, "127.0.0.1:1") {
			t.Errorf("exit = %d, stderr = %q; want the probe to have followed "+
				"TRACEPAD_LISTEN to 127.0.0.1:1", got.code, got.stderr)
		}
	})
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
