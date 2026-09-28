package store

import (
	"context"
	"errors"
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
		name string
		fail error
	}{
		{name: "done"},
		{name: "failed", fail: errors.New(`a chunk refused "user-a" twice`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newErasureFixture(t)
			f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "a", nil)}), false, daysAgo(1))
			var writer jobSubmitter = f.writer
			if tc.fail != nil {
				writer = &failingChunk{jobSubmitter: f.writer, at: 1, err: tc.fail}
			}
			e := f.startErasure(t, "user-a")
			if e.UserID != "user-a" {
				t.Fatalf("a queued erasure is of %q, want the user", e.UserID)
			}
			if _, err := f.store.runErasure(t.Context(), writer, e.ID, EraserOptions{}); err != nil {
				t.Fatal(err)
			}
			got := f.erasureOf(t, "user-a")
			if got.UserID != "" || got.Phase != "" || got.FinishedAt == 0 {
				t.Errorf("ended, the erasure is of %q in %q, finished at %d", got.UserID, got.Phase, got.FinishedAt)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM erasures WHERE user_id IS NOT NULL`); n != 0 {
				t.Errorf("%d erasures still name a user", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM erasure_tail`); n != 0 {
				t.Errorf("%d tail rows are left", n)
			}
			if tc.fail != nil && (got.State != ErasureFailed || got.Error != `a chunk refused "the user" twice`) {
				t.Errorf("the erasure is %s with %q, want failed without the id", got.State, got.Error)
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
