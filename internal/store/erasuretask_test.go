package store

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/logpace"
)

// An erasure is a task (spec 047): recorded, run by one worker, resumed after
// a stop, forgetting the person when it ends.

// A second request for a user whose erasure is queued or running answers that
// erasure; once it has ended, a request records a new one (#11).
func TestASecondRequestAnswersTheRunningErasure(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	first := f.startErasure(t, "user-a")
	second := f.startErasure(t, "user-a")
	if second.ID != first.ID {
		t.Errorf("a second request recorded %s beside %s", second.ID, first.ID)
	}
	// Another user of the project, and the same user of another project,
	// are erasures of their own.
	other := f.startErasure(t, "user-b")
	elsewhere, err := f.store.CreateProject("elsewhere", KeyPair{PublicKey: "tp-pk-else", Secret: "tp-sk-else"})
	if err != nil {
		t.Fatal(err)
	}
	there, err := f.store.StartErasure(t.Context(), f.writer, UserErasure{ProjectID: elsewhere.ID,
		UserID: "user-a", Confirm: "user-a"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == first.ID || there.ID == first.ID {
		t.Errorf("another user's or another project's erasure is %s, %s: the first's", other.ID, there.ID)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM erasures`); n != 3 {
		t.Errorf("%d erasures recorded, want 3", n)
	}

	if _, err := f.store.runErasure(t.Context(), f.writer, first.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if third := f.startErasure(t, "user-a"); third.ID == first.ID || third.State != ErasureQueued {
		t.Errorf("a request after the end answered %s (%s), want a new queued erasure", third.ID, third.State)
	}
}

// The record forgets the person when the erasure ends, done or failed (#9):
// no user id in the row, no tail, and a failure said as its phase and a cause
// from a fixed list, never the error's own text, whatever it held (#32). The
// server's log has the failure's cause, types and position, and no text
// either.
func TestAnEndedErasureForgetsTheUser(t *testing.T) {
	const unexpected = "the parsed phase failed: an unexpected error, whose type the server's log has"
	for _, tc := range []struct {
		name, user string
		fail       error
		want       string
	}{
		{name: "done"},
		// Words that read like a cause are still only the error's.
		{name: "failed with any text", fail: errors.New("the disk is full"), want: unexpected},
		{name: "failed with a condition", fail: fmt.Errorf("commit: %w", codedError{13}),
			want: "the parsed phase failed: the disk is full"},
		{name: "failed naming the user", user: "user-a", fail: errors.New(`a chunk refused "user-a" twice`),
			want: unexpected},
		{name: "failed quoting the user", user: `we"ird`, fail: fmt.Errorf("a chunk refused %q", `we"ird`),
			want: unexpected},
		{name: "failed escaping the user", user: "zoë/7", fail: fmt.Errorf("GET /users/%s: refused",
			url.PathEscape("zoë/7")), want: unexpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was := busyWait
			busyWait = 50 * time.Millisecond
			t.Cleanup(func() { busyWait = was })
			var logged bytes.Buffer
			old := logger
			logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
			t.Cleanup(func() { logger = old })
			f := newErasureFixture(t)
			user := cmp.Or(tc.user, "user-a")
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, user, "", "a", nil)}), false, daysAgo(1))
			var writer jobSubmitter = f.writer
			if tc.fail != nil {
				// Every try of the chunks: a condition is retried (#16).
				writer = &failingJobs{jobSubmitter: f.writer, err: tc.fail,
					fails: func(job WriteJob) bool { _, chunk := job.(*UserDataErase); return chunk }}
			}
			e := f.startErasure(t, user)
			if e.UserID != user {
				t.Fatalf("a queued erasure is of %q, want the user", e.UserID)
			}
			if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
				t.Fatal(err)
			}
			got := f.erasureOf(t, user)
			if got.UserID != "" || got.Phase != "" || got.FinishedAt == 0 {
				t.Errorf("ended, the erasure is of %q in %q, finished at %d", got.UserID, got.Phase, got.FinishedAt)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM erasures WHERE user_id IS NOT NULL`); n != 0 {
				t.Errorf("%d erasures still name a user", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM erasure_tail`); n != 0 {
				t.Errorf("%d tail rows are left", n)
			}
			if tc.fail == nil {
				return
			}
			if got.State != ErasureFailed || got.Error != tc.want {
				t.Errorf("the erasure is %s with %q, want failed with %q", got.State, got.Error, tc.want)
			}
			// The log says what failed and where, once, and not the text.
			out := logged.String()
			if strings.Count(out, "the parsed phase of an erasure failed") != 1 || !strings.Contains(out, "phase=parsed") ||
				!strings.Contains(out, "types=") || strings.Contains(out, tc.fail.Error()) ||
				strings.Contains(out, strconv.Quote(tc.fail.Error())) {
				t.Errorf("the log %q, want the parsed phase's failure once, with its types and not its text", out)
			}
		})
	}
}

// Finished erasures are kept 30 days from their end; a running one of any age
// stays (#15).
func TestFinishedErasuresGoAfterThirtyDays(t *testing.T) {
	f := newErasureFixture(t)
	old := f.startErasure(t, "user-a")
	if _, err := f.store.runErasure(t.Context(), f.writer, old.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	recent := f.startErasure(t, "user-b")
	if _, err := f.store.runErasure(t.Context(), f.writer, recent.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	running := f.startErasure(t, "user-c")
	day := int64(24 * time.Hour)
	for id, finished := range map[string]int64{old.ID: sweepNow.UnixNano() - 31*day, recent.ID: sweepNow.UnixNano() - 29*day} {
		if _, err := f.store.db.Exec(`UPDATE erasures SET finished_at = ? WHERE id = ?`, finished, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.Exec(`UPDATE erasures SET created_at = ? WHERE id = ?`,
		sweepNow.UnixNano()-90*day, running.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	left, err := queryColumn[string](f.store.db, `SELECT id FROM erasures ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0] != running.ID || left[1] != recent.ID {
		t.Errorf("after the pass %v are left, want the running one and the recent one", left)
	}
}

// A job that fails with a database condition is retried until it passes
// (#16): a full disk that is freed does not fail the erasure.
func TestAnErasureRetriesADatabaseCondition(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	full := 0
	writer := &failingJobs{jobSubmitter: f.writer, fails: func(job WriteJob) bool {
		_, chunk := job.(*UserDataErase)
		if chunk && full < 2 {
			full++
			return true
		}
		return false
	}, err: codedError{code: sqliteFull}}
	e := f.startErasure(t, "user-a")
	if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureDone || got.Counts.Traces != 1 || full != 2 {
		t.Errorf("the erasure ended %s (%q) with %d traces after %d full disks, want done with one",
			got.State, got.Error, got.Counts.Traces, full)
	}
}

// The worker (#10, #12): at its start it takes the erasure a stop left
// running before any queued one, and then each erasure as it is recorded; a
// caller waiting for one hears it end.
func TestTheWorkerRunsTheInterruptedErasureFirst(t *testing.T) {
	f := newErasureFixture(t)
	for i, user := range []string{"user-a", "user-b", "user-c"} {
		f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1+i, 1, user, "", "x", nil)}), false, daysAgo(1))
	}
	queued := f.startErasure(t, "user-a")
	interrupted := f.startErasure(t, "user-b")
	stop := errors.New("stopped")
	if _, err := f.store.runErasure(t.Context(), f.writer, interrupted.ID, EraserOptions{
		after: func(int) error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("the run was not stopped: %v", err)
	}

	worker := f.store.NewEraser(f.writer, EraserOptions{})
	worker.Start()
	defer worker.Close()
	for _, id := range []string{queued.ID, interrupted.ID} {
		if e, err := f.store.AwaitErasure(t.Context(), f.project.ID, id, 10*time.Second); err != nil ||
			e == nil || e.State != ErasureDone {
			t.Fatalf("erasure %s: %+v, %v; want done", id, e, err)
		}
	}
	first, _ := f.store.Erasure(t.Context(), f.project.ID, interrupted.ID)
	second, _ := f.store.Erasure(t.Context(), f.project.ID, queued.ID)
	if second.StartedAt < first.FinishedAt {
		t.Errorf("the queued erasure started at %d, before the interrupted one ended at %d",
			second.StartedAt, first.FinishedAt)
	}

	// Recorded while the worker waits: it is woken, not polled for.
	late := f.startErasure(t, "user-c")
	e, err := f.store.AwaitErasure(t.Context(), f.project.ID, late.ID, 10*time.Second)
	if err != nil || e == nil || e.State != ErasureDone {
		t.Fatalf("an erasure recorded while the worker waited: %+v, %v; want done", e, err)
	}
}

// A stop interrupts the worker and does not wait for the erasure (#17): the
// worker returns at once, the erasure stays running for the next start, and
// a caller waiting for it is answered.
func TestAStopLeavesTheErasureRunning(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	held := make(chan struct{})
	writer := &holdingChunk{jobSubmitter: f.writer, held: held}
	worker := f.store.NewEraser(writer, EraserOptions{})
	worker.Start()
	e := f.startErasure(t, "user-a")
	<-held

	waited := make(chan *Erasure)
	go func() {
		got, _ := f.store.AwaitErasure(context.Background(), f.project.ID, e.ID, time.Minute)
		waited <- got
	}()
	began := time.Now()
	worker.Close()
	if took := time.Since(began); took > time.Second {
		t.Errorf("the stop took %v", took)
	}
	select {
	case got := <-waited:
		if got == nil || got.State != ErasureRunning || got.Phase != phaseParsed {
			t.Errorf("the waiting caller was told %+v, want the erasure running in its parsed phase", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting caller was not answered when the worker stopped")
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureRunning || got.UserID != "user-a" {
		t.Errorf("after the stop the erasure is %s of %q, want running, still the user's", got.State, got.UserID)
	}
}

// holdingChunk holds the parsed phase's first chunk until its caller gives
// up, the way a writer busy with a long commit does; held says it has one.
type holdingChunk struct {
	jobSubmitter
	held chan struct{}
}

func (w *holdingChunk) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*UserDataErase); ok {
		close(w.held)
		<-ctx.Done()
		return ctx.Err()
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// A clean stop gives the start back (#27): an erasure longer than a few
// deploys is resumed by each start after them, not failed by the fourth.
func TestACleanStopDoesNotCountAsAnInterruption(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	e := f.startErasure(t, "user-a")
	for range erasureAttempts + 2 {
		ctx, cancel := context.WithCancel(t.Context())
		writer := &stopOnChunk{jobSubmitter: f.writer, stop: cancel}
		if _, err := f.store.runErasure(ctx, writer, e.ID, EraserOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("the run was not stopped: %v", err)
		}
		cancel()
		if got := f.erasureOf(t, "user-a"); got.attempts != 0 || got.State != ErasureRunning {
			t.Fatalf("after a clean stop the erasure is %s with %d starts counted, want running and none",
				got.State, got.attempts)
		}
	}
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureDone || got.Counts.Traces != 1 {
		t.Errorf("the erasure ended %s (%q) with %d traces, want done with one", got.State, got.Error, got.Counts.Traces)
	}
}

// stopOnChunk is the worker's stop arriving while a chunk is submitted: the
// context ends, and the chunk with it.
type stopOnChunk struct {
	jobSubmitter
	stop context.CancelFunc
}

func (w *stopOnChunk) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*UserDataErase); ok {
		w.stop()
		return ctx.Err()
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// The freeze clock goes forward with the chunks (#27): an erasure recorded long
// ago judges a swept hour by now, not by the day it was recorded; one whose
// stored clock is ahead of the wall's keeps it.
func TestAChunkFreezesByTheLaterOfTheTwoClocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored int64
		ahead  bool
	}{
		{name: "recorded long ago", stored: 1},
		{name: "a clock that went back", stored: time.Now().Add(time.Hour).UnixNano(), ahead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newErasureFixture(t)
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
			e := f.startErasure(t, "user-a")
			if _, err := f.store.db.Exec(`UPDATE erasures SET now = ? WHERE id = ?`, tc.stored, e.ID); err != nil {
				t.Fatal(err)
			}
			writer := &clockOfChunks{jobSubmitter: f.writer}
			before := time.Now().UnixNano()
			if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
				t.Fatal(err)
			}
			switch {
			case len(writer.clocks) == 0:
				t.Fatal("no chunk ran")
			case tc.ahead && writer.clocks[0] != tc.stored:
				t.Errorf("the chunk froze by %d, want the stored clock %d, which is later", writer.clocks[0], tc.stored)
			case !tc.ahead && writer.clocks[0] < before:
				t.Errorf("the chunk froze by %d, before the run began at %d", writer.clocks[0], before)
			}
		})
	}
}

// clockOfChunks records the freeze clock of every chunk it passes on.
type clockOfChunks struct {
	jobSubmitter
	clocks []int64
}

func (w *clockOfChunks) Submit(ctx context.Context, job WriteJob) error {
	if chunk, ok := job.(*UserDataErase); ok {
		w.clocks = append(w.clocks, chunk.Now)
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// A tail that fails leaves the erasure running in its tail, with the windows
// it has yet to scrub, for the next start (#28): ending it would drop them,
// and the batches they name hold spans of traces already gone.
func TestAFailedTailIsLeftToTheNextStart(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil),
		otlpSpan(t, 2, 1, "user-b", "", "b", nil)}), false, daysAgo(1))
	var late int64
	e := f.startErasure(t, "user-a")
	broken := &brokenTail{jobSubmitter: f.writer}
	_, err := f.store.runErasure(t.Context(), broken, e.ID, EraserOptions{after: func(step int) error {
		if step == 1 {
			late = f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 9, "user-a", "", "late", nil),
				otlpSpan(t, 2, 9, "user-b", "", "b", nil)}), false, 0)
		}
		return nil
	}})
	if err == nil || !broken.failed {
		t.Fatalf("a run whose tail failed answered %v (the tail failed: %v)", err, broken.failed)
	}
	got := f.erasureOf(t, "user-a")
	if got.State != ErasureRunning || got.Phase != phaseTail || got.UserID != "user-a" {
		t.Fatalf("after its tail failed the erasure is %s in %q of %q, want running in its tail", got.State,
			got.Phase, got.UserID)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM erasure_tail`); n == 0 {
		t.Fatal("the tail's windows were dropped")
	}
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureDone {
		t.Errorf("the next start ended it %s (%q), want done", got.State, got.Error)
	}
	if spans := f.rawSpans(t, late); !slices.Equal(spans, []string{"span-2-9"}) {
		t.Errorf("the late batch holds %v, want only the other user's span", spans)
	}
}

// brokenTail fails the scrubs that come after the last chunk, the way a disk
// that went bad mid-erasure fails them.
type brokenTail struct {
	jobSubmitter
	parsed, failed bool
	err            error
}

func (w *brokenTail) Submit(ctx context.Context, job WriteJob) error {
	if _, scrub := job.(*RawScrub); scrub && w.parsed {
		w.failed = true
		return cmp.Or(w.err, errors.New("the disk is broken"))
	}
	err := w.jobSubmitter.Submit(ctx, job)
	if chunk, ok := job.(*UserDataErase); ok && err == nil && !chunk.More {
		w.parsed = true
	}
	return err
}

// A run that fails is not taken again at the next wake (#28): a request for
// another user wakes the worker, and it must not spend the failed erasure's
// starts on the same failure, nor hold the queue behind it.
func TestAFailedRunIsNotRetakenOnEveryWake(t *testing.T) {
	f := newErasureFixture(t)
	writer := &failingJobs{jobSubmitter: f.writer, err: errors.New("the disk is broken"),
		fails: func(job WriteJob) bool { _, end := job.(*erasureEnd); return end }}
	worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
	worker.Start()
	defer worker.Close()
	e := f.startErasure(t, "user-a")
	waitFor(t, func() bool {
		got := f.erasureOf(t, "user-a")
		return got.attempts == 1 && got.Phase == phaseTail
	})
	time.Sleep(50 * time.Millisecond)
	for _, user := range []string{"user-b", "user-c", "user-d"} {
		if _, err := f.store.StartErasure(t.Context(), f.writer, UserErasure{ProjectID: f.project.ID,
			UserID: user, Confirm: user}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, err := f.store.Erasure(t.Context(), f.project.ID, e.ID)
	if err != nil || got.attempts != 1 {
		t.Errorf("after three wakes the failed erasure has %d starts (%v), want its one", got.attempts, err)
	}
}

// A stop that ends the wait for the start while the writer still commits it
// gives the start back too (#28).
func TestAStopWhileTheStartIsQueuedGivesItBack(t *testing.T) {
	f := newErasureFixture(t)
	e := f.startErasure(t, "user-a")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &stopAfterBegin{jobSubmitter: f.writer, stop: cancel}
	if _, err := f.store.runErasure(ctx, writer, e.ID, EraserOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the run was not stopped: %v", err)
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureRunning || got.attempts != 0 {
		t.Errorf("the erasure is %s with %d starts counted, want running and none", got.State, got.attempts)
	}
}

// stopAfterBegin commits the start and then answers as a stop that ended the
// wait for it does.
type stopAfterBegin struct {
	jobSubmitter
	stop context.CancelFunc
}

func (w *stopAfterBegin) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*erasureBegin); ok {
		if err := w.jobSubmitter.Submit(context.WithoutCancel(ctx), job); err != nil {
			return err
		}
		w.stop()
		return ctx.Err()
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// The listing puts erasures under way first (#28): one that waits behind a
// hundred newer records is still in it.
func TestTheListingPutsErasuresUnderWayFirst(t *testing.T) {
	f := newErasureFixture(t)
	waiting := f.startErasure(t, "user-waiting")
	for i := range ErasureListed + 1 {
		user := fmt.Sprintf("user-%d", i)
		done := f.startErasure(t, user)
		if _, err := f.store.runErasure(t.Context(), f.writer, done.ID, EraserOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := f.store.Erasures(t.Context(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != ErasureListed || listed[0].ID != waiting.ID || listed[1].CreatedAt < listed[2].CreatedAt {
		t.Errorf("the listing has %d, first %s, want %d with the queued %s first and the rest newest first",
			len(listed), listed[0].ID, ErasureListed, waiting.ID)
	}
}

// An erasure that ends failed is logged as a failure, even from a run that met
// no failure of its own: the one an earlier start recorded (#28).
func TestAnErasureThatEndsFailedIsLoggedAsOne(t *testing.T) {
	f := newErasureFixture(t)
	e := f.startErasure(t, "user-a")
	if err := f.writer.Submit(t.Context(), &erasureBegin{ID: e.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), &erasureStep{ID: e.ID, Phase: phaseTail, Error: "the disk is broken"}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.erasureOf(t, "user-a"); got.State != ErasureFailed {
		t.Fatalf("the erasure ended %s, want failed", got.State)
	}
	if out := logged.String(); !strings.Contains(out, "an erasure failed") || strings.Contains(out, "erased a user's data") {
		t.Errorf("the log says %q, want the failure", out)
	}
}

// A tail that fails at every start ends the erasure with what it failed with,
// not only with the count of its starts (#29): the cause is the operator's
// to act on, and a log line is gone by the time anyone reads the record.
func TestAnErasureThatGivesUpSaysWhatItsTailFailedWith(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	e := f.startErasure(t, "user-a")
	_, err := f.store.runErasure(t.Context(), &brokenTail{jobSubmitter: f.writer}, e.ID,
		EraserOptions{after: func(step int) error {
			if step == 1 {
				f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 9, "user-a", "", "late", nil)}), false, 0)
			}
			return nil
		}})
	if err == nil {
		t.Fatal("the first run's tail did not fail")
	}
	for range erasureAttempts {
		if _, err := f.store.runErasure(t.Context(), &brokenTail{jobSubmitter: f.writer, parsed: true}, e.ID,
			EraserOptions{}); err == nil {
			t.Fatal("a resumed tail did not fail")
		}
	}
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Erasure(t.Context(), f.project.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ErasureFailed || !strings.HasPrefix(got.Error, "3 starts ended before the erasure did") ||
		!strings.HasSuffix(got.Error, "; its tail last failed with: "+causeRawBatch) {
		t.Errorf("the erasure ended %s with %q, want failed with its starts and what its tail said", got.State, got.Error)
	}
}

// A stop that ends the wait for room in a full queue ends a run that recorded
// no start, and its pause takes back none (#29): the starts on the row are the
// crashes before it.
func TestAStopBeforeTheStartKeepsTheCrashesCounted(t *testing.T) {
	f := newErasureFixture(t)
	e := f.startErasure(t, "user-a")
	for range 2 {
		if err := f.writer.Submit(t.Context(), &erasureBegin{ID: e.ID}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &stopWhileQueueFull{jobSubmitter: f.writer, stop: cancel}
	if _, err := f.store.runErasure(ctx, writer, e.ID, EraserOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the run was not stopped: %v", err)
	}
	if got := f.erasureOf(t, "user-a"); got.attempts != 2 {
		t.Errorf("the erasure has %d starts counted, want the two crashes", got.attempts)
	}
}

// stopWhileQueueFull answers the start with a full queue, and the worker's
// stop comes while the run waits for room.
type stopWhileQueueFull struct {
	jobSubmitter
	stop context.CancelFunc
}

func (w *stopWhileQueueFull) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*erasureBegin); ok {
		w.stop()
		return ErrWriterBusy
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// A worker that could not read the next erasure started none, and a request
// still wakes it (#29): it is not a failed run, to wait out a whole poll.
func TestAWorkerThatCouldNotReadStillWakes(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger {
		return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil))
	}
	t.Cleanup(func() { logger = old })
	if _, err := f.store.db.Exec(`ALTER TABLE erasures RENAME TO erasures_away`); err != nil {
		t.Fatal(err)
	}
	worker := f.store.NewEraser(f.writer, EraserOptions{Poll: time.Hour})
	worker.Start()
	defer worker.Close()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "could not read the next erasure")
	})
	if _, err := f.store.db.Exec(`ALTER TABLE erasures_away RENAME TO erasures`); err != nil {
		t.Fatal(err)
	}
	e := f.startErasure(t, "user-a")
	waitFor(t, func() bool {
		got, err := f.store.Erasure(t.Context(), f.project.ID, e.ID)
		return err == nil && got != nil && got.State == ErasureDone
	})
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// The end's log line says how long each phase of the run took (spec 047 #5,
// #29), and not a phase an earlier start finished.
func TestTheEndLogsEachPhaseTheRunWentThrough(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	whole := f.startErasure(t, "user-a")
	if _, err := f.store.runErasure(t.Context(), f.writer, whole.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"raw_took=", "parsed_took=", "tail_took="} {
		if !strings.Contains(logged.String(), key) {
			t.Errorf("the end's line %q has no %s", logged.String(), key)
		}
	}

	logged.Reset()
	resumed := f.startErasure(t, "user-b")
	for _, job := range []WriteJob{&erasureBegin{ID: resumed.ID}, &erasureStep{ID: resumed.ID, Phase: phaseTail}} {
		if err := f.writer.Submit(t.Context(), job); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.runErasure(t.Context(), f.writer, resumed.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if out := logged.String(); !strings.Contains(out, "tail_took=") || strings.Contains(out, "raw_took=") ||
		strings.Contains(out, "parsed_took=") {
		t.Errorf("a run resumed in its tail logs %q, want its tail's time only", out)
	}
}

// The last start's own tail failure is what the give-up says, though that
// start wrote the cap's sentence before its tail ran (#30): three crashes
// leave nothing in last_failure, and the fourth start's tail is the first to
// fail.
func TestAGiveUpSaysWhatTheLastTailFailedWith(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	e := f.startErasure(t, "user-a")
	// A crash after the chunks: the tail's windows are stored, and the
	// start stays counted.
	_, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{after: func(step int) error {
		switch step {
		case 1:
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 9, "user-a", "", "late", nil)}), false, 0)
		case 3:
			return errors.New("the process died")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("the run did not stop after its chunks")
	}
	for range erasureAttempts - 1 {
		if err := f.writer.Submit(t.Context(), &erasureBegin{ID: e.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.runErasure(t.Context(), &brokenTail{jobSubmitter: f.writer, parsed: true}, e.ID,
		EraserOptions{}); err == nil {
		t.Fatal("the last start's tail did not fail")
	}
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Erasure(t.Context(), f.project.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ErasureFailed || got.Error != capSentence+"; its tail last failed with: "+causeRawBatch {
		t.Errorf("the erasure ended %s with %q, want failed with what its last tail said", got.State, got.Error)
	}
}

// An erasure whose run failed does not hold up the others (#30): they are
// taken at once, before it, and it keeps the one start it spent.
func TestAFailedErasureDoesNotHoldUpTheQueue(t *testing.T) {
	f := newErasureFixture(t)
	failing := f.startErasure(t, "user-a")
	writer := &failingJobs{jobSubmitter: f.writer, err: errors.New("the disk is broken"),
		fails: func(job WriteJob) bool { end, ok := job.(*erasureEnd); return ok && end.ID == failing.ID }}
	worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
	worker.Start()
	defer worker.Close()
	waitFor(t, func() bool { return f.erasureOf(t, "user-a").attempts == 1 })
	for _, user := range []string{"user-b", "user-c"} {
		next := f.startErasure(t, user)
		waitFor(t, func() bool {
			got, err := f.store.Erasure(t.Context(), f.project.ID, next.ID)
			return err == nil && got.State == ErasureDone
		})
	}
	got, err := f.store.Erasure(t.Context(), f.project.ID, failing.ID)
	if err != nil || got.attempts != 1 || got.State != ErasureRunning {
		t.Errorf("the failed erasure is %s with %d starts (%v), want running with its one", got.State,
			got.attempts, err)
	}
}

// A last start whose tail ran to its end and whose end was not written leaves
// the give-up nothing to call dropped and nothing stale to say (#31): the
// windows were scrubbed, and an earlier tail's failure is not this one's.
func TestAGiveUpAfterATailThatRanDropsNothing(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	e := f.startErasure(t, "user-a")
	var late int64
	_, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{after: func(step int) error {
		switch step {
		case 1:
			late = f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 9, "user-a", "", "late", nil)}), false, 0)
		case 3:
			return errors.New("the process died")
		}
		return nil
	}})
	if err == nil {
		t.Fatal("the run did not stop after its chunks")
	}
	for _, job := range []WriteJob{&erasureStep{ID: e.ID, TailFailure: "an older tail failed"},
		&erasureBegin{ID: e.ID}, &erasureBegin{ID: e.ID}} {
		if err := f.writer.Submit(t.Context(), job); err != nil {
			t.Fatal(err)
		}
	}
	noEnd := &failingJobs{jobSubmitter: f.writer, err: errors.New("the disk is broken"),
		fails: func(job WriteJob) bool { _, end := job.(*erasureEnd); return end }}
	if _, err := f.store.runErasure(t.Context(), noEnd, e.ID, EraserOptions{}); err == nil {
		t.Fatal("the last start's end was written")
	}
	if spans := f.rawSpans(t, late); len(spans) != 0 {
		t.Fatalf("the last start's tail left %v", spans)
	}
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	if _, err := f.store.runErasure(t.Context(), f.writer, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Erasure(t.Context(), f.project.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ErasureFailed || got.Error != capSentence {
		t.Errorf("the erasure ended %s with %q, want failed with %q alone", got.State, got.Error, capSentence)
	}
	if !strings.Contains(logged.String(), "tail_windows_dropped=0") {
		t.Errorf("the give-up logs %q, want no windows dropped", logged.String())
	}
}

// Each source of an erasure's failures has its cause in the list (#32), and
// anything else is the one that sends an operator to the log.
func TestAFailureIsSaidAsACauseFromTheList(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("commit: %w", codedError{13}), causeFull},
		{fmt.Errorf("read: %w", codedError{5 | 2<<8}), causeBusy},
		{codedError{6}, causeBusy},
		{fmt.Errorf("rewrite raw batch 7: %w", codedError{10 | 4<<8}), causeIO},
		{codedError{7}, causeMemory},
		{codedError{14}, causeOpen},
		{ErrWriterBusy, causeQueue},
		{fmt.Errorf("%w 7: %w", errRawBatch, errors.New("replace the refs: boom")), causeRawBatch},
		{fmt.Errorf("%w 7: %w", errRawBatch,
			&Rejection{Kind: RejectConflict, Message: "raw batch 7 was rewritten since it was read"}), causeRawBatch},
		// A condition inside a raw batch's failure is the condition.
		{fmt.Errorf("%w 7: %w", errRawBatch, codedError{13}), causeFull},
		// Not conditions — they do not pass — but an operator acts on them.
		{fmt.Errorf("commit: %w", codedError{8}), causeReadOnly},
		{codedError{11}, causeDamaged},
		{codedError{26}, causeDamaged},
		{errors.New(`chunk limit 0 is not positive for "user-4711"`), causeOther},
	} {
		if got := failureCause(tc.err); got != tc.want {
			t.Errorf("failureCause(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A batch that cannot be read to plan its scrub is a raw batch that could not
// be rewritten, not an unexpected error (#32).
func TestABatchThatCannotBeReadIsARawBatchCause(t *testing.T) {
	f := newErasureFixture(t)
	if _, err := f.store.db.Exec(`ALTER TABLE raw_batches RENAME TO raw_batches_away`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.store.db.Exec(`ALTER TABLE raw_batches_away RENAME TO raw_batches`) })
	err := f.store.scrubBatches(t.Context(), f.writer, &Erasure{ProjectID: f.project.ID}, []int64{1},
		map[string]bool{"t": true})
	if err == nil || failureCause(err) != causeRawBatch {
		t.Errorf("a batch that could not be read failed with %v, said as %q", err, failureCause(err))
	}
}

// The writer does not give the error of an erasure's failed job (#33): its
// "write commit failed" line — and the line of a window the job took down —
// would give the error whole, and a chunk's refusal quoted the user. Both of
// the writer's paths: a window of the one
// job, and a window of several that is retried one job at a time.
func TestTheWriterLeavesAnErasureJobsFailureToIt(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })
	refused := func() *UserDataErase {
		return &UserDataErase{ProjectID: f.project.ID, UserID: "user-4711", Confirm: "someone else", Limit: 1,
			Erasure: &chunkErasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5"}}
	}
	var rejection *Rejection

	alone, err := f.store.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer alone.Close()
	if err := alone.Submit(t.Context(), refused()); !errors.As(err, &rejection) {
		t.Fatalf("a refused chunk answered %v, want its refusal", err)
	}
	// The store's own words on the erasure's path never hold the id (#33).
	if strings.Contains(rejection.Message, "4711") {
		t.Errorf("a chunk's refusal names the user: %q", rejection.Message)
	}

	window, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var commits atomic.Int32
	parked, release := make(chan struct{}), make(chan struct{})
	window.beforeCommit = func() {
		if commits.Add(1) == 1 {
			close(parked)
			<-release
		}
	}
	defer window.Close()
	first := make(chan error, 1)
	go func() { first <- window.Submit(context.Background(), sharesAWindow()) }()
	<-parked
	answers := make(chan error, 2)
	go func() { answers <- window.Submit(context.Background(), refused()) }()
	go func() { answers <- window.Submit(context.Background(), sharesAWindow()) }()
	waitFor(t, func() bool { return len(window.queue) == 2 })
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	failed := 0
	for range 2 {
		if err := <-answers; err != nil {
			if !errors.As(err, &rejection) {
				t.Fatalf("a job answered %v", err)
			}
			failed++
		}
	}
	// The parked window and the window of two: the chunk is refused in the
	// window's second pass, with a savepoint, and its neighbour committed.
	if failed != 1 || commits.Load() != 2 {
		t.Fatalf("%d refused after %d commits, want the chunk refused inside a window of two", failed,
			commits.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	for _, line := range []string{"write commit failed", "4711", "confirm is not the user id"} {
		if strings.Contains(logged.String(), line) {
			t.Errorf("the writer's log gives %q: %q", line, logged.String())
		}
	}
	// What refused inside the window is still said, without the error: what
	// it was a step of, and the cause (#33), once for the chunk refused alone
	// and once for the chunk refused inside the window.
	if out := logged.String(); strings.Count(out, "a write of an erasure did not commit") != 2 ||
		!strings.Contains(out, "erasure=4f0c9d3e8a1b2c3d4e5f60718293a4b5") || !strings.Contains(out, "cause=") {
		t.Errorf("the writer's log %q has no line for each refusal with the erasure and the cause", out)
	}
}

// The docs and the API's description give the cap's sentence as the code
// builds it, so that a change to the number of starts is a change to them.
func TestTheCapSentenceIsTheOneDocumented(t *testing.T) {
	for _, path := range []string{"../../docs/admin.md", "../server/openapi.json"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), capSentence) {
			t.Errorf("%s does not give %q", path, capSentence)
		}
	}
}

// failsInside fails in the writer's transaction with a condition, and words
// that name the user.
type failsInside struct{}

func (failsInside) apply(*sql.Tx) error {
	return fmt.Errorf("rewrite the batch of user-4711: %w", codedError{13})
}
func (failsInside) failureRedacted() (string, bool) { return "4f0c9d3e8a1b2c3d4e5f60718293a4b5", true }

// A condition an erasure's job met still reaches the writer's paced line —
// the condition, not the job's words (#33).
func TestTheWriterStillCountsAnErasureJobsCondition(t *testing.T) {
	f := newErasureFixture(t)
	conditionLog = &logpace.Keyed{Every: time.Minute}
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	if err := f.writer.Submit(t.Context(), failsInside{}); err == nil {
		t.Fatal("the job did not fail")
	}
	out := logged.String()
	if !strings.Contains(out, "write commit failed") || !strings.Contains(out, "condition=SQLITE_FULL") ||
		strings.Contains(out, "4711") || strings.Contains(out, "erasure=") {
		t.Errorf("the writer's log %q, want the condition, not the job's words and not one erasure's label", out)
	}
}

// failsReported is a job whose owner logs its failure, as the aggregator's
// hour does, failing inside the writer's transaction.
type failsReported struct{ err error }

func (f failsReported) apply(*sql.Tx) error { return f.err }
func (failsReported) failureReported() bool { return true }

// The writer's own rules for a job that reports its failure stay as they were
// before the erasure's (#33): a job of a window that fails by itself is refused
// there, and the writer says nothing of it, nor of a condition it met — the
// job's owner said it. No window came apart, so no line says one did.
func TestTheWriterKeepsItsRulesForAJobThatReportsItself(t *testing.T) {
	f := newErasureFixture(t)
	conditionLog = &logpace.Keyed{Every: time.Minute}
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })

	window, err := f.store.NewWriter(WriterOptions{CommitWindow: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	var commits atomic.Int32
	parked, release := make(chan struct{}), make(chan struct{})
	window.beforeCommit = func() {
		if commits.Add(1) == 1 {
			close(parked)
			<-release
		}
	}
	first := make(chan error, 1)
	go func() { first <- window.Submit(context.Background(), sharesAWindow()) }()
	<-parked
	answers := make(chan error, 2)
	go func() {
		answers <- window.Submit(context.Background(), failsReported{errors.New("the hour is broken")})
	}()
	go func() { answers <- window.Submit(context.Background(), sharesAWindow()) }()
	waitFor(t, func() bool { return len(window.queue) == 2 })
	close(release)
	<-first
	<-answers
	<-answers
	if err := window.Submit(context.Background(), failsReported{codedError{13}}); err == nil {
		t.Fatal("a job that met a condition did not fail")
	}
	mu.Lock()
	defer mu.Unlock()
	out := logged.String()
	if strings.Contains(out, "write window failed") {
		t.Errorf("the writer warned of a window that did not come apart: %q", out)
	}
	if strings.Contains(out, "write commit failed") || strings.Contains(out, "write refused") ||
		strings.Contains(out, "condition=") {
		t.Errorf("the writer logged a reporting job's failure, which its owner does: %q", out)
	}
}

// A refusal is routine: an erasure whose project was purged while it ran gets
// its refusal logged at Info, as the writer always logged it, not as an error
// (#33).
func TestARefusalOfAnErasureIsLoggedAsRoutine(t *testing.T) {
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	e := &Erasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5", UserID: "user-a"}
	e.reportFailure("a step failed", phaseParsed, &Rejection{Kind: RejectNotFound, Message: "no such project"})
	e.reportFailure("a step failed", phaseParsed, fmt.Errorf("commit: %w", codedError{13}))
	// A scrub that gave up on its conflicts ends the erasure failed: not routine.
	e.reportFailure("a step failed", phaseRaw, &rawBatchError{id: 7,
		err: &Rejection{Kind: RejectConflict, Message: "raw batch 7 was rewritten since it was read"}})
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[1], "level=ERROR") ||
		!strings.Contains(lines[2], "level=ERROR") {
		t.Errorf("the log %q, want the purge at INFO and the disk and the conflict at ERROR", logged.String())
	}
}

// A write of an erasure that fails after its submitter has given up — the
// context ended while the window was committing — is still in the log, as a
// line without the error: the run never sees it (#33).
func TestAnAbandonedErasureWriteIsStillLogged(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })
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
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	job := &UserDataErase{ProjectID: f.project.ID, UserID: "user-4711", Confirm: "other", Limit: 1,
		Erasure: &chunkErasure{ID: "4f0c9d3e8a1b2c3d4e5f60718293a4b5"}}
	go func() { gone <- w.Submit(ctx, job) }()
	<-parked
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned submission answered %v", err)
	}
	close(release)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "did not commit")
	})
	mu.Lock()
	defer mu.Unlock()
	out := logged.String()
	if !strings.Contains(out, "erasure=4f0c9d3e8a1b2c3d4e5f60718293a4b5") || !strings.Contains(out, "level=INFO") ||
		strings.Contains(out, "4711") || strings.Contains(out, "logs its error") {
		t.Errorf("the writer's line %q, want the erasure, INFO for the refusal, no user, and no promise", out)
	}
}

// failsOnceRedacted fails the first time it is applied, in a window, and
// commits alone after.
type failsOnceRedacted struct{ tries *atomic.Int32 }

func (f failsOnceRedacted) apply(*sql.Tx) error {
	if f.tries.Add(1) == 1 {
		return errors.New("a constraint the window hit")
	}
	return nil
}

func (failsOnceRedacted) failureRedacted() (string, bool) {
	return "4f0c9d3e8a1b2c3d4e5f60718293a4b5", true
}

// A job of an erasure that fails a window's first pass and is committed by the
// second leaves the writer's line of the replay, without the error, the only
// trace of why the window was applied twice (#33).
func TestAWindowAnErasureJobBrokeIsStillSaid(t *testing.T) {
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
	var commits atomic.Int32
	parked, release := make(chan struct{}), make(chan struct{})
	w.beforeCommit = func() {
		if commits.Add(1) == 1 {
			close(parked)
			<-release
		}
	}
	first := make(chan error, 1)
	go func() { first <- w.Submit(context.Background(), sharesAWindow()) }()
	<-parked
	var tries atomic.Int32
	answers := make(chan error, 2)
	go func() { answers <- w.Submit(context.Background(), failsOnceRedacted{&tries}) }()
	go func() { answers <- w.Submit(context.Background(), sharesAWindow()) }()
	waitFor(t, func() bool { return len(w.queue) == 2 })
	close(release)
	<-first
	for range 2 {
		if err := <-answers; err != nil {
			t.Fatalf("a job of the window failed alone: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	out := logged.String()
	if !strings.Contains(out, "first pass and passed the second") ||
		!strings.Contains(out, "erasure=4f0c9d3e8a1b2c3d4e5f60718293a4b5") || strings.Contains(out, "a constraint the window hit") {
		t.Errorf("the writer's log %q, want the replay's line with the erasure and not the error", out)
	}
}

// The task's jobs are the ones whose failure the writer does not give (#33),
// and each answers the erasure it is a step of; the same jobs outside a task
// are not.
func TestTheTasksJobsRedactTheirFailure(t *testing.T) {
	const id = "4f0c9d3e8a1b2c3d4e5f60718293a4b5"
	for _, tc := range []struct {
		name     string
		job      WriteJob
		redacted bool
	}{
		{"a chunk", &UserDataErase{Erasure: &chunkErasure{ID: id}}, true},
		{"a chunk on its own", &UserDataErase{}, false},
		{"a scrub", &RawScrub{ErasureID: id}, true},
		{"a scrub on its own", &RawScrub{}, false},
		{"a start", &erasureBegin{ID: id}, true},
		{"a step", &erasureStep{ID: id}, true},
		{"an end", &erasureEnd{ID: id}, true},
		{"a pause", &erasurePause{ID: id}, true},
	} {
		got, redacted := redacts(tc.job)
		if redacted != tc.redacted || (redacted && got != id) {
			t.Errorf("%s: redacts = (%q, %v), want (%q, %v)", tc.name, got, redacted, id, tc.redacted)
		}
	}
}

// A conflict on a scrub that is retried, and a retry whose batch can no
// longer be read, fail as a raw batch's and read as one (#33).
func TestARetryThatCannotReadItsBatchIsARawBatchFailure(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	id := f.count(t, `SELECT MAX(id) FROM raw_batches`)
	writer := &conflictThenBreak{jobSubmitter: f.writer, db: f.store.db}
	err := f.store.scrubBatches(t.Context(), writer, &Erasure{ProjectID: f.project.ID}, []int64{id},
		map[string]bool{hexTrace(1): true})
	if err == nil || !errors.Is(err, errRawBatch) || !strings.HasPrefix(err.Error(), "raw batch ") ||
		strings.Contains(err.Error(), "rewritten 7") {
		t.Fatalf("the retry answered %v, want a raw batch's failure that reads as one", err)
	}
	if got := failureCause(err); got != causeRawBatch {
		t.Errorf("the failure is said as %q, want %q", got, causeRawBatch)
	}
}

// conflictThenBreak answers a scrub with a conflict and takes the table the
// retry reads its batch from away.
type conflictThenBreak struct {
	jobSubmitter
	db *sql.DB
}

func (w *conflictThenBreak) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*RawScrub); ok {
		if _, err := w.db.Exec(`ALTER TABLE raw_batches RENAME TO raw_batches_away`); err != nil {
			return err
		}
		return &Rejection{Kind: RejectConflict, Message: "raw batch was rewritten since it was read"}
	}
	return w.jobSubmitter.Submit(ctx, job)
}

// The parsed phase's failure line says so, and a tail's cause that cannot be
// recorded is a warning, as it was (#33).
func TestTheParsedPhaseAndTheUnrecordedCauseAreSaidAsWhatTheyAre(t *testing.T) {
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
	var logged bytes.Buffer
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = old })
	was := busyWait
	busyWait = 20 * time.Millisecond
	t.Cleanup(func() { busyWait = was })

	e := f.startErasure(t, "user-a")
	if _, err := f.store.runErasure(t.Context(), &failingChunk{jobSubmitter: f.writer, at: 1,
		err: errors.New("the chunk broke")}, e.ID, EraserOptions{}); err != nil {
		t.Fatal(err)
	}
	if out := logged.String(); !strings.Contains(out, "the parsed phase of an erasure failed") ||
		strings.Contains(out, "a chunk of an erasure failed") {
		t.Errorf("the log %q, want the parsed phase's failure said as one", out)
	}

	logged.Reset()
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 2, 1, "user-b", "", "b", nil)}), false, daysAgo(1))
	e = f.startErasure(t, "user-b")
	broken := &brokenTail{jobSubmitter: f.writer}
	noCause := &failingJobs{jobSubmitter: broken, err: errors.New("the cause could not be written"),
		fails: func(job WriteJob) bool { step, ok := job.(*erasureStep); return ok && step.TailFailure != "" }}
	_, err := f.store.runErasure(t.Context(), noCause, e.ID, EraserOptions{after: func(step int) error {
		if step == 1 {
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 2, 9, "user-b", "", "late", nil)}), false, 0)
		}
		return nil
	}})
	if err == nil {
		t.Fatal("the tail did not fail")
	}
	if out := logged.String(); !strings.Contains(out, "level=WARN msg=\"a failed tail's cause is not recorded\"") {
		t.Errorf("the log %q, want the unrecorded cause at WARN", out)
	}
}

// What a line says of a failure is its cause, the Go types of the error and of
// what it wraps, SQLite's code, the kind of a refusal and the batch — names of
// the code and numbers, not words (#33).
func TestFailureFactsAreTypesAndCodesNotText(t *testing.T) {
	facts := func(err error) string { return fmt.Sprint(failureFacts(err)...) }
	got := fmt.Sprint(failureFacts(fmt.Errorf("commit the words of %q: %w", "someone", codedError{13})))
	for _, want := range []string{"cause", causeFull, "types", "store.codedError", "sqlite", "13"} {
		if !strings.Contains(got, want) {
			t.Errorf("facts of a full disk %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "someone") || strings.Contains(got, "fmt.wrapError") {
		t.Errorf("facts of a full disk %q hold text or a plain wrapper", got)
	}
	got = facts(&rawBatchError{id: 7, err: &Rejection{Kind: RejectConflict, Message: "words"}})
	for _, want := range []string{"store.Rejection", "refusal", RejectConflict, "batch", "7"} {
		if !strings.Contains(got, want) {
			t.Errorf("facts of a conflict %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "words") || strings.Contains(got, "rawBatchError") {
		t.Errorf("facts of a conflict %q hold the refusal's words or the store's own wrapper", got)
	}
	// However deep the chain, the names are few.
	deep := errors.New("bottom")
	for range 30 {
		deep = &layeredError{err: deep}
	}
	if n := strings.Count(errorTypes(deep), ">"); n > 7 {
		t.Errorf("a chain of 31 errors is named with %d separators, want at most 7", n)
	}
}

// layeredError is an error that wraps one, for a chain of types to name.
type layeredError struct{ err error }

func (e *layeredError) Error() string { return "layer" }
func (e *layeredError) Unwrap() error { return e.err }

// The worker's line says where the run stopped and the facts of its failure,
// and never what the error said (#33).
func TestTheWorkersLineGivesThePhaseAndTheFacts(t *testing.T) {
	f := newErasureFixture(t)
	var logged bytes.Buffer
	var mu sync.Mutex
	old := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &mu}, nil)) }
	t.Cleanup(func() { logger = old })
	writer := &failingJobs{jobSubmitter: f.writer, err: errors.New("the end could not be written"),
		fails: func(job WriteJob) bool { _, end := job.(*erasureEnd); return end }}
	worker := f.store.NewEraser(writer, EraserOptions{Poll: time.Hour})
	worker.Start()
	defer worker.Close()
	f.startErasure(t, "user-a")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logged.String(), "an erasure stopped before it ended")
	})
	mu.Lock()
	defer mu.Unlock()
	out := logged.String()
	for _, want := range []string{`cause="` + causeOther + `"`, "phase=end", "types=*errors.errorString"} {
		if !strings.Contains(out, want) {
			t.Errorf("the worker's line %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "runError") {
		t.Errorf("the worker's line names the store's own wrapper: %q", out)
	}
	if strings.Contains(out, "the end could not be written") {
		t.Errorf("the worker's line gives the error's words: %q", out)
	}
}
