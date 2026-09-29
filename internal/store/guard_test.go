package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// A panic in a write job, in a worker's pass or in an erasure's run is
// contained (spec 043 #42): it is logged with its stack, the work in hand is
// abandoned, and the process — the writer, the workers, the interface — goes on.

// A job that panics in a window is the job failing by itself: rolled back
// alone, answered with a *PanicError, and the jobs beside it commit (spec 043
// #38). The log names the job's type and carries the stack, and not what the
// panic quoted.
func TestAJobThatPanicsInAWindowRollsBackAlone(t *testing.T) {
	logged := captureLog(t)
	s, p := openIngestStore(t)
	probe := newProbeWindow(t, s)
	h := newHeldWriter(t, s, p)
	var applied atomic.Int32
	panicker := &weighedJob{rows: 1, do: func(*sql.Tx) error {
		applied.Add(1)
		panic(fmt.Errorf("the user %s", "quoted-user"))
	}}
	errs, _ := h.submitAll(t, probe.watching("a"), panicker, probe.job("b", nil))

	var panicked *PanicError
	if errs[0] != nil || !errors.As(errs[1], &panicked) || errs[2] != nil {
		t.Fatalf("answers %v, want the middle job answered with a panic error and the others committed", errs)
	}
	if got, want := probe.stored(t), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("stored %v, want %v", got, want)
	}
	for _, want := range []string{"a panic was recovered", "write job *store.weighedJob", "stack=", "TestAJobThatPanics"} {
		if !strings.Contains(logged(), want) {
			t.Errorf("the log does not say %q:\n%s", want, logged())
		}
	}
	if strings.Contains(logged(), "quoted-user") || strings.Contains(panicked.Error(), "quoted-user") {
		t.Error("the panic's value reached the log or the error")
	}

	// The writer is still there.
	if err := h.Submit(t.Context(), probe.job("after", nil)); err != nil {
		t.Errorf("a write after the panic: %v", err)
	}
}

// A lone job that panics is answered with the error too, and its
// transaction is discarded.
func TestALoneJobThatPanicsIsAnswered(t *testing.T) {
	captureLog(t)
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	probe := newProbeWindow(t, s)
	err = w.Submit(t.Context(), &weighedJob{rows: 1, do: func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO probe (id) VALUES ('half')`); err != nil {
			return err
		}
		var none *IngestBatch
		_ = none.ProjectID // a nil dereference
		return nil
	}})
	var panicked *PanicError
	if !errors.As(err, &panicked) {
		t.Fatalf("answer = %v, want a panic error", err)
	}
	if got := probe.stored(t); len(got) != 0 {
		t.Errorf("stored %v after a job that panicked, want nothing", got)
	}
	if err := w.Submit(t.Context(), batchFor(p.ID, fmt.Sprintf("%032x", 7), spanHex(7))); err != nil {
		t.Errorf("a write after the panic: %v", err)
	}
}

// A panic in a pass costs that pass: the sweeper and the aggregator tick on,
// and stop when they are told to.
func TestAPanicInAPassCostsThatPassAlone(t *testing.T) {
	for _, name := range []string{"sweeper", "aggregator"} {
		t.Run(name, func(t *testing.T) {
			logged := captureLog(t)
			s, _ := openIngestStore(t)
			w, err := s.NewWriter(WriterOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()

			// Armed after the constructor, which reads the clock once itself.
			var armed atomic.Bool
			var calls atomic.Int32
			now := func() time.Time {
				if armed.Load() && calls.Add(1) == 1 {
					panic("the first pass")
				}
				return time.Now()
			}
			var closer interface{ Close() error }
			switch name {
			case "sweeper":
				sw := s.NewSweeper(w, SweepOptions{Interval: 10 * time.Millisecond, Now: now})
				armed.Store(true)
				sw.Start()
				closer = sw
			default:
				ag := s.NewAggregator(w, RollupOptions{Interval: 10 * time.Millisecond, Now: now})
				armed.Store(true)
				ag.Start()
				closer = ag
			}
			deadline := time.Now().Add(5 * time.Second)
			for calls.Load() < 3 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if calls.Load() < 3 {
				t.Fatalf("the %s ran %d passes, want it to go on after the panic", name, calls.Load())
			}
			done := make(chan struct{})
			go func() { closer.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not return")
			}
			if !strings.Contains(logged(), "a panic was recovered") {
				t.Errorf("the panic was not logged:\n%s", logged())
			}
		})
	}
}

// A panic in an erasure's run leaves the erasure running, as any failed run
// does (spec 047 #12): the worker rests it for its poll and takes it again, and
// the second run finishes it. No crash loop on a start that resumes it.
func TestAnErasureWhoseRunPanicsIsTakenAgain(t *testing.T) {
	logged := captureLog(t)
	f := newErasureFixture(t)
	f.ingestOTLP(t, export([]*tracepb.Span{otlpSpan(t, 1, 1, "user-a", "", "x", nil)}), false, daysAgo(1))
	erasure := f.startErasure(t, "user-a")

	var panics atomic.Int32
	worker := f.store.NewEraser(f.writer, EraserOptions{Poll: 20 * time.Millisecond, after: func(int) error {
		if panics.Add(1) == 1 {
			panic(errors.New("a fault that quotes user-a"))
		}
		return nil
	}})
	worker.Start()
	defer worker.Close()

	e, err := f.store.AwaitErasure(t.Context(), f.project.ID, erasure.ID, 10*time.Second)
	if err != nil || e == nil || e.State != ErasureDone {
		t.Fatalf("the erasure after a panicking run: %+v, %v; want done", e, err)
	}
	if !strings.Contains(logged(), "a panic was recovered") || strings.Contains(logged(), "quotes user-a") {
		t.Errorf("the log, want the panic without its words:\n%s", logged())
	}
	// The line that reports the run names the erasure and the phase it was in.
	if !strings.Contains(logged(), "where=\"erasure "+erasure.ID+"\"") || !strings.Contains(logged(), "phase=") {
		t.Errorf("the log does not tie the panic to its erasure and phase:\n%s", logged())
	}
}

// A panic in the sweep of one project costs that project's sweep: the pass goes
// on to the projects and the steps after it, and finishes.
func TestAPanicInOneProjectsSweepDoesNotStopTheRest(t *testing.T) {
	captureLog(t)
	s, _ := openIngestStore(t)
	if _, err := s.CreateProject("second", KeyPair{PublicKey: "tp-pk-second", Secret: "tp-sk-second"}); err != nil {
		t.Fatal(err)
	}
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sw := s.NewSweeper(w, SweepOptions{})
	var seen []string
	sw.beforeProject = func(p *Project) {
		seen = append(seen, p.Name)
		if len(seen) == 1 {
			panic("the first project's data")
		}
	}
	err = sw.Pass(t.Context())
	if !errors.As(err, new(*PanicError)) {
		t.Fatalf("Pass = %v, want the panic among its failures", err)
	}
	if len(seen) != 2 {
		t.Errorf("the pass swept %v, want both projects", seen)
	}
	if sw.lastRun.IsZero() {
		t.Error("the pass did not finish: its last run never moved")
	}
}

// The same for the roll: a project that panics costs its own statistics.
func TestAPanicInOneProjectsRollDoesNotStopTheRest(t *testing.T) {
	captureLog(t)
	s, _ := openIngestStore(t)
	if _, err := s.CreateProject("second", KeyPair{PublicKey: "tp-pk-second", Secret: "tp-sk-second"}); err != nil {
		t.Fatal(err)
	}
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ag := s.NewAggregator(w, RollupOptions{})
	var seen []string
	ag.beforeProject = func(p *Project) {
		seen = append(seen, p.Name)
		if len(seen) == 1 {
			panic("the first project's data")
		}
	}
	if err := ag.Pass(t.Context()); !errors.As(err, new(*PanicError)) {
		t.Fatalf("Pass = %v, want the panic among its failures", err)
	}
	if len(seen) != 2 {
		t.Errorf("the pass rolled %v, want both projects", seen)
	}
	if ag.LastRun().IsZero() {
		t.Error("the pass did not finish: its last run never moved")
	}
}

// A nil job is refused where it is submitted: the writer's loop never calls a
// method on one (spec 043 #42).
func TestANilJobIsRefusedAtSubmit(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Submit(t.Context(), nil); err == nil {
		t.Error("Submit(nil) was accepted")
	}
	if err := w.SubmitWaiting(t.Context(), nil); err == nil {
		t.Error("SubmitWaiting(nil) was accepted")
	}
	if err := w.Submit(t.Context(), batchFor(p.ID, fmt.Sprintf("%032x", 11), spanHex(11))); err != nil {
		t.Errorf("a write after the refusals: %v", err)
	}
}

// A project whose part of the pass panics in every pass is given up on after
// maxHeldPasses, as a failing hour is (spec 043 #8, #42); one that recovers
// starts the count again.
func TestAProjectThatPanicsInEveryPassIsGivenUp(t *testing.T) {
	logged := captureLog(t)
	s, _ := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	sw := s.NewSweeper(w, SweepOptions{})
	swept := 0
	sw.beforeProject = func(*Project) {
		swept++
		panic("this project's data")
	}
	for range maxHeldPasses + 3 {
		_ = sw.Pass(t.Context())
	}
	if swept != maxHeldPasses {
		t.Errorf("the project was swept %d times, want %d and then left out", swept, maxHeldPasses)
	}
	if !strings.Contains(logged(), "retention gave up on a project") {
		t.Errorf("giving up was not logged:\n%s", logged())
	}

	// A pass that does not panic starts the count again.
	var ledger panicLedger
	panicked := &PanicError{Where: "x"}
	for range maxHeldPasses - 1 {
		ledger.settle("p", panicked)
	}
	ledger.settle("p", nil)
	for range maxHeldPasses - 1 {
		ledger.settle("p", panicked)
	}
	if ledger.skip("p") {
		t.Error("a project that recovered in between was given up on")
	}
}
