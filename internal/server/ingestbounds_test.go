package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"github.com/tracepad/tracepad/internal/model"
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
	if err := f.failing(job); err != nil {
		return err
	}
	return f.inner.Submit(ctx, job)
}

func (f *failingWriter) SubmitWaiting(ctx context.Context, job store.WriteJob) error {
	if err := f.failing(job); err != nil {
		return err
	}
	return f.inner.SubmitWaiting(ctx, job)
}

// failing counts an ingest job and answers the condition for the nth.
func (f *failingWriter) failing(job store.WriteJob) error {
	if _, ok := job.(*store.IngestBatch); !ok {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen++
	if f.seen == f.fail {
		return fmt.Errorf("commit write transaction: %w", codedError{13}) // SQLITE_FULL
	}
	return nil
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

func (h *stalledWriter) SubmitWaiting(ctx context.Context, job store.WriteJob) error {
	select {
	case <-h.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return h.inner.SubmitWaiting(ctx, job)
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
	// Each body reserves its declared length (#33); the budget is made the
	// two of them exactly, so that they spend it.
	first, second := full(1), full(2)
	spent := int64(len(first) + len(second))
	h.server.bodies.capacity = spent
	held := make(chan *httptest.ResponseRecorder, 2)
	for _, body := range [][]byte{first, second} {
		go func() { held <- h.post(t, "/v1/traces", body) }()
	}
	waitUntil(t, func() bool { return h.server.bodies.heldBytes() == spent })
	if gauge := h.ingestSystem(t, testSecret).BodyBudget; gauge.Held != spent || gauge.Capacity != spent {
		t.Errorf("body_budget = %+v, want the two bodies held of %d", gauge, spent)
	}

	third := full(3)
	plain := &tallyReader{r: bytes.NewReader(third)}
	rec := refusedWithin(t, func() *httptest.ResponseRecorder { return h.post(t, "/v1/traces", third, counted(plain)) })
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	// Answered first, then the rest of the body read and dropped, so the
	// exporter reads the 429 rather than a closed connection; that it kept at
	// most one step past what fit is TestBudgetReaderStopsAStepPastTheBudget.
	if plain.total() != int64(len(third)) {
		t.Errorf("read %d of %d bytes of a refused body, want it drained after the answer", plain.total(), len(third))
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
	long := strings.Repeat("字", 20_000)
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
	cut := strings.Repeat("字", 1000)
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

// A budget reader keeps at most one step past what the budget held: the read
// that did not fit is the last (spec 043 #13).
func TestBudgetReaderStopsAStepPastTheBudget(t *testing.T) {
	budget := &bodyBudget{capacity: 3 * bodyStep}
	hold := &bodyHold{budget: budget}
	body := bytes.Repeat([]byte("x"), 10*bodyStep)
	read, err := io.ReadAll(budgeted(bytes.NewReader(body), hold, int64(len(body))))
	if !errors.Is(err, errBodyBudget) {
		t.Fatalf("err = %v, want the budget's", err)
	}
	if len(read) > 4*bodyStep {
		t.Errorf("read %d bytes against a budget of %d, want at most one step past it", len(read), 3*bodyStep)
	}
	if budget.heldBytes() != 3*bodyStep {
		t.Errorf("held %d, want the whole budget", budget.heldBytes())
	}
	hold.releaseAll()
	if budget.heldBytes() != 0 {
		t.Errorf("held %d after the release", budget.heldBytes())
	}
}

// A client that hangs up while its write waits for the writer does not take
// its body out of the budget: the writer commits the job all the same, and the
// handler waits for it holding the reservation — for an export and for a JSON
// API write alike (spec 043 #31).
func TestAHangUpKeepsTheBodyInTheBudget(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	release := make(chan struct{})
	h.server.writer = &stalledWriter{inner: h.server.writer, release: release}

	for name, send := range map[string]func(ctx context.Context) *httptest.ResponseRecorder{
		"export": func(ctx context.Context) *httptest.ResponseRecorder {
			return h.post(t, "/v1/traces", bulkBody(t, 1, 2, 3), func(r *http.Request) { *r = *r.WithContext(ctx) })
		},
		"score": func(ctx context.Context) *httptest.ResponseRecorder {
			return h.call(t, "POST", "/api/v1/scores", mustJSON(t, []map[string]any{
				{"trace_id": traceHex(1), "name": "accuracy", "value": 1}}), func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
				*r = *r.WithContext(ctx)
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, hangUp := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { send(ctx); close(done) }()
			waitUntil(t, func() bool { return h.server.bodies.heldBytes() > 0 })
			hangUp()
			select {
			case <-done:
				t.Fatal("the handler returned when its client hung up, with its job still queued")
			case <-time.After(100 * time.Millisecond):
			}
			if h.server.bodies.heldBytes() == 0 {
				t.Error("the body left the budget while its job waited for the writer")
			}
			release <- struct{}{}
			<-done
			if held := h.server.bodies.heldBytes(); held != 0 {
				t.Errorf("%d bytes still held after the write", held)
			}
		})
	}
}

// A password change — a JSON API write that goes to the writer without the
// shared submit — waits for its commit holding its body's reservation too
// (spec 043 #31, #32).
func TestAHangUpKeepsAPasswordChangeInTheBudget(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	release := make(chan struct{})
	h.server.writer = &stalledWriter{inner: h.server.writer, release: release}

	ctx, hangUp := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
			"password": map[string]any{"current": testAccountPassword, "new": "a brand new password"},
		}), asSession(who), func(r *http.Request) { *r = *r.WithContext(ctx) })
	}()
	waitUntil(t, func() bool { return h.server.bodies.heldBytes() > 0 })
	hangUp()
	select {
	case <-done:
		t.Fatal("the handler returned when its client hung up, with its write still queued")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	if held := h.server.bodies.heldBytes(); held != 0 {
		t.Errorf("%d bytes still held after the write", held)
	}
}

// An upload URL granted for more than the whole body budget holds — issued
// before a restart with smaller bounds — is refused for good, having read
// nothing, rather than with a 429 its SDK would retry until the grant expired
// (spec 043 #32).
func TestAnUploadLongerThanTheBudgetIsFinal(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(30000, 9)
	_, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil {
		t.Fatal("no upload URL")
	}
	h.server.bodies.capacity = int64(len(picture)) - 1
	put := &tallyReader{r: bytes.NewReader(picture)}
	rec := h.putUpload(t, *upload, put)
	expectError(t, rec, http.StatusRequestEntityTooLarge, "ask for a new upload URL")
	if put.total() != 0 {
		t.Errorf("read %d bytes of an upload refused for its length, want none", put.total())
	}
}

// hangUpAfterFirstDelete lets the first chunk of a bulk deletion commit and
// then hangs its client up, counting the chunks submitted.
type hangUpAfterFirstDelete struct {
	JobWriter
	hangUp  context.CancelFunc
	deletes int
}

func (w *hangUpAfterFirstDelete) Submit(ctx context.Context, job store.WriteJob) error {
	if _, ok := job.(*store.TraceDelete); !ok {
		return w.JobWriter.Submit(ctx, job)
	}
	w.deletes++
	err := w.JobWriter.Submit(ctx, job)
	w.hangUp()
	return err
}

// A write that read no body keeps its client's cancellation: a bulk deletion
// whose client hangs up stops at the chunk in progress, as it did before the
// body budget (spec 043 #33).
func TestAHangUpStillStopsABulkDeletion(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const hour = int64(time.Hour)
	for i := range 3 {
		start := seedBase + int64(i)*hour
		h.seed(t, &model.Trace{ID: traceHex(i + 1), Name: "chat"},
			&model.Observation{TraceID: traceHex(i + 1), ID: spanHex(i + 1), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: start, EndTime: start + 1000})
	}
	ctx, hangUp := context.WithCancel(context.Background())
	writer := &hangUpAfterFirstDelete{JobWriter: h.server.writer, hangUp: hangUp}
	h.server.writer = writer
	to := time.Unix(0, seedBase+10*hour).UTC().Format(time.RFC3339)
	h.call(t, "DELETE", "/api/v1/traces?to="+to+"&confirm=test", nil, func(r *http.Request) { *r = *r.WithContext(ctx) })
	// The chunk being handed over when the client left is committed, as it
	// always was — the writer takes a queued job — and the round stops there.
	if writer.deletes != 2 {
		t.Errorf("%d chunks submitted, want the round stopped at the chunk in progress when its client hung up (2)", writer.deletes)
	}
	// That chunk commits in the writer's own time; the one after it never
	// reaches the writer.
	if n := h.countTraces(t); n < 1 {
		t.Errorf("%d traces left, want the one the stopped round did not reach", n)
	}
}

// mediaGoneOnce answers ErrMediaGone to an export's second slice once, as a
// deletion between slices would; after that, a full queue refuses every
// ingest job not submitted to wait.
type mediaGoneOnce struct {
	inner *store.Writer
	mu    sync.Mutex
	jobs  int
	gone  bool
}

func (m *mediaGoneOnce) Submit(ctx context.Context, job store.WriteJob) error {
	if _, ok := job.(*store.IngestBatch); ok {
		m.mu.Lock()
		m.jobs++
		gone, answer := m.gone, m.jobs == 2 && !m.gone
		m.gone = m.gone || answer
		m.mu.Unlock()
		if answer {
			return store.ErrMediaGone
		}
		if gone {
			return store.ErrWriterBusy
		}
	}
	return m.inner.Submit(ctx, job)
}

func (m *mediaGoneOnce) SubmitWaiting(ctx context.Context, job store.WriteJob) error {
	m.mu.Lock()
	m.jobs++
	answer := m.jobs == 2 && !m.gone
	m.gone = m.gone || answer
	m.mu.Unlock()
	if answer {
		return store.ErrMediaGone
	}
	return m.inner.SubmitWaiting(ctx, job)
}

// An export taken again after ErrMediaGone met it half-way is admitted
// already: a full queue does not refuse its first slice again, which would
// leave part of it on disk (spec 043 #33).
func TestAnExportTakenAgainAfterMediaGoneWaitsForRoom(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.writer = &mediaGoneOnce{inner: h.writer}
	expectStatus(t, h.post(t, "/v1/traces", bulkBody(t, 1, 150, 16)), 200)
	if n := h.countRows(t, h.project.ID, "observations"); n != 2400 {
		t.Errorf("%d observations, want all 2,400", n)
	}
}

// A body that declares its length reserves exactly that, whole, before it is
// read — a small one does not hold a 64 KiB step — and one that does not fit
// is refused before a byte of it is read; a declared length over the cap is
// 413 before the budget is asked (spec 043 #33).
func TestADeclaredBodyIsReservedWholeBeforeItIsRead(t *testing.T) {
	const bodyCap = 256 << 10
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: bodyCap,
		BodyBudgetBytes: bodyCap}, store.WriterOptions{})
	release := make(chan struct{})
	h.server.writer = &stalledWriter{inner: h.server.writer, release: release}

	small, big := bulkBody(t, 1, 1, 2), bulkBody(t, 2, 20, 16)
	// Room for the small one and all but a byte of the big one.
	h.server.bodies.capacity = int64(len(small) + len(big) - 1)
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() { held <- h.post(t, "/v1/traces", small) }()
	waitUntil(t, func() bool { return h.server.bodies.heldBytes() > 0 })
	if got := h.server.bodies.heldBytes(); got != int64(len(small)) {
		t.Errorf("a %d-byte body holds %d bytes of the budget, want exactly its length", len(small), got)
	}

	// Refused before it is read, then drained: when the drain first reads,
	// the refusal is already counted.
	reader := &pausingReader{r: bytes.NewReader(big), pauseAt: 1, paused: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.post(t, "/v1/traces", big, func(r *http.Request) { r.Body = io.NopCloser(reader) }) }()
	select {
	case <-reader.paused:
	case <-time.After(10 * time.Second):
		t.Fatal("the body was never read")
	}
	refusedFirst := h.ingestSystem(t, testSecret).Counters.RefusedForBody == 1
	close(reader.resume)
	rec := refusedWithin(t, func() *httptest.ResponseRecorder { return <-done })
	expectError(t, rec, http.StatusTooManyRequests, bodyBusy)
	if !refusedFirst {
		t.Error("the body was read before it was refused; a declared length is decided on before reading")
	}

	// Over the cap is final, whatever the budget holds.
	over := bytes.Repeat([]byte{0}, bodyCap+1)
	expectStatus(t, h.post(t, "/v1/traces", over), http.StatusRequestEntityTooLarge)

	close(release)
	expectStatus(t, <-held, 200)
}

// A body reserved whole before it is read is read as it is, not a step at a
// time: there is nothing left to cover (spec 043 #34). One without a declared
// length still goes through the steps.
func TestABodyReservedWholeIsNotReadInSteps(t *testing.T) {
	const limit = 1 << 20
	for _, declared := range []bool{true, false} {
		hold := &bodyHold{budget: &bodyBudget{capacity: limit}}
		r := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(make([]byte, 4*bodyStep)))
		if !declared {
			r.ContentLength = -1
		}
		if err := reserveDeclared(r, hold, limit); err != nil {
			t.Fatal(err)
		}
		_, stepped := budgeted(r.Body, hold, limit).(*budgetReader)
		if stepped == declared {
			t.Errorf("declared length %t: read in steps %t, want %t", declared, stepped, !declared)
		}
	}
}

// A budget at the largest int64 — the default under a huge body cap — refuses
// what does not fit rather than wrapping its count negative and admitting
// everything after it.
func TestABudgetAtTheLargestInt64DoesNotWrap(t *testing.T) {
	budget := &bodyBudget{capacity: math.MaxInt64}
	half := int64(math.MaxInt64/2 + 1)
	if !budget.reserve(half) {
		t.Fatal("the first half did not fit an empty budget")
	}
	if budget.reserve(half) {
		t.Error("a second half fit a budget that holds one")
	}
	if held := budget.heldBytes(); held != half {
		t.Errorf("the budget holds %d, want %d", held, half)
	}
}

// oneBusyWriter answers ErrWriterBusy to the nth ingest job submitted without
// waiting, as a full queue would, and passes everything else through.
type oneBusyWriter struct {
	inner   *store.Writer
	mu      sync.Mutex
	seen    int
	busyAt  int
	waiting int
}

func (b *oneBusyWriter) Submit(ctx context.Context, job store.WriteJob) error {
	if _, ok := job.(*store.IngestBatch); ok {
		b.mu.Lock()
		b.seen++
		busy := b.seen == b.busyAt
		b.mu.Unlock()
		if busy {
			return store.ErrWriterBusy
		}
	}
	return b.inner.Submit(ctx, job)
}

func (b *oneBusyWriter) SubmitWaiting(ctx context.Context, job store.WriteJob) error {
	b.mu.Lock()
	b.seen++
	b.waiting++
	b.mu.Unlock()
	return b.inner.SubmitWaiting(ctx, job)
}

// A full queue refuses an export's first slice only: the slices after it wait
// for room, so an admitted export is not left half written for a retry that
// meets the same queue (spec 043 #31).
func TestAFullQueueRefusesOnlyTheFirstSlice(t *testing.T) {
	body := bulkBody(t, 1, 150, 16) // 2,550 rows: three slices

	h := newHarness(t, nil, store.WriterOptions{})
	busy := &oneBusyWriter{inner: h.writer, busyAt: 2}
	h.server.writer = busy
	expectStatus(t, h.post(t, "/v1/traces", body), 200)
	if busy.waiting != 2 {
		t.Errorf("%d slices waited for room, want the two after the first", busy.waiting)
	}
	if n := h.countRows(t, h.project.ID, "observations"); n != 2400 {
		t.Errorf("%d observations, want all 2,400", n)
	}

	h.server.writer = &oneBusyWriter{inner: h.writer, busyAt: 1}
	rec := h.post(t, "/v1/traces", bulkBody(t, 2, 150, 16))
	expectStatus(t, rec, http.StatusTooManyRequests)
}

// A request the budget refuses gives its reservation back before its body is
// drained, rather than holding it for as long as the drain takes (spec 043
// #31).
func TestARefusedBodyIsDrainedWithoutItsReservation(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	budget := h.server.bodies
	budget.mu.Lock()
	budget.held = budget.capacity - bodyStep // one step free
	budget.mu.Unlock()
	t.Cleanup(func() {
		budget.mu.Lock()
		budget.held = 0
		budget.mu.Unlock()
	})

	// The second step does not fit, so the drain starts past it and pauses
	// just after.
	body := bulkBody(t, 1, 40, 16)
	if len(body) < 3*bodyStep {
		t.Fatalf("a body of %d bytes, want one the drain has to go on reading", len(body))
	}
	drain := make(chan struct{})
	reader := &pausingReader{r: bytes.NewReader(body), pauseAt: 2*bodyStep + 100, paused: make(chan struct{}), resume: drain}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.post(t, "/v1/traces", body, func(r *http.Request) { r.Body = io.NopCloser(reader) }) }()
	select {
	case <-reader.paused:
	case rec := <-done:
		t.Fatalf("answered %d without draining the body", rec.Code)
	case <-time.After(10 * time.Second):
		t.Fatal("the body was never drained")
	}
	if held := budget.heldBytes(); held != budget.capacity-bodyStep {
		t.Errorf("held %d during the drain, want the refused request's step given back (%d)",
			held, budget.capacity-bodyStep)
	}
	close(drain)
	expectError(t, <-done, http.StatusTooManyRequests, bodyBusy)
}

// pausingReader stops once, after pauseAt bytes, until resumed.
type pausingReader struct {
	r       io.Reader
	read    int
	pauseAt int
	paused  chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (p *pausingReader) Read(b []byte) (int, error) {
	if p.read >= p.pauseAt {
		p.once.Do(func() { close(p.paused); <-p.resume })
	}
	if limit := p.pauseAt - p.read; limit > 0 && len(b) > limit {
		b = b[:limit]
	}
	n, err := p.r.Read(b)
	p.read += n
	return n, err
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

// A lookup by a label longer than the bound finds the row ingest stored cut:
// every trace filter, the user and the session in a path, a score's session
// and the user an erasure names are cut as ingest cuts them (spec 043 #34).
// Looked up whole, each matched nothing, and the erasure erased nothing.
func TestALookupByALongLabelFindsItsCutRow(t *testing.T) {
	h := newAdminHarness(t)
	long := func(prefix string) string { return prefix + strings.Repeat("x", 1500) }
	user, session, tag := long("u"), long("s"), long("t")
	tags, err := json.Marshal([]string{tag})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, otlptest.SpanWith(
		"langfuse.user.id", user,
		"langfuse.session.id", session,
		"langfuse.trace.tags", string(tags),
		"langfuse.trace.name", long("n"),
		"langfuse.environment", long("e"),
		"langfuse.release", long("r"),
		"langfuse.version", long("v"),
	))), 200)
	h.postScore(t, map[string]any{"session_id": session, "name": "helpful", "value": 1})

	count := func(path, key string) int {
		t.Helper()
		rec := h.get(t, path)
		expectStatus(t, rec, 200)
		listing := decodeJSON[map[string]json.RawMessage](t, rec)
		var rows []json.RawMessage
		if err := json.Unmarshal(listing[key], &rows); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return len(rows)
	}
	for _, query := range []string{
		"user_id=" + user, "session_id=" + session, "tag=" + tag, "name=" + long("n"),
		"environment=" + long("e"), "release=" + long("r"), "version=" + long("v"),
	} {
		if n := count("/api/v1/traces?"+query, "traces"); n != 1 {
			t.Errorf("traces by %.12s…: %d, want the one stored cut", query, n)
		}
	}
	if n := count("/api/v1/sessions?user_id="+user, "sessions"); n != 1 {
		t.Errorf("sessions by the user: %d, want 1", n)
	}
	// The user listing answers from the rollup.
	h.rollTheCorpus(t, time.Now().Add(2*time.Hour))
	if n := count("/api/v1/users?prefix="+user, "users"); n != 1 {
		t.Errorf("users by the whole id as a prefix: %d, want 1", n)
	}
	if n := count("/api/v1/scores?session_id="+session, "scores"); n != 1 {
		t.Errorf("scores by the session: %d, want 1", n)
	}
	if n := count("/api/v1/sessions/"+session, "traces"); n != 1 {
		t.Errorf("the session's traces: %d, want 1", n)
	}
	expectStatus(t, h.get(t, "/api/v1/users/"+user), 200)
	if rec := h.get(t, "/api/v1/stats?user_id="+user); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Errorf("stats for the user: %d %s, want its one trace", rec.Code, rec.Body.String())
	}

	path := "/api/v1/projects/" + h.project.ID + "/users/" + user + "/data"
	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		WouldDelete map[string]int64 `json:"would_delete"`
	}](t, rec)
	if preview.WouldDelete["traces"] != 1 || preview.WouldDelete["session_scores"] != 1 {
		t.Errorf("would_delete = %v, want the trace and the session's score", preview.WouldDelete)
	}
	rec = h.call(t, "DELETE", path+"?confirm="+user, nil)
	expectStatus(t, rec, 200)
	if erased := decodeJSON[struct {
		Deleted map[string]int64 `json:"deleted"`
	}](t, rec); erased.Deleted["traces"] != 1 {
		t.Errorf("deleted = %v, want the trace", erased.Deleted)
	}
	if n := count("/api/v1/traces", "traces"); n != 0 {
		t.Errorf("%d traces left after the erasure, want none", n)
	}
}
