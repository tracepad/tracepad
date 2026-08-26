package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

func openIngestStore(t *testing.T) (*Store, *Project) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func batchFor(projectID, traceID, spanID string, opts ...func(*model.Observation)) *IngestBatch {
	obs := &model.Observation{
		TraceID:   traceID,
		ID:        spanID,
		Type:      model.TypeSpan,
		Name:      "unit",
		Level:     model.LevelDefault,
		StartTime: 1_000_000_000,
		EndTime:   1_500_000_000,
	}
	for _, opt := range opts {
		opt(obs)
	}
	return &IngestBatch{
		ProjectID:    projectID,
		Traces:       []*model.Trace{{ID: traceID, Name: "unit-trace"}},
		Observations: []*model.Observation{obs},
	}
}

// Concurrent submissions are folded into commit windows and every caller is
// released only after its own data is committed (spec 002 #15).
func TestWriterGroupCommit(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{CommitWindow: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	const submissions = 16
	var wg sync.WaitGroup
	errs := make(chan error, submissions)
	for i := range submissions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- w.Submit(context.Background(),
				batchFor(p.ID, "aa11bb22cc33dd44ee55ff6677889900", spanHex(i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	trace, err := s.Trace(p.ID, "aa11bb22cc33dd44ee55ff6677889900")
	if err != nil || trace == nil {
		t.Fatalf("trace = %v, err = %v", trace, err)
	}
	if trace.ObservationCount != submissions {
		t.Errorf("observation_count = %d, want %d", trace.ObservationCount, submissions)
	}
}

// A full queue is refused immediately: an exporter can retry a 429, but it
// cannot recover from a request that hangs (spec 002 #15).
func TestWriterReportsBackpressure(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{QueueDepth: 1, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	parked := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	w.beforeCommit = func() {
		parked <- struct{}{}
		<-release
	}
	defer func() { unblock(); w.Close() }()

	submit := func(i int) <-chan error {
		out := make(chan error, 1)
		go func() {
			out <- w.Submit(context.Background(),
				batchFor(p.ID, "aa11bb22cc33dd44ee55ff6677889900", spanHex(i)))
		}()
		return out
	}

	// The first submission occupies the writer, which parks in
	// beforeCommit; the second then fills the single queue slot.
	first := submit(1)
	<-parked
	second := submit(2)
	waitFor(t, func() bool { return len(w.queue) == 1 })

	if err := <-submit(3); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("submit into a full queue = %v, want ErrWriterBusy", err)
	}

	unblock()
	if err := <-first; err != nil {
		t.Errorf("first submission: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second submission: %v", err)
	}
}

// waitFor polls a condition that another goroutine establishes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the writer queue to settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWriterRefusesSubmitAfterClose(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	err = w.Submit(context.Background(), batchFor(p.ID, "aa11bb22cc33dd44ee55ff6677889900", spanHex(1)))
	if !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("submit after close = %v, want ErrWriterClosed", err)
	}
}

// A batch that cannot be written must not take its window neighbours down
// with it.
func TestWriterIsolatesAFailingBatch(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{CommitWindow: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	poison := batchFor(p.ID, "aa11bb22cc33dd44ee55ff6677889900", spanHex(1))
	poison.Observations[0].Type = "not-a-type" // violates the CHECK constraint
	good := batchFor(p.ID, "aa11bb22cc33dd44ee55ff6677889900", spanHex(2))

	var wg sync.WaitGroup
	var poisonErr, goodErr error
	wg.Add(2)
	go func() { defer wg.Done(); poisonErr = w.Submit(context.Background(), poison) }()
	go func() { defer wg.Done(); goodErr = w.Submit(context.Background(), good) }()
	wg.Wait()

	if poisonErr == nil {
		t.Error("a constraint violation should be reported to its submitter")
	}
	if goodErr != nil {
		t.Errorf("a valid batch sharing the window failed too: %v", goodErr)
	}
	observations, err := s.Observations(p.ID, "aa11bb22cc33dd44ee55ff6677889900")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Errorf("observations = %d, want only the valid one", len(observations))
	}
}

// Values above the threshold are compressed, small ones are not, and both
// round-trip (spec 002 #8).
func TestPayloadCompression(t *testing.T) {
	small := []byte(`{"k":"v"}`)
	compression, body, sizeRaw := compress(small)
	if compression != CompressionNone || sizeRaw != len(small) {
		t.Fatalf("small payload: compression=%s size=%d", compression, sizeRaw)
	}
	restored, err := Decompress(compression, body)
	if err != nil || string(restored) != string(small) {
		t.Fatalf("round-trip small: %s, %v", restored, err)
	}

	large := make([]byte, 4096)
	for i := range large {
		large[i] = byte('a' + i%26)
	}
	compression, body, sizeRaw = compress(large)
	if compression != CompressionZstd || sizeRaw != len(large) || len(body) >= len(large) {
		t.Fatalf("large payload: compression=%s size=%d stored=%d", compression, sizeRaw, len(body))
	}
	restored, err = Decompress(compression, body)
	if err != nil || string(restored) != string(large) {
		t.Fatalf("round-trip large: %v", err)
	}
}

// spanHex builds a distinct 16-hex span id per index.
func spanHex(i int) string {
	const digits = "0123456789abcdef"
	out := []byte("000000000000000")
	return string(out) + string(digits[i%16])
}
