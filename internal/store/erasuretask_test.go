package store

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
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
// no user id in the row, no tail, and the failure's sentence without the id
// even when the error quoted it.
func TestAnEndedErasureForgetsTheUser(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		fail       error
	}{
		{name: "done"},
		{name: "failed", fail: errors.New("the disk is full")},
		// An error that names the user is not kept, in either form (#27).
		{name: "failed naming the user", user: "user-a", fail: errors.New(`a chunk refused "user-a" twice`)},
		{name: "failed quoting the user", user: `we"ird`, fail: fmt.Errorf("a chunk refused %q", `we"ird`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newErasureFixture(t)
			user := cmp.Or(tc.user, "user-a")
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, user, "", "a", nil)}), false, daysAgo(1))
			var writer jobSubmitter = f.writer
			if tc.fail != nil {
				writer = &failingChunk{jobSubmitter: f.writer, at: 1, err: tc.fail}
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
			switch {
			case tc.fail == nil:
			case tc.user == "":
				if got.State != ErasureFailed || got.Error != tc.fail.Error() {
					t.Errorf("the erasure is %s with %q, want failed with the chunk's error", got.State, got.Error)
				}
			case got.State != ErasureFailed || strings.Contains(got.Error, "ird") ||
				strings.Contains(got.Error, "user-a") || !strings.Contains(got.Error, "parsed phase failed"):
				t.Errorf("the erasure is %s with %q, want failed, the phase named and the user not", got.State, got.Error)
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
}

func (w *brokenTail) Submit(ctx context.Context, job WriteJob) error {
	if _, scrub := job.(*RawScrub); scrub && w.parsed {
		w.failed = true
		return errors.New("the disk is broken")
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
		!strings.Contains(got.Error, "its tail last failed with: ") || !strings.Contains(got.Error, "the disk is broken") {
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
