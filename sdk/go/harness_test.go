package tracepad

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	caseID  = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	otherID = "b2c3d4e5f60718293a4b5c6d7e8f90a1"
	runID   = "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7"
)

// fakeStore answers by path — a list of answers is consumed one per call —
// and records every call it saw.
type fakeStore struct {
	*httptest.Server
	mu      sync.Mutex
	answers map[string][]any
	calls   []call
}

type call struct {
	method, path, query string
	body                any
}

func serveStore(t *testing.T) *fakeStore {
	t.Helper()
	fs := &fakeStore{answers: map[string][]any{}}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body any
		_ = json.Unmarshal(raw, &body)
		fs.mu.Lock()
		fs.calls = append(fs.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, body})
		var answer any = map[string]any{}
		if queue := fs.answers[r.URL.Path]; len(queue) > 0 {
			answer, fs.answers[r.URL.Path] = queue[0], queue[1:]
		}
		fs.mu.Unlock()
		if status, refused := answer.(int); refused {
			http.Error(w, `{"error":"refused"}`, status)
			return
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeStore) answer(path string, answers ...any) { fs.answers[path] = answers }

func (fs *fakeStore) paths() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []string
	for _, c := range fs.calls {
		p := c.method + " " + c.path
		if c.query != "" {
			p += "?" + c.query
		}
		out = append(out, p)
	}
	return out
}

func (fs *fakeStore) last() call {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.calls[len(fs.calls)-1]
}

// harness is a recorder whose default points at a fake store.
func harness(t *testing.T) (*recorder, *fakeStore) {
	t.Helper()
	fs := serveStore(t)
	r := setup(t, WithHost(fs.URL))
	return r, fs
}

func opened(t *testing.T, fs *fakeStore) *Run {
	t.Helper()
	fs.answer("/api/v1/datasets/golden/runs", map[string]any{"id": runID, "name": "a run", "dataset_version": 12})
	run, err := NewDataset("golden").Run(context.Background(), "a run", WithRunMetadata(map[string]any{"prompt": "p@7"}))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestADatasetMakesNoRequest(t *testing.T) {
	_, fs := harness(t)
	NewDataset("golden")
	if len(fs.paths()) != 0 {
		t.Errorf("calls = %v", fs.paths())
	}
}

func TestPutItemsSendsOneBody(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/datasets/golden/items", map[string]any{"version": 3, "changed": 2})
	version, changed, err := NewDataset("golden").PutItems(context.Background(), []Item{
		{ID: caseID, Input: map[string]any{"q": "?"}, ExpectedOutput: "!"},
		{ID: otherID, Input: "bare", DatasetVersion: 99},
	})
	if err != nil || version != 3 || changed != 2 {
		t.Fatalf("version %d, changed %d, err %v", version, changed, err)
	}
	// Unset fields are omitted, not sent as null, and the version is never sent.
	want := []any{
		map[string]any{"id": caseID, "input": map[string]any{"q": "?"}, "expected_output": "!"},
		map[string]any{"id": otherID, "input": "bare"},
	}
	if !reflect.DeepEqual(fs.last().body, want) {
		t.Errorf("body = %v", fs.last().body)
	}
}

func TestItemsFollowTheCursorToTheEnd(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/datasets/golden/items",
		map[string]any{"items": []any{map[string]any{"id": caseID, "version": 12}}, "next_cursor": "c2"},
		map[string]any{"items": []any{map[string]any{"id": otherID, "version": 11}}, "next_cursor": ""},
	)
	var ids []string
	for item, err := range NewDataset("golden").Items(context.Background(), 12) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, item.ID)
		if item.DatasetVersion == 0 {
			t.Error("the version the row was written at was not read")
		}
	}
	if strings.Join(ids, ",") != caseID+","+otherID {
		t.Errorf("ids = %v", ids)
	}
	paths := fs.paths()
	if len(paths) != 2 || !strings.Contains(paths[0], "limit=500") || !strings.Contains(paths[0], "version=12") ||
		strings.Contains(paths[0], "cursor") || !strings.Contains(paths[1], "cursor=c2") {
		t.Errorf("paths = %v", paths)
	}
}

func TestAPagingErrorEndsTheSequenceWithIt(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/datasets/golden/items", 503)
	var got error
	for _, err := range NewDataset("golden").Items(context.Background(), 0) {
		got = err
	}
	var h *HTTPError
	if !errors.As(got, &h) || h.Status != 503 {
		t.Errorf("err = %v", got)
	}
}

func TestARunCarriesTheVersionItPinned(t *testing.T) {
	_, fs := harness(t)
	run := opened(t, fs)
	if run.ID != runID || run.DatasetVersion != 12 || run.Name != "a run" {
		t.Errorf("run = %+v", run)
	}
	body := fs.last().body.(map[string]any)
	if body["name"] != "a run" || body["metadata"].(map[string]any)["prompt"] != "p@7" {
		t.Errorf("body = %v", body)
	}
}

func TestFinishFlushesBeforeItPosts(t *testing.T) {
	r, fs := harness(t)
	run := opened(t, fs)
	s := &sender{}
	queue(t, s, nil)
	fs.answer("/api/v1/runs/"+runID+"/finish", map[string]any{"status": "finished"})
	_, step := Span(context.Background(), "s")
	_ = Score(context.Background(), "x", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	step.End()
	closed, err := run.Finish(context.Background())
	if err != nil || closed["status"] != "finished" {
		t.Fatalf("closed = %v, err = %v", closed, err)
	}
	if len(s.batches) != 1 || len(r.spans()) != 1 {
		t.Errorf("scores = %d, spans = %d, want both delivered before the post", len(s.batches), len(r.spans()))
	}
	if body := fs.last().body.(map[string]any); len(body) != 0 {
		t.Errorf("finish body = %v, want empty", body)
	}
}

func TestFailPostsTheError(t *testing.T) {
	_, fs := harness(t)
	run := opened(t, fs)
	if _, err := run.Fail(context.Background(), errors.New("judge timed out")); err != nil {
		t.Fatal(err)
	}
	body := fs.last().body.(map[string]any)
	if body["status"] != "failed" || body["error"] != "judge timed out" {
		t.Errorf("body = %v", body)
	}
}

func TestTheReadSideIsTheServersJSON(t *testing.T) {
	_, fs := harness(t)
	run := opened(t, fs)
	fs.answer("/api/v1/runs/"+runID, map[string]any{"summary": map[string]any{"traces": map[string]any{"count": 2.0}}})
	fs.answer("/api/v1/runs/"+runID+"/items", map[string]any{"items": []any{map[string]any{"id": caseID}}})
	fs.answer("/api/v1/runs/a/compare/b", map[string]any{"a": map[string]any{"id": "a"}})
	got, err := run.Get(context.Background())
	if err != nil || got["summary"].(map[string]any)["traces"].(map[string]any)["count"] != 2.0 {
		t.Errorf("get = %v, err = %v", got, err)
	}
	var items []map[string]any
	for item, err := range run.Items(context.Background(), WithUnknown(), WithLimit(10)) {
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	if len(items) != 1 || items[0]["id"] != caseID || !strings.Contains(fs.last().query, "unknown=true") ||
		!strings.Contains(fs.last().query, "limit=10") {
		t.Errorf("items = %v, query = %s", items, fs.last().query)
	}
	if verdict, err := Compare(context.Background(), "a", "b"); err != nil || verdict["a"].(map[string]any)["id"] != "a" {
		t.Errorf("compare = %v, err = %v", verdict, err)
	}
}

func TestDeleteEchoesTheName(t *testing.T) {
	_, fs := harness(t)
	if _, err := NewDataset("golden").Delete(context.Background(), "golden"); err != nil {
		t.Fatal(err)
	}
	if last := fs.last(); last.method != "DELETE" || last.query != "confirm=golden" {
		t.Errorf("last = %+v", last)
	}
}

func TestCreateSendsOnlyWhatWasGiven(t *testing.T) {
	_, fs := harness(t)
	if _, err := NewDataset("golden").Create(context.Background(), "the golden set", nil); err != nil {
		t.Fatal(err)
	}
	if last := fs.last(); last.method != "PUT" || last.body.(map[string]any)["description"] != "the golden set" || len(last.body.(map[string]any)) != 1 {
		t.Errorf("last = %+v", last)
	}
}

// --- the stamping ---------------------------------------------------------

func stamped(t *testing.T, r *recorder, name string) (run, item string) {
	t.Helper()
	attrs := r.attrs(t, name)
	return attrs[attrRunID].AsString(), attrs[attrItemID].AsString()
}

func TestASpanInsideTheContextCarriesTheRunAndTheItem(t *testing.T) {
	r, fs := harness(t)
	run := opened(t, fs)
	ctx, attempt := run.Item(context.Background(), Item{ID: caseID})

	// The framework's span, started by another tracer from the same context:
	// the processor reads the context at OnStart, whoever started the span.
	ctx, request := r.provider.Tracer("net/http").Start(ctx, "GET /answer")
	_, step := Span(ctx, "answer")
	step.End()
	request.End()

	for _, name := range []string{"GET /answer", "answer"} {
		if gotRun, gotItem := stamped(t, r, name); gotRun != runID || gotItem != caseID {
			t.Errorf("%s stamped %q / %q", name, gotRun, gotItem)
		}
	}
	// The context opened no span of its own, and one trace was recorded.
	if len(r.spans()) != 2 || len(attempt.Traces()) != 1 || attempt.TraceID() != step.TraceID() {
		t.Errorf("spans = %d, traces = %v", len(r.spans()), attempt.Traces())
	}
}

func TestEveryRootIsRecordedAndTheLastIsTheTrace(t *testing.T) {
	r, fs := harness(t)
	ctx, attempt := opened(t, fs).Item(context.Background(), Item{ID: caseID})
	_, first := Span(ctx, "first")
	first.End()
	_, second := Span(ctx, "second")
	second.End()
	if traces := attempt.Traces(); len(traces) != 2 || traces[1] != second.TraceID() || attempt.TraceID() != second.TraceID() {
		t.Errorf("traces = %v", traces)
	}
	_ = r
}

func TestACaseBeginsWhereTheContextDoesNotWhereTheTraceDoes(t *testing.T) {
	r, fs := harness(t)
	// The loop itself runs under a span of the harness's own.
	ctx, loop := Span(context.Background(), "the-loop")
	ctx, attempt := opened(t, fs).Item(ctx, Item{ID: caseID})
	_, step := Span(ctx, "answer")
	step.End()
	loop.End()
	if attempt.TraceID() != loop.TraceID() || len(attempt.Traces()) != 1 {
		t.Errorf("traces = %v, want the loop's trace, once", attempt.Traces())
	}
	if _, ok := r.attrs(t, "the-loop")[attrRunID]; ok {
		t.Error("the loop's own span was stamped")
	}
}

func TestASpanOutsideTheContextIsNotStamped(t *testing.T) {
	r, fs := harness(t)
	run := opened(t, fs)
	_, attempt := run.Item(context.Background(), Item{ID: caseID})
	_, step := Span(context.Background(), "outside")
	step.End()
	if _, ok := r.attrs(t, "outside")[attrRunID]; ok || len(attempt.Traces()) != 0 {
		t.Error("a span started from another context was stamped")
	}
}

func TestAGoroutineHandedTheContextIsStamped(t *testing.T) {
	r, fs := harness(t)
	ctx, _ := opened(t, fs).Item(context.Background(), Item{ID: caseID})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, step := Span(ctx, "worker")
		step.End()
	}()
	<-done
	if gotRun, _ := stamped(t, r, "worker"); gotRun != runID {
		t.Errorf("worker stamped %q", gotRun)
	}
}

func TestAScoreBeforeAnyTraceIsErrNoTrace(t *testing.T) {
	_, fs := harness(t)
	_, attempt := opened(t, fs).Item(context.Background(), Item{ID: caseID})
	err := attempt.Score(context.Background(), "accuracy", WithValue(1))
	if !errors.Is(err, ErrNoTrace) || !strings.Contains(err.Error(), caseID) {
		t.Errorf("err = %v", err)
	}
}

func TestAScoreGoesAgainstTheTraceTheContextSaw(t *testing.T) {
	_, fs := harness(t)
	s := &sender{}
	q := queue(t, s, nil)
	ctx, attempt := opened(t, fs).Item(context.Background(), Item{ID: caseID})
	_, step := Span(ctx, "answer")
	step.End()
	// Scored from a context with no span of its own: the attempt's trace is
	// the target, not the caller's.
	if err := attempt.Score(context.Background(), "accuracy", WithValue(1), WithComment("ok")); err != nil {
		t.Fatal(err)
	}
	flushed(t, q)
	if got := s.batches[0][0]; got["trace_id"] != step.TraceID() || got["comment"] != "ok" {
		t.Errorf("score = %v", got)
	}
}

func TestACaseWithoutAnIDStampsNothing(t *testing.T) {
	r, fs := harness(t)
	ctx, _ := opened(t, fs).Item(context.Background(), Item{})
	_, step := Span(ctx, "s")
	step.End()
	if _, ok := r.attrs(t, "s")[attrRunID]; ok || !strings.Contains(r.logs.String(), "needs an item id") {
		t.Errorf("stamped = %v, logs = %q", ok, r.logs.String())
	}
}

func TestAttributesAreTheTwoForAnotherProcess(t *testing.T) {
	_, fs := harness(t)
	_, attempt := opened(t, fs).Item(context.Background(), Item{ID: caseID})
	want := map[string]string{"tracepad.run_id": runID, "tracepad.item_id": caseID}
	if got := attempt.Attributes(); got["tracepad.run_id"] != want["tracepad.run_id"] || got["tracepad.item_id"] != want["tracepad.item_id"] {
		t.Errorf("attributes = %v", got)
	}
}

func TestScoreConfigsArePutInOrderAndStopAtTheFirstRefusal(t *testing.T) {
	_, fs := harness(t)
	min, max := 0.0, 1.0
	configs := []ScoreConfig{
		{Name: "accuracy", DataType: "numeric", Direction: "higher", Min: &min, Max: &max},
		{Name: "verdict", DataType: "categorical", Categories: []string{"pass", "fail"}},
	}
	if err := ScoreConfigs(context.Background(), configs); err != nil {
		t.Fatal(err)
	}
	paths := fs.paths()
	if len(paths) != 2 || paths[0] != "PUT /api/v1/score-configs/accuracy" || paths[1] != "PUT /api/v1/score-configs/verdict" {
		t.Errorf("paths = %v", paths)
	}
	if want := (map[string]any{"data_type": "numeric", "direction": "higher", "min": 0.0, "max": 1.0}); !reflect.DeepEqual(fs.calls[0].body, want) {
		t.Errorf("body = %v", fs.calls[0].body)
	}

	fs.answer("/api/v1/score-configs/accuracy", 400)
	err := ScoreConfigs(context.Background(), configs)
	var h *HTTPError
	if !errors.As(err, &h) || h.Status != 400 || !strings.Contains(err.Error(), `"accuracy"`) || len(fs.paths()) != 3 {
		t.Errorf("err = %v, calls = %v", err, fs.paths())
	}
}

func TestItemIDIsTheDocumentedDerivation(t *testing.T) {
	// sha256(key)[:32], as docs/scores.md derives a score id.
	sum := sha256.Sum256([]byte("cases/refund.json"))
	if got := ItemID("cases/refund.json"); got != hex.EncodeToString(sum[:])[:32] {
		t.Errorf("id = %s", got)
	}
}

func TestTheRunLinkIsNotADialectOfItsOwn(t *testing.T) {
	// The two keys are the ones the mapper reads (spec 014 #2), the same in
	// every harness in every language.
	if attrRunID != "tracepad.run_id" || attrItemID != "tracepad.item_id" {
		t.Errorf("keys = %s, %s", attrRunID, attrItemID)
	}
}
