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
// #33): an error's text may quote what it holds, an erasure's user among it,
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

// waitLine waits for a line of the log that a goroutine writes.
func waitLine(t *testing.T, log func() string, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(log(), message) {
		if time.Now().After(deadline) {
			t.Fatalf("the log has no %q: %q", message, log())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// erasureLogPath is one way an erasure's failure reaches the log: it runs
// and answers the message the log must hold, so that a path that logs nothing
// does not pass for one that leaks nothing.
type erasureLogPath struct {
	name    string
	message []string
	// once is a path that takes nothing from the id or the error, which one
	// run of stands for all.
	once bool
	// run takes the log as a function, for a path whose line a goroutine
	// writes and that must not stop that goroutine before it has.
	run func(t *testing.T, f *sweepFixture, id string, fail error, log func() string)
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
		{"the raw phase", []string{"the raw phase of an erasure failed", "an erasure failed"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				e := f.startErasure(t, id)
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { step, ok := job.(*erasureStep); return ok && step.TracesAtStart != nil }}
				if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
					t.Fatal(err)
				}
			}},
		{"the parsed phase", []string{"the parsed phase of an erasure failed", "an erasure failed"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				e := f.startErasure(t, id)
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { _, chunk := job.(*UserDataErase); return chunk }}
				if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
					t.Fatal(err)
				}
			}},
		{"the tail and its unrecorded cause", []string{"the tail of an erasure failed", "a failed tail's cause is not recorded"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
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
		{"a scrub retried after a conflict", []string{"the tail of an erasure failed"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				tailOf(t, f, id, func(inner jobSubmitter) jobSubmitter {
					return &conflictsThenFails{jobSubmitter: inner, err: fail}
				})
			}},
		{"a batch that cannot be read", []string{"could not be read to scrub it"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
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
		{"the worker", []string{"an erasure stopped before it ended"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { _, end := job.(*erasureEnd); return end }}
				worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
				worker.Start()
				defer worker.Close()
				f.startErasure(t, id)
				waitLine(t, log, "an erasure stopped before it ended")
			}},
		{"a batch deleted whole for want of a rewrite", []string{"deleting it whole"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "someone-else", "", "a", nil)}), false, daysAgo(1))
				batch := f.count(t, `SELECT MAX(id) FROM raw_batches`)
				saved := scrubRewrite
				scrubRewrite = func(*mapping.ExportBody, map[string]bool) ([]byte, error) { return nil, fail }
				t.Cleanup(func() { scrubRewrite = saved })
				e := &Erasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5", ProjectID: f.project.ID, UserID: id}
				plan, err := f.store.planScrub(t.Context(), e, batch, map[string]bool{hexTrace(1): true})
				if err != nil || plan == nil || !plan.job.Delete {
					t.Fatalf("the batch was not planned to be deleted whole: %v, %v", plan, err)
				}
			}},
		{"a start that fails", []string{"an erasure stopped before it ended"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				writer := &failingJobs{jobSubmitter: f.writer, err: fail,
					fails: func(job WriteJob) bool { _, begin := job.(*erasureBegin); return begin }}
				worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
				worker.Start()
				defer worker.Close()
				f.startErasure(t, id)
				waitLine(t, log, "an erasure stopped before it ended")
			}},
		{"the worker that cannot read", []string{"could not read the next erasure"}, true,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				// The driver's own words: a table it cannot find.
				if _, err := f.store.db.Exec(`ALTER TABLE erasures RENAME TO erasures_away`); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = f.store.db.Exec(`ALTER TABLE erasures_away RENAME TO erasures`) })
				worker := f.store.NewEraser(f.writer, EraserOptions{Poll: time.Hour})
				worker.Start()
				defer worker.Close()
				waitLine(t, log, "could not read the next erasure")
			}},
		{"a stop whose pause is refused", []string{"a stopped erasure's start stays counted"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				e := f.startErasure(t, id)
				writer := &stopsThenRefusesPause{jobSubmitter: f.writer, stop: cancel, err: fail}
				if _, err := f.store.runErasure(ctx, writer, e.ID, EraserOptions{}); err == nil {
					t.Fatal("the run was not stopped")
				}
			}},
		{"a write whose submitter left", []string{"a write of an erasure did not commit"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
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
				// The writer commits, fails and logs after the submitter has
				// left: the test waits for the line.
				waitLine(t, log, "a write of an erasure did not commit")
			}},
		{"a window of three", []string{"a write of an erasure did not commit"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
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
				go func() { answers <- w.Submit(context.Background(), sharesAWindow()) }()
				<-parked
				go func() { answers <- w.Submit(context.Background(), hostileJob{err: fail}) }()
				go func() { answers <- w.Submit(context.Background(), sharesAWindow()) }()
				waitFor(t, func() bool { return len(w.queue) == 2 })
				close(release)
				for range 3 {
					<-answers
				}
				// The window is applied again with a savepoint per job, and
				// the job that failed is refused there: its line, and no
				// line of a window that came apart, which did not.
				waitLine(t, log, "a write of an erasure did not commit")
				if out := log(); strings.Contains(out, "write window failed") {
					t.Errorf("a window a job of an erasure's failed by itself is said to have come apart:\n%s", out)
				}
			}},
		{"a job replayed after its window's first pass", []string{"a job failed its window's first pass and passed the second"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				w, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				// A job of an erasure's that fails the window's first pass and
				// passes the second is committed and refused nothing: the
				// replay line is the only one it leaves.
				if errs := runWindow(t, w, sharesAWindow(), &failsOnce{err: fail}, sharesAWindow()); errors.Join(errs...) != nil {
					t.Fatalf("answers %v, want every job committed by the second pass", errs)
				}
				waitLine(t, log, "a job failed its window's first pass and passed the second")
			}},
		{"a window a job broke", []string{"write window failed", "a write of an erasure did not commit"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				w, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close()
				// A job that rolls its transaction back itself leaves the
				// second pass no savepoint to return to: the window comes
				// apart, and is retried job by job.
				runWindow(t, w, sharesAWindow(), rollsBackItself{err: fail}, sharesAWindow())
				waitLine(t, log, "write window failed")
			}},
		{"a caller that wraps the writer's error and logs it",
			[]string{"a caller printed the writer's error", "a caller logged the writer's error"}, false,
			func(t *testing.T, f *sweepFixture, id string, fail error, log func() string) {
				err := f.writer.Submit(t.Context(), hostileJob{err: fail})
				if err == nil {
					t.Fatal("the job did not fail")
				}
				// A wrap above the writer's answer prints the wrap's text, not
				// the failure's value (#36): through slog as it is, and
				// through the writer's own line.
				wrapped := fmt.Errorf("the caller: %w", err)
				logger().Error("a caller printed the writer's error", "err", wrapped, "text", wrapped.Error())
				logFailure(wrapped, slog.LevelError, "a caller logged the writer's error")
			}},
	}
}

// rollsBackItself is a job of an erasure's that ends the writer's transaction
// itself and fails with an error of its own words.
type rollsBackItself struct{ err error }

func (j rollsBackItself) apply(tx *sql.Tx) error {
	_, _ = tx.Exec(`ROLLBACK`)
	return j.err
}
func (rollsBackItself) failureRedacted() (string, bool) {
	return "4f0c9d3e8a1b2c3d4e5f60718293a4b5", true
}

// failsOnce is a job of an erasure's that fails its first application with an
// error of its own words and passes every one after.
type failsOnce struct {
	err   error
	calls int
}

func (j *failsOnce) apply(*sql.Tx) error {
	if j.calls++; j.calls == 1 {
		return j.err
	}
	return nil
}
func (*failsOnce) failureRedacted() (string, bool) {
	return "4f0c9d3e8a1b2c3d4e5f60718293a4b5", true
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
// reach its log lines (#33). Each path, each id, an error of plain words and
// one that is SQLite's.
func TestNoErasureLineQuotesAnErrorOrItsUser(t *testing.T) {
	was := busyWait
	busyWait = 20 * time.Millisecond
	t.Cleanup(func() { busyWait = was })
	for _, path := range erasureLogPaths() {
		for n, id := range hostileIDs() {
			for _, coded := range []bool{false, true} {
				if path.once && (n > 0 || coded) {
					continue
				}
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
					path.run(t, f, id, fail, func() string {
						mu.Lock()
						defer mu.Unlock()
						return logged.String()
					})

					// The lines a goroutine writes are waited for, not slept on.
					var out string
					deadline := time.Now().Add(3 * time.Second)
					for {
						mu.Lock()
						out = logged.String()
						mu.Unlock()
						missing := ""
						for _, message := range path.message {
							if !strings.Contains(out, message) {
								missing = message
							}
						}
						if missing == "" {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("the log has no %q: %q", missing, out)
						}
						time.Sleep(5 * time.Millisecond)
					}
					if found := leaks(out, id); len(found) > 0 {
						t.Errorf("the log gives %s:\n%s", strings.Join(found, ", "), out)
					}
				})
			}
		}
	}
}

// runWindow submits jobs to a writer in one window, the first parked in its
// commit so that the rest queue behind it, and answers what each returned.
func runWindow(t *testing.T, w *Writer, first WriteJob, rest ...WriteJob) []error {
	t.Helper()
	var commits int
	var mu sync.Mutex
	parked, release := make(chan struct{}), make(chan struct{})
	w.beforeCommit = func() {
		mu.Lock()
		commits++
		leading := commits == 1
		mu.Unlock()
		if leading {
			close(parked)
			<-release
		}
	}
	answers := make(chan error, len(rest)+1)
	go func() { answers <- w.Submit(context.Background(), first) }()
	<-parked
	for _, job := range rest {
		go func() { answers <- w.Submit(context.Background(), job) }()
	}
	waitFor(t, func() bool { return len(w.queue) == len(rest) })
	close(release)
	out := make([]error, 0, len(rest)+1)
	for range len(rest) + 1 {
		out = append(out, <-answers)
	}
	return out
}

// The levels of what an erasure writes hold (#33): a refusal of the writer is
// Info, as it has always been; the refusal of a project that is gone is Info
// on the erasure's own line; a conflict that ends the erasure failed, and
// anything else, is an Error — a window that came apart, a Warn. A job that
// fails inside a window is refused there with its own line, at the same level
// as alone; only a job that takes the window's transaction with it comes it
// apart.
func TestTheLevelsOfAnErasuresLines(t *testing.T) {
	refusal := &Rejection{Kind: RejectInvalid, Message: "words"}
	broken := errors.New("words")
	for _, tc := range []struct {
		name   string
		fail   error
		window bool
		want   string
	}{
		{"a refused write", refusal, false, "level=INFO msg=\"a write of an erasure did not commit\""},
		{"a broken write", broken, false, "level=ERROR msg=\"a write of an erasure did not commit\""},
		{"a refused write in a window", refusal, true, "level=INFO msg=\"a write of an erasure did not commit\""},
		{"a broken write in a window", broken, true, "level=ERROR msg=\"a write of an erasure did not commit\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newErasureFixture(t)
			var logged bytes.Buffer
			var mu sync.Mutex
			old := logger
			logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
			t.Cleanup(func() { logger = old })
			w, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if tc.window {
				runWindow(t, w, sharesAWindow(), hostileJob{err: tc.fail}, sharesAWindow())
			} else if err := w.Submit(t.Context(), hostileJob{err: tc.fail}); err == nil {
				t.Fatal("the job did not fail")
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(logged.String(), tc.want) {
				t.Errorf("the log %q, want %q", logged.String(), tc.want)
			}
		})
	}
}

// A window that a job of an erasure's takes down by ending the transaction
// itself is a Warn, without the job's words and naming the erasure it was a
// step of (spec 047 #33, #35): its cause is the savepoint the job left no way
// back to, not the job's own failure. The job's own failure is a line of its
// own, an Info when it is a refusal and an Error when it is not, as alone.
func TestAWindowAnErasureJobTookDownIsAWarn(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail error
		job  string
	}{
		{"a broken job", errors.New("words of the user"), "level=ERROR"},
		{"a refusing job", &Rejection{Kind: RejectInvalid, Message: "words of the user"}, "level=INFO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newErasureFixture(t)
			log := captureLog(t)
			w, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			runWindow(t, w, sharesAWindow(), rollsBackItself{err: tc.fail}, sharesAWindow())
			waitLine(t, log, "a write of an erasure did not commit")
			out := log()
			const erasure = "erasure=4f0c9d3e8a1b2c3d4e5f60718293a4b5"
			var window, job string
			for _, line := range strings.Split(out, "\n") {
				switch {
				case strings.Contains(line, "write window failed"):
					window = line
				case strings.Contains(line, "a write of an erasure did not commit"):
					job = line
				}
			}
			if !strings.Contains(window, "level=WARN") || !strings.Contains(window, erasure) {
				t.Errorf("the line of the window %q, want a Warn naming the erasure", window)
			}
			if !strings.Contains(job, tc.job) || !strings.Contains(job, erasure) {
				t.Errorf("the line of the job %q, want %s naming the erasure", job, tc.job)
			}
			if strings.Contains(out, "words of the user") {
				t.Errorf("the log carries the job's words:\n%s", out)
			}
		})
	}
}

// A tail that fails is said by the tail's own line, and the worker does not
// say it again (#33): the erasure's line, and the writer's for the write.
func TestATailFailureIsNotSaidAgainByTheWorker(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })
	worker := f.store.NewEraser(&brokenTail{jobSubmitter: f.writer}, EraserOptions{Poll: time.Hour,
		after: func(step int) error {
			if step == 1 {
				f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 9, "user-a", "", "late", nil)}), false, 0)
			}
			return nil
		}})
	worker.Start()
	f.startErasure(t, "user-a")
	waitLine(t, func() string { mu.Lock(); defer mu.Unlock(); return logged.String() },
		"the tail of an erasure failed")
	// Close waits for the worker to go past the run that failed.
	worker.Close()
	mu.Lock()
	defer mu.Unlock()
	out := logged.String()
	if strings.Count(out, "the tail of an erasure failed") != 1 || strings.Contains(out, "an erasure stopped before it ended") {
		t.Errorf("the log %q, want the tail's failure said once and not again by the worker", out)
	}
}

// A start that fails says where the run stopped: the start (#33).
func TestAFailedStartIsSaidWithItsPhase(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })
	writer := &failingJobs{jobSubmitter: f.writer, err: errors.New("the start could not be written"),
		fails: func(job WriteJob) bool { _, begin := job.(*erasureBegin); return begin }}
	worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
	worker.Start()
	defer worker.Close()
	f.startErasure(t, "user-a")
	log := func() string { mu.Lock(); defer mu.Unlock(); return logged.String() }
	waitLine(t, log, "an erasure stopped before it ended")
	if out := log(); !strings.Contains(out, "phase=start") {
		t.Errorf("the worker's line %q has no phase=start", out)
	}
}

// A decoder's failure names the erasure whose scrub met it (#33).
func TestAScrubsLinesNameTheErasure(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	seedRawBatch(t, f.store, f.project.ID, &RawBatch{ReceivedAt: daysAgo(1), ContentType: mapping.ContentTypeJSON,
		Body: []byte("{not an export")})
	batch := f.count(t, `SELECT MAX(id) FROM raw_batches`)
	e := &Erasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5", ProjectID: f.project.ID, UserID: "user-a"}
	if _, err := f.store.planScrub(t.Context(), e, batch, map[string]bool{hexTrace(1): true}); err != nil {
		t.Fatal(err)
	}
	if out := logged.String(); !strings.Contains(out, "erasure=4f0c9d3e8a1b2c3d4e5f60718293a4b5") ||
		!strings.Contains(out, "batch=") {
		t.Errorf("the scrub's line %q does not name the erasure and the batch", out)
	}
}

// A job's failure says its facts wherever it goes (#36): wrapped above, the
// writer's line still gives the erasure, the cause and the kind of the refusal,
// and its text is a sentence of the erasure and the cause, never the error's
// words. errors.Is and errors.As still reach what failed, which is how every
// classifier of a failure reads it; a job that does not redact is its own error.
func TestAJobFailureSaysItsFactsWhereverItGoes(t *testing.T) {
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	const erasure = "4f0c9d3e8a1b2c3d4e5f60718293a4b5"

	inner := &Rejection{Kind: RejectInvalid, Message: "words of user-4711"}
	wrapped := fmt.Errorf("the caller: %w", failedAt(hostileJob{}, inner))
	var rejection *Rejection
	if !errors.As(wrapped, &rejection) || rejection != inner || !rejected(wrapped) {
		t.Errorf("the wrap of %v does not reach the refusal it carries", wrapped)
	}
	if text := wrapped.Error(); strings.Contains(text, "4711") || !strings.Contains(text, "erasure "+erasure) ||
		!strings.Contains(text, causeOther) {
		t.Errorf("the text %q, want the erasure and the cause without the error's words", text)
	}
	logFailure(wrapped, slog.LevelError, "a line")
	out := logged.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "erasure="+erasure) ||
		!strings.Contains(out, "refusal=invalid") || strings.Contains(out, "4711") || strings.Contains(out, "the caller") {
		t.Errorf("the line %q, want the refusal's facts at Info, not the wrap's text", out)
	}

	full := failedAt(hostileJob{}, fmt.Errorf("write for user-4711: %w", codedError{13}))
	if code, ok := sqliteCode(full); !ok || code != 13 || failureCause(full) != sqliteCauses[13] {
		t.Errorf("the failure %v lost its code", full)
	}
	if value := full.(slog.LogValuer).LogValue(); value.Kind() != slog.KindString || strings.Contains(value.String(), erasure) {
		t.Errorf("a condition says %v, want its name alone", value)
	}

	if err := failedAt(sharesAWindow(), inner); err != error(inner) {
		t.Errorf("a job that does not redact answers %v, want its own error", err)
	}
}
