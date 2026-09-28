package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/mapping"
)

// What an erasure writes to the log carries no text of an error (spec 047
// #32): an error's text may quote what it holds, an erasure's user among it,
// in any form, and a filter of text is a list of the forms someone thought
// of. So the lines give the cause, the Go types of the error, SQLite's code
// and the position — and this test is the invariant: every way an erasure
// fails, with an id that the errors quote in every form they are known to
// take, and the whole of the log taken. No line holds the id, a run of four
// characters of it in any of its forms, or a word of the error.

// hostileIDs are user ids that the forms below make hard: one plain, one
// with what encodings change, and one longer than a label is stored.
func hostileIDs() []string {
	return []string{
		"qz7-kwv-9zq",
		`qz7 kwv+9zq/"é\`,
		strings.Repeat("kwvqz9-", 172)[:1200],
	}
}

// formsOf are the ways an error may quote an id: as it is, quoted as Go
// quotes it, with its letters escaped, escaped for a URL path and for a query
// (a + for a space), in a JSON string as it is written, in one written as
// ASCII, and as %XX in either case.
func formsOf(id string) []string {
	inner := func(s string) string { return s[1 : len(s)-1] }
	encoded, _ := json.Marshal(id)
	var percent, upper strings.Builder
	for _, b := range []byte(id) {
		fmt.Fprintf(&percent, "%%%02x", b)
		fmt.Fprintf(&upper, "%%%02X", b)
	}
	ascii := strings.Builder{}
	for _, r := range id {
		if r < 0x80 {
			ascii.WriteRune(r)
		} else {
			fmt.Fprintf(&ascii, `\u%04x`, r)
		}
	}
	return []string{id, inner(strconv.Quote(id)), inner(strconv.QuoteToASCII(id)), url.PathEscape(id),
		url.QueryEscape(id), inner(string(encoded)), ascii.String(), percent.String(), upper.String()}
}

// hostileError is an error whose text quotes the id in every form, after
// words of its own.
func hostileError(id string) error {
	return errors.New("ERRTEXT refused " + strings.Join(formsOf(id), " "))
}

// leaks lists what of an id or an error is in a log: the id and each of its
// forms whole, any four characters of any form, and the error's own words.
func leaks(log, id string) []string {
	var found []string
	for _, word := range []string{"ERRTEXT", "must be hex-encoded", "refused", "no such table"} {
		if strings.Contains(log, word) {
			found = append(found, "the word "+strconv.Quote(word))
		}
	}
	for _, form := range formsOf(id) {
		runes := []rune(form)
		for i := 0; i+4 <= len(runes); i++ {
			if piece := string(runes[i : i+4]); strings.Contains(log, piece) {
				found = append(found, fmt.Sprintf("%q of %q", piece, form))
				break
			}
		}
	}
	return found
}

// erasureLogPath is one way an erasure's failure reaches the log: it runs
// and answers the message the log must hold, so that a path that logs nothing
// does not pass for one that leaks nothing.
type erasureLogPath struct {
	name    string
	message []string
	run     func(t *testing.T, f *sweepFixture, id string, fail error)
}

func erasureLogPaths() []erasureLogPath {
	// A tail to scrub, for an erasure of any user: a batch that holds a trace's
	// spans, and the row that says the erasure deleted that trace.
	tailOf := func(t *testing.T, f *sweepFixture, id string, writer func(job jobSubmitter) jobSubmitter) {
		f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "someone-else", "", "late", nil)}), false, daysAgo(1))
		batch := f.count(t, `SELECT MAX(id) FROM raw_batches`)
		received := f.count(t, `SELECT received_at FROM raw_batches WHERE id = ?`, batch)
		e := f.startErasure(t, id)
		for _, job := range []WriteJob{&erasureBegin{ID: e.ID}, &erasureStep{ID: e.ID, Phase: phaseTail}} {
			if err := f.writer.Submit(t.Context(), job); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.store.db.Exec(`INSERT INTO erasure_tail (erasure_id, trace_id, arrived_from, arrived_to)
			VALUES (?, ?, ?, ?)`, e.ID, hexTrace(1), received-int64(time.Second), received+int64(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.runErasure(t.Context(), writer(f.writer), e.ID, EraserOptions{}); err == nil {
			t.Fatal("the tail did not fail")
		}
	}
	return []erasureLogPath{
		{"the raw phase", []string{"the raw phase of an erasure failed", "an erasure failed"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				e := f.startErasure(t, id)
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { step, ok := job.(*erasureStep); return ok && step.TracesAtStart != nil }}
				if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
					t.Fatal(err)
				}
			}},
		{"the parsed phase", []string{"the parsed phase of an erasure failed", "an erasure failed"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				e := f.startErasure(t, id)
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { _, chunk := job.(*UserDataErase); return chunk }}
				if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
					t.Fatal(err)
				}
			}},
		{"the tail and its unrecorded cause", []string{"the tail of an erasure failed", "a failed tail's cause is not recorded"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				tailOf(t, f, id, func(inner jobSubmitter) jobSubmitter {
					return &failingJobs{jobSubmitter: inner, err: fail, fails: func(job WriteJob) bool {
						switch j := job.(type) {
						case *RawScrub:
							return true
						case *erasureStep:
							return j.TailFailure != ""
						}
						return false
					}}
				})
			}},
		{"a scrub retried after a conflict", []string{"the tail of an erasure failed"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				tailOf(t, f, id, func(inner jobSubmitter) jobSubmitter {
					return &conflictsThenFails{jobSubmitter: inner, err: fail}
				})
			}},
		{"a batch that cannot be read", []string{"could not be read to scrub it"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				// The decoder's own words: it quotes the value it refused.
				quoted, _ := json.Marshal(id)
				seedRawBatch(t, f.store, f.project.ID, &RawBatch{ReceivedAt: daysAgo(1), ContentType: mapping.ContentTypeJSON,
					Body: []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":` + string(quoted) + `,"spanId":"aa"}]}]}]}`)})
				batch := f.count(t, `SELECT MAX(id) FROM raw_batches`)
				e := &Erasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5", ProjectID: f.project.ID, UserID: id}
				if _, err := f.store.planScrub(t.Context(), e, batch, map[string]bool{hexTrace(1): true}); err != nil {
					t.Fatal(err)
				}
			}},
		{"the worker", []string{"an erasure stopped before it ended"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { _, end := job.(*erasureEnd); return end }}
				worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
				worker.Start()
				defer worker.Close()
				f.startErasure(t, id)
				waitFor(t, func() bool { return f.erasureOf(t, id).attempts == 1 && f.erasureOf(t, id).Phase == phaseTail })
				time.Sleep(50 * time.Millisecond)
			}},
		{"the worker that cannot read", []string{"could not read the next erasure"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				// The driver's own words: a table it cannot find.
				if _, err := f.store.db.Exec(`ALTER TABLE erasures RENAME TO erasures_away`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = f.store.db.Exec(`ALTER TABLE erasures_away RENAME TO erasures`) })
				worker := f.store.NewEraser(f.writer, EraserOptions{Poll: time.Hour})
				worker.Start()
				defer worker.Close()
				time.Sleep(100 * time.Millisecond)
			}},
		{"a stop whose pause is refused", []string{"a stopped erasure's start stays counted"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				e := f.startErasure(t, id)
				writer := &stopsThenRefusesPause{jobSubmitter: f.writer, stop: cancel, err: fail}
				if _, err := f.store.runErasure(ctx, writer, e.ID, EraserOptions{}); err == nil {
					t.Fatal("the run was not stopped")
				}
			}},
		{"a write whose submitter left", []string{"a write of an erasure did not commit"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				w, err := f.store.NewWriter(WriterOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				parked, release := make(chan struct{}), make(chan struct{})
				w.beforeCommit = func() {
					close(parked)
					<-release
				}
				ctx, cancel := context.WithCancel(t.Context())
				gone := make(chan error, 1)
				go func() { gone <- w.Submit(ctx, hostileJob{err: fail}) }()
				<-parked
				cancel()
				<-gone
				close(release)
				// The writer commits, fails and logs after the submitter has left.
				time.Sleep(100 * time.Millisecond)
			}},
		{"a window of three", []string{"write window failed", "a write of an erasure did not commit"},
			func(t *testing.T, f *sweepFixture, id string, fail error) {
				w, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				var commits int
				var mu sync.Mutex
				parked, release := make(chan struct{}), make(chan struct{})
				w.beforeCommit = func() {
					mu.Lock()
					commits++
					first := commits == 1
					mu.Unlock()
					if first {
						close(parked)
						<-release
					}
				}
				answers := make(chan error, 3)
				go func() { answers <- w.Submit(context.Background(), &erasureSweep{}) }()
				<-parked
				go func() { answers <- w.Submit(context.Background(), hostileJob{err: fail}) }()
				go func() { answers <- w.Submit(context.Background(), &erasureSweep{}) }()
				waitFor(t, func() bool { return len(w.queue) == 2 })
				close(release)
				for range 3 {
					<-answers
				}
			}},
	}
}

// hostileJob fails inside the writer's transaction with an error of its own
// words, as a job of an erasure's that redacts its failure.
type hostileJob struct{ err error }

func (j hostileJob) apply(*sql.Tx) error { return j.err }
func (hostileJob) failureRedacted() (string, bool) {
	return "4f0c9d3e8a1b2c3d4e5f60718293a4b5", true
}

// conflictsThenFails answers the first scrub with a conflict whose words are
// hostile, and every scrub after with an error that is.
type conflictsThenFails struct {
	jobSubmitter
	err   error
	scrub int
}

func (w *conflictsThenFails) Submit(ctx context.Context, job WriteJob) error {
	switch job.(type) {
	case *RawScrub:
		w.scrub++
		if w.scrub == 1 {
			return &Rejection{Kind: RejectConflict, Message: w.err.Error()}
		}
		return w.err
	case *erasureStep:
		if job.(*erasureStep).TailFailure != "" {
			return w.err
		}
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// stopsThenRefusesPause stops the worker at the first chunk, and refuses the
// pause that gives the start back.
type stopsThenRefusesPause struct {
	jobSubmitter
	stop context.CancelFunc
	err  error
}

func (w *stopsThenRefusesPause) Submit(ctx context.Context, job WriteJob) error {
	switch job.(type) {
	case *UserDataErase:
		w.stop()
		return ctx.Err()
	case *erasurePause:
		return w.err
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// The user an erasure erases, and the words of the errors it meets, never
// reach its log lines (#32). Each path, each id, an error of plain words and
// one that is SQLite's.
func TestNoErasureLineQuotesAnErrorOrItsUser(t *testing.T) {
	was := busyWait
	busyWait = 20 * time.Millisecond
	t.Cleanup(func() { busyWait = was })
	for _, path := range erasureLogPaths() {
		for n, id := range hostileIDs() {
			for _, coded := range []bool{false, true} {
				name := fmt.Sprintf("%s/id %d/coded %v", path.name, n, coded)
				t.Run(name, func(t *testing.T) {
					var logged bytes.Buffer
					var mu sync.Mutex
					old := logger
					logger = func() *slog.Logger {
						return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil))
					}
					t.Cleanup(func() { logger = old })
					f := newErasureFixture(t)
					fail := hostileError(id)
					if coded {
						fail = fmt.Errorf("%w: %w", fail, codedError{11})
					}
					path.run(t, f, id, fail)

					mu.Lock()
					out := logged.String()
					mu.Unlock()
					for _, message := range path.message {
						if !strings.Contains(out, message) {
							t.Errorf("the log has no %q: %q", message, out)
						}
					}
					if found := leaks(out, id); len(found) > 0 {
						t.Errorf("the log gives %s:\n%s", strings.Join(found, ", "), out)
					}
				})
			}
		}
	}
}
