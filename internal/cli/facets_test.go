package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// `facets` and the list form of `--env`, `--release` and `--name` (spec 027
// #5). Three clients, one surface: what the flag accepts is exactly what the
// query parameter accepts, because the command passes the string through.

func TestFacetsListsTheValues(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), true, "facets")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	// One block per column, each headed by the flag that takes it, so the
	// output says what to do with what it printed.
	for _, want := range []string{
		"ENV", "production", "staging",
		"NAME", "support-chat", "nightly-eval",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, got.stdout)
		}
	}
	// The corpus names no release, and an empty column says so rather than
	// printing a header over nothing.
	if !strings.Contains(got.stdout, "--release: nothing in this range") {
		t.Errorf("an empty column did not say so:\n%s", got.stdout)
	}
}

// Spec 004 #1 in one assertion: `--json` is the endpoint's own bytes, and the
// three list flags pass their string through untouched.
func TestFacetsAndListFlagsAreTheEndpoint(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		// An explicit range rather than the default: the default is
		// "now", and two calls a moment apart would honestly differ.
		{"a range", []string{"facets", "--since", "2026-08-01T00:00:00Z", "--until", "2026-09-01T00:00:00Z"},
			"/api/v1/facets?from=2026-08-01T00%3A00%3A00Z&to=2026-09-01T00%3A00%3A00Z"},
		{"a list of environments", []string{"traces", "ls", "--env", "production,staging"},
			"/api/v1/traces?environment=production%2Cstaging"},
		{"a list of names", []string{"traces", "ls", "--name", "support-chat,nightly-eval"},
			"/api/v1/traces?name=support-chat%2Cnightly-eval"},
		{"a list of releases", []string{"traces", "ls", "--release", "a,b"},
			"/api/v1/traces?release=a%2Cb"},
		{"sessions take the list too", []string{"sessions", "ls", "--env", "production,staging"},
			"/api/v1/sessions?environment=production%2Cstaging"},
		{"and the statistics", []string{"stats", "--env", "production,staging"},
			"/api/v1/stats?environment=production%2Cstaging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.run(t.Context(), false, append(tc.args, "--json")...)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+testKey)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var fromCommand, fromEndpoint any
			if err := json.Unmarshal([]byte(got.stdout), &fromCommand); err != nil {
				t.Fatalf("--json is not JSON: %v (%s)", err, got.stdout)
			}
			if err := json.NewDecoder(response.Body).Decode(&fromEndpoint); err != nil {
				t.Fatal(err)
			}
			left, _ := json.Marshal(fromCommand)
			right, _ := json.Marshal(fromEndpoint)
			if string(left) != string(right) {
				t.Errorf("the command and the endpoint disagree:\n%s\n%s", left, right)
			}
		})
	}
}

// A list the server refuses is refused here, with the server's own sentence:
// the CLI parses nothing, so there is one answer to what a list may hold.
func TestAnEmptyItemIsRefusedThroughTheCommand(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), false, "traces", "ls", "--env", "production,")
	if got.code == ExitOK {
		t.Fatalf("an empty item was accepted:\n%s", got.stdout)
	}
	if !strings.Contains(got.stderr, "empty item in list") {
		t.Errorf("stderr = %q, want the server's own sentence", got.stderr)
	}
}
