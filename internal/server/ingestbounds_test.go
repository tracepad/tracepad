package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// The ingest bounds of spec 043 (Testing 8–11, and the body budget's half of
// 16): spans per export, slices, the body budget and the labels.

// bulkBody is an otlptest.Bulk export, encoded.
func bulkBody(t *testing.T, seed, traces, spansPerTrace int) []byte {
	t.Helper()
	return encodeExport(t, otlptest.Bulk(seed, traces, spansPerTrace))
}

// ingestSystem is the part of `GET /api/v1/system` the ingest bounds report.
type ingestSystem struct {
	BodyBudget struct {
		Held     int64 `json:"held_bytes"`
		Capacity int64 `json:"capacity_bytes"`
	} `json:"body_budget"`
	Counters struct {
		Rejected       int64 `json:"rejected_batches"`
		OverSpanCap    int64 `json:"exports_over_span_cap"`
		RefusedForBody int64 `json:"bodies_refused_for_budget"`
	} `json:"counters"`
}

func (h *harness) ingestSystem(t *testing.T, secret string) ingestSystem {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/system", nil, asKey(secret))
	expectStatus(t, rec, 200)
	return decodeJSON[ingestSystem](t, rec)
}

// dump is a project's traces and observations as text, every column the
// export decides, for comparing two stores that took the same spans.
func (h *harness) dump(t *testing.T, projectID string) []string {
	t.Helper()
	db := h.sqlOf(t)
	var out []string
	for _, query := range []string{
		`SELECT id, name, user_id, session_id, environment, tags, observation_count, error_count,
		        total_cost, timestamp, latency_ms, ttft_ms
		   FROM traces WHERE project_id = ? ORDER BY id`,
		`SELECT trace_id, id, parent_observation_id, type, name, start_time, end_time, model, usage
		   FROM observations WHERE project_id = ? ORDER BY trace_id, id`,
	} {
		rows, err := db.Query(query, projectID)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(values...))
		}
		rows.Close()
	}
	return out
}

// Testing 8: an export of the setting's number of spans is stored; one more is
// `413` with the message, stores nothing — not even the raw body — and is
// counted as a rejected batch and as over the cap, in its own project only
// (spec 043 #10, #21).
func TestSpanCap(t *testing.T) {
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		MaxSpansPerRequest: 32}, store.WriterOptions{})
	h.second(t, "other", "tp-sk-other")

	expectStatus(t, h.post(t, "/v1/traces", bulkBody(t, 1, 4, 8)), 200)
	rec := h.post(t, "/v1/traces", bulkBody(t, 2, 3, 11))
	expectError(t, rec, http.StatusRequestEntityTooLarge,
		"this export carries 33 spans; the server takes at most 32 per request (TRACEPAD_MAX_SPANS_PER_REQUEST)")
	if n := h.countTraces(t); n != 4 {
		t.Errorf("%d traces stored, want the first export's 4 and none of the refused one's", n)
	}
	if n := len(archived(t, h)); n != 1 {
		t.Errorf("%d raw bodies, want the accepted export's alone", n)
	}
	// The JSON encoding is counted the same way.
	jsonBody, err := otlptest.JSONBody(otlptest.Bulk(3, 1, 33))
	if err != nil {
		t.Fatal(err)
	}
	rec = h.post(t, "/v1/traces", jsonBody, func(r *http.Request) { r.Header.Set("Content-Type", "application/json") })
	expectStatus(t, rec, http.StatusRequestEntityTooLarge)

	mine := h.ingestSystem(t, testSecret)
	if mine.Counters.Rejected != 2 || mine.Counters.OverSpanCap != 2 {
		t.Errorf("counters = %+v, want 2 rejected batches, both over the span cap", mine.Counters)
	}
	if theirs := h.ingestSystem(t, "tp-sk-other"); theirs.Counters.OverSpanCap != 0 || theirs.Counters.Rejected != 0 {
		t.Errorf("another project's counters = %+v, want none of this project's refusals", theirs.Counters)
	}
}

// failingWriter passes submissions to the real writer and fails the nth
// ingest job with a database condition, as a full disk would.
type failingWriter struct {
	inner JobWriter
	mu    sync.Mutex
	seen  int
	fail  int
}

func (f *failingWriter) Submit(ctx context.Context, job store.WriteJob) error {
	if _, ok := job.(*store.IngestBatch); ok {
		f.mu.Lock()
		f.seen++
		fail := f.seen == f.fail
		f.mu.Unlock()
		if fail {
			return fmt.Errorf("commit write transaction: %w", codedError{13}) // SQLITE_FULL
		}
	}
	return f.inner.Submit(ctx, job)
}

// Testing 9: an export of 20,000 spans commits in transactions of at most
// 2,000 rows — a slice, and the job that crossed the line — and an export from
// a second project sent while the first commits is answered before the first
// finishes (spec 043 #11, #12).
func TestExportCommitsInSlices(t *testing.T) {
	var (
		mu           sync.Mutex
		transactions []int
		firstCommit  = make(chan struct{})
		secondQueued = make(chan struct{})
		once         sync.Once
	)
	h := newHarness(t, nil, store.WriterOptions{Committed: func(rows int) {
		mu.Lock()
		transactions = append(transactions, rows)
		mu.Unlock()
		// The first commit waits until the second project's export is
		// queued behind it, so the second has to be served between two
		// of the first's slices or not at all.
		once.Do(func() {
			close(firstCommit)
			<-secondQueued
		})
	}})
	h.second(t, "other", "tp-sk-other")

	big := bulkBody(t, 1, 1250, 16)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- h.post(t, "/v1/traces", big) }()

	<-firstCommit
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDone <- h.post(t, "/v1/traces", bulkBody(t, 2, 1, 1), asKey("tp-sk-other"))
	}()
	waitUntil(t, func() bool { waiting, _ := h.writer.QueueDepth(); return waiting > 0 })
	close(secondQueued)

	select {
	case rec := <-secondDone:
		expectStatus(t, rec, 200)
	case rec := <-firstDone:
		t.Fatalf("the 20,000-span export (%d) finished before a one-span export queued behind its first slice",
			rec.Code)
	}
	select {
	case <-firstDone:
		t.Fatal("the big export finished while the small one was being answered; nothing was between its slices")
	default:
	}
	expectStatus(t, <-firstDone, 200)

	mu.Lock()
	defer mu.Unlock()
	if len(transactions) < 21 {
		t.Errorf("%d transactions for 21,250 rows, want the export cut into slices: %v", len(transactions), transactions)
	}
	if slices.Max(transactions) > 2*store.SliceRows {
		t.Errorf("a transaction of %d rows, want at most %d: %v", slices.Max(transactions), 2*store.SliceRows, transactions)
	}
	if n := h.countTraces(t); n != 1250 {
		t.Errorf("%d traces, want 1250", n)
	}
	if n := h.countRows(t, h.project.ID, "observations"); n != 20000 {
		t.Errorf("%d observations, want every one of the 20,000 spans", n)
	}
	if n := len(archived(t, h)); n != 1 {
		t.Errorf("%d raw bodies, want one", n)
	}
}

// Testing 9, second half: a failure in the third slice answers `503`, leaves
// the first two slices' rows and no raw body, and the retried export converges
// on the rows of an uninterrupted one, with one raw body (spec 043 #11).
func TestAFailedSliceIsRetriedWhole(t *testing.T) {
	body := bulkBody(t, 1, 300, 16) // 5,100 rows: six slices

	clean := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, clean.post(t, "/v1/traces", body), 200)

	h := newHarness(t, nil, store.WriterOptions{})
	real := h.server.writer
	h.server.writer = &failingWriter{inner: real, fail: 3}
	rec := h.post(t, "/v1/traces", body)
	expectError(t, rec, http.StatusServiceUnavailable, "storage is temporarily unavailable; retry shortly")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	stored := h.countRows(t, h.project.ID, "observations")
	if stored == 0 || stored >= 4800 {
		t.Errorf("%d observations after a failure in the third slice, want the first two slices'", stored)
	}
	if n := len(archived(t, h)); n != 0 {
		t.Errorf("%d raw bodies after a failed export, want none", n)
	}

	h.server.writer = real
	expectStatus(t, h.post(t, "/v1/traces", body), 200)
	if got, want := h.dump(t, h.project.ID), clean.dump(t, clean.project.ID); !slices.Equal(got, want) {
		t.Errorf("the retried export stored %d rows that differ from an uninterrupted one's %d", len(got), len(want))
	}
	if n := len(archived(t, h)); n != 1 {
		t.Errorf("%d raw bodies after the retry, want one", n)
	}
}

// stalledWriter holds every submission until released, as a writer busy with a
// long commit would: what a request holds while it waits for the writer is
// what the body budget counts.
type stalledWriter struct {
	inner   JobWriter
	release chan struct{}
}

func (h *stalledWriter) Submit(ctx context.Context, job store.WriteJob) error {
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return h.inner.Submit(ctx, job)
}

// tallyReader counts what the server reads of a request body.
type tallyReader struct {
	mu   sync.Mutex
	r    io.Reader
	read int64
}

func (c *tallyReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.mu.Lock()
	c.read += int64(n)
	c.mu.Unlock()
	return n, err
}

func (c *tallyReader) total() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read
}

func counted(counter *tallyReader) func(*http.Request) {
	return func(r *http.Request) { r.Body = io.NopCloser(counter) }
}

// Testing 10: with the budget at twice the cap and the writer held, two
// exports of a full cap are admitted and a third — plain or gzip — is `429`
// with `Retry-After` after reading at most one step past what fit; a JSON API
// body and a media upload count against the same budget; releasing the writer
// admits them. The gauge shows what is held, and the refusals are counted in
// their own project only (spec 043 #13, #21).
func TestBodyBudget(t *testing.T) {
	const bodyCap = 256 << 10
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: bodyCap,
		BodyBudgetBytes: 2 * bodyCap}, store.WriterOptions{})
	h.second(t, "other", "tp-sk-other")

	full := func(seed int) []byte {
		body := bulkBody(t, seed, 36, 16)
		if len(body) <= bodyCap-bodyStep || len(body) > bodyCap {
			t.Fatalf("a body of %d bytes, want one that reserves the whole cap of %d", len(body), bodyCap)
		}
		return body
	}
	// The media ask comes first: it is a body too.
	picture := testPicture(30000, 7)
	_, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil {
		t.Fatal("no upload URL")
	}

	release := make(chan struct{})
	real := h.server.writer
	h.server.writer = &stalledWriter{inner: real, release: release}
	held := make(chan *httptest.ResponseRecorder, 2)
	for seed := range 2 {
		body := full(seed + 1)
		go func() { held <- h.post(t, "/v1/traces", body) }()
	}
	waitUntil(t, func() bool { return h.server.bodies.heldBytes() == 2*bodyCap })
	if gauge := h.ingestSystem(t, testSecret).BodyBudget; gauge.Held != 2*bodyCap || gauge.Capacity != 2*bodyCap {
		t.Errorf("body_budget = %+v, want the two bodies held of %d", gauge, 2*bodyCap)
	}

	third := full(3)
	plain := &tallyReader{r: bytes.NewReader(third)}
	rec := refusedWithin(t, func() *httptest.ResponseRecorder { return h.post(t, "/v1/traces", third, counted(plain)) })
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if plain.total() > bodyStep {
		t.Errorf("read %d bytes of a body the budget refused, want at most one step (%d)", plain.total(), bodyStep)
	}
	rec = refusedWithin(t, func() *httptest.ResponseRecorder {
		return h.post(t, "/v1/traces", gzipped(t, third), func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") })
	})
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)

	// A JSON API body and a media upload share the budget.
	rec = refusedWithin(t, func() *httptest.ResponseRecorder {
		return h.send(t, "POST", "/api/v1/scores", []map[string]any{{"trace_id": traceHex(1), "name": "accuracy", "value": 1}})
	})
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)
	put := &tallyReader{r: bytes.NewReader(picture)}
	rec = refusedWithin(t, func() *httptest.ResponseRecorder { return h.putUpload(t, *upload, put) })
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)
	if rec.Header().Get("Retry-After") != "1" || put.total() != int64(len(picture)) {
		t.Errorf("a refused upload: Retry-After %q, %d of %d bytes read; want the answer first and the rest drained",
			rec.Header().Get("Retry-After"), put.total(), len(picture))
	}

	close(release)
	for range 2 {
		expectStatus(t, <-held, 200)
	}
	if held := h.server.bodies.heldBytes(); held != 0 {
		t.Errorf("%d bytes still held after every request returned", held)
	}
	expectStatus(t, h.post(t, "/v1/traces", third), 200)
	expectStatus(t, h.send(t, "POST", "/api/v1/scores",
		[]map[string]any{{"trace_id": traceHex(1), "name": "accuracy", "value": 1}}), 201)
	expectStatus(t, h.putUpload(t, *upload, bytes.NewReader(picture)), 200)

	if mine := h.ingestSystem(t, testSecret).Counters.RefusedForBody; mine != 4 {
		t.Errorf("bodies_refused_for_budget = %d, want 4", mine)
	}
	if theirs := h.ingestSystem(t, "tp-sk-other").Counters.RefusedForBody; theirs != 0 {
		t.Errorf("another project's bodies_refused_for_budget = %d, want 0", theirs)
	}
}

// putUpload is the SDK's PUT of an upload URL with the body it is given.
func (h *harness) putUpload(t *testing.T, upload string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	parsed, err := url.Parse(upload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", parsed.RequestURI(), body)
	req.Header.Set("Content-Type", "image/png")
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

// A body over the cap is still `413`, even under a budget of exactly one cap
// that another body holds part of: the budget does not turn a final answer
// into a retryable one (spec 043 #13, spec 002 #27).
func TestAnOversizedBodyIsStill413UnderTheBudget(t *testing.T) {
	const bodyCap = 100 << 10 // not a multiple of the step
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: bodyCap,
		BodyBudgetBytes: bodyCap}, store.WriterOptions{})
	body := bulkBody(t, 1, 20, 16)
	if len(body) <= bodyCap {
		t.Fatalf("a body of %d bytes, want one over the %d cap", len(body), bodyCap)
	}
	expectStatus(t, h.post(t, "/v1/traces", body), http.StatusRequestEntityTooLarge)
	// And a body of exactly the cap fits a budget of exactly the cap.
	fits := bulkBody(t, 2, 14, 16)
	if len(fits) > bodyCap || len(fits) <= bodyCap-bodyStep {
		t.Fatalf("a body of %d bytes, want one in the cap's last step", len(fits))
	}
	expectStatus(t, h.post(t, "/v1/traces", fits), 200)
}

// Testing 11: a 20,000-character trace name, user id and model are stored at
// 1,000 characters, the raw body keeps them whole, and a remap produces the
// same rows; 200 tags with repeats keep the first 50 distinct in order
// (spec 043 #14).
func TestLabelsAreCutAtIngest(t *testing.T) {
	long := strings.Repeat("ж", 20_000)
	var tags []string
	for i := range 200 {
		tags = append(tags, fmt.Sprintf("t%d", i%80))
	}
	tagJSON, err := json.Marshal(tags)
	if err != nil {
		t.Fatal(err)
	}
	body := encodeExport(t, otlptest.SpanWith(
		"langfuse.trace.name", long,
		"langfuse.user.id", long,
		"langfuse.observation.model.name", long,
		"langfuse.trace.tags", string(tagJSON),
	))
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.post(t, "/v1/traces", body), 200)

	rec := h.get(t, "/api/v1/traces/"+probeTrace)
	expectStatus(t, rec, 200)
	var trace struct {
		Name         string   `json:"name"`
		UserID       string   `json:"user_id"`
		Tags         []string `json:"tags"`
		Observations []struct {
			Model string `json:"model"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatal(err)
	}
	cut := strings.Repeat("ж", 1000)
	if trace.Name != cut || trace.UserID != cut || len(trace.Observations) != 1 || trace.Observations[0].Model != cut {
		t.Errorf("name %d, user %d characters, want both cut at 1000", len([]rune(trace.Name)), len([]rune(trace.UserID)))
	}
	var want []string
	for i := range 50 {
		want = append(want, fmt.Sprintf("t%d", i))
	}
	if !slices.Equal(trace.Tags, want) {
		t.Errorf("tags = %v, want the first 50 distinct in order", trace.Tags)
	}

	raw := archived(t, h)
	if len(raw) != 1 || !bytes.Equal(raw[0].Body, body) {
		t.Fatal("the raw body is not the export as sent")
	}
	resourceSpans, _, err := mapping.DecodeExportRequest(raw[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw[0].Body), long) || mapping.CountSpans(resourceSpans) != 1 {
		t.Error("the raw body does not keep the whole name")
	}
	remapped := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, remapped.post(t, "/v1/traces", raw[0].Body), 200)
	if got, want := remapped.dump(t, remapped.project.ID), h.dump(t, h.project.ID); !slices.Equal(got, want) {
		t.Error("a remap of the raw body stored other rows")
	}
}

// refusedWithin runs a request that the budget should refuse at once; one
// that was read whole instead waits for the held writer, and fails the test
// here rather than hanging it.
func refusedWithin(t *testing.T, request func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request() }()
	select {
	case rec := <-done:
		return rec
	case <-time.After(5 * time.Second):
		t.Fatal("the request was not refused: it was read whole and is waiting for the writer")
		return nil
	}
}

// waitUntil polls a condition another goroutine establishes.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(time.Millisecond)
	}
}
