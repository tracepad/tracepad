package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// `tail` (spec 004 #13, Testing #3 and #5): polling the public API, against a
// server that is being written to while the follow runs.

// TestTailFollowsAnIngestingServer starts a follow, ingests while it runs, and
// asserts every trace is printed exactly once. The watermark is the cursor
// pair, so a trace committed between two polls cannot be missed or repeated.
func TestTailFollowsAnIngestingServer(t *testing.T) {
	h := newHarness(t)
	// One trace before the follow starts: `tail` shows the end of the
	// stream before following it, the way tail(1) does.
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "before", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	output := make(chan string, 1)
	go func() {
		got := h.run(ctx, true, "tail", "--interval", "20ms")
		output <- got.stdout
	}()

	// Write while the follow is running: this is the race the command has
	// to survive, not just a sequence it has to render.
	for i := 2; i <= 6; i++ {
		h.seed(t, &model.Trace{ID: traceHex(i), Name: "during", Environment: "production"},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level:     model.LevelDefault,
				StartTime: seedBase + int64(i)*ms, EndTime: seedBase + int64(i)*ms + ms})
		time.Sleep(15 * time.Millisecond)
	}
	// One more poll interval so the last write is certainly seen.
	time.Sleep(120 * time.Millisecond)
	cancel()

	printed := <-output
	for i := 1; i <= 6; i++ {
		if count := strings.Count(printed, traceHex(i)); count != 1 {
			t.Fatalf("trace %d printed %d times, want exactly once:\n%s", i, count, printed)
		}
	}
	// Oldest first: a follow reads like a log.
	if strings.Index(printed, traceHex(1)) > strings.Index(printed, traceHex(6)) {
		t.Errorf("output is newest-first, want a log order:\n%s", printed)
	}
}

// TestTailFiltersAndJSON: the follow inherits the listing's filters, and in a
// pipe it emits one JSON row per line — the shape a script greps.
func TestTailFiltersAndJSON(t *testing.T) {
	h := newHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "ok", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "broken", Environment: "production"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Level: model.LevelError, StartTime: seedBase + ms, EndTime: seedBase + 2*ms})

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	got := h.run(ctx, false, "tail", "--error", "--interval", "20ms")

	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], traceHex(2)) {
		t.Fatalf("output = %q, want only the failed trace", got.stdout)
	}
	if !strings.HasPrefix(lines[0], `{"id":`) {
		t.Errorf("line = %q, want one JSON row per line in a pipe", lines[0])
	}
}

func TestTailRejectsANonPositiveInterval(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), false, "tail", "--interval", "0s")
	if got.code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error", got.code)
	}
}

// httpGet fetches a URL with the test project's key, for the byte-identity
// check the CLI makes about itself.
func httpGet(t *testing.T, target string) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
