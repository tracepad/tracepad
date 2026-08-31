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

	out, done := h.follow(ctx, true, "tail", "--interval", "20ms")

	// Write while the follow is running: this is the race the command has
	// to survive, not just a sequence it has to render. The writes are paced
	// under the poll interval on purpose, so that some of them land between
	// two polls.
	for i := 2; i <= 6; i++ {
		h.seed(t, &model.Trace{ID: traceHex(i), Name: "during", Environment: "production"},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level:     model.LevelDefault,
				StartTime: seedBase + int64(i)*ms, EndTime: seedBase + int64(i)*ms + ms})
		time.Sleep(15 * time.Millisecond)
	}
	// Wait for the last write to be printed rather than for a duration that
	// is usually one poll: on a loaded runner it is not, and the trace that
	// had not been polled yet was read as one the follow had missed.
	awaitOutput(t, out, traceHex(6), "the last trace written during the follow")
	cancel()

	printed := (<-done).stdout
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

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Ended when the follow has printed, not after a deadline that is a
	// guess about how long a poll takes.
	out, done := h.follow(ctx, false, "tail", "--error", "--interval", "20ms")
	awaitOutput(t, out, traceHex(2), "the failed trace")
	cancel()
	got := <-done

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

// TestTailShowsALateArrival is the regression for what review of PR #5 found:
// a trace's timestamp is when its earliest span started, not when it was
// stored, and exporters batch. A run that began before one already printed can
// arrive after it, and a follow that only looked forward silently skipped it.
func TestTailShowsALateArrival(t *testing.T) {
	h := newHarness(t)
	// Printed first: it started later.
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "started-later", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase + 5000*ms, EndTime: seedBase + 5001*ms})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out, done := h.follow(ctx, true, "tail", "--interval", "20ms")

	// The late trace has to be stored *after* the follow has printed the
	// first one, or there is nothing late about it: both would land on the
	// same poll and the test would pass without exercising anything. That is
	// what the sleep here was for, and a sleep cannot say it happened.
	awaitOutput(t, out, traceHex(1), "the trace that started later")

	// Stored second, but it started five seconds earlier — exactly what a
	// batching exporter produces.
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "started-earlier", Environment: "production"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	// And the follow has to be given until it polls, not until a duration
	// somebody measured on a quiet laptop: this is the wait that made the
	// test flaky on CI (INBOX, gate of PR #28).
	awaitOutput(t, out, traceHex(2), "the late arrival")
	cancel()

	printed := (<-done).stdout
	for _, id := range []string{traceHex(1), traceHex(2)} {
		if count := strings.Count(printed, id); count != 1 {
			t.Fatalf("trace %s printed %d times, want exactly once:\n%s", id, count, printed)
		}
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
