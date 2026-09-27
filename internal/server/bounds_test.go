package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/logpace"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// Spec 043, the server's half of PR 1: a status an exporter or a client can
// act on for every failure that is not the request's own, and numbers that
// always render.

// A credential the server could not check is `503` with `Retry-After`, never
// `401` — an exporter drops a batch on `401`, and a browser signs out (spec 043
// #1). A closed database fails every lookup.
func TestACredentialThatCouldNotBeCheckedIs503(t *testing.T) {
	expectCannotCheck := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		expectError(t, rec, http.StatusServiceUnavailable, "cannot check credentials right now; retry shortly")
		if got := rec.Header().Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1", got)
		}
	}

	t.Run("a key", func(t *testing.T) {
		h := newHarness(t, nil, store.WriterOptions{})
		h.store.Close()
		expectCannotCheck(t, h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation")))
		expectCannotCheck(t, h.get(t, "/api/v1/traces"))
	})

	t.Run("a session", func(t *testing.T) {
		h := newAccountHarness(t)
		who := h.owner(t)
		h.store.Close()
		rec := h.call(t, "GET", "/api/v1/traces", nil, anonymous, asSession(who), inProject(h.project.ID))
		expectCannotCheck(t, rec)
		// Not a sign-out: the cookie was never judged, so it is not
		// cleared.
		if cookies := rec.Header().Values("Set-Cookie"); len(cookies) > 0 {
			t.Errorf("Set-Cookie = %v, want the session left alone", cookies)
		}
	})
}

// conditionWriter fails every write with the error it holds.
type conditionWriter struct{ err error }

func (w conditionWriter) Submit(context.Context, store.WriteJob) error { return w.err }

// codedError is the driver's error as the store reads it: a message and a
// result code.
type codedError struct{ code int }

func (e codedError) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e codedError) Code() int     { return e.code }

// A failure of the database is `503` with `Retry-After`, which every OTLP
// exporter retries; a failure of the batch stays `500`, which none does, so
// that a poison batch is not retried for ever (spec 043 #2).
func TestADatabaseConditionIsRetryable(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := fixtureBody(t, "001-langfuse-sdk-generation")

	h.server.writer = conditionWriter{fmt.Errorf("commit write transaction: %w", codedError{13})} // SQLITE_FULL
	rec := h.post(t, "/v1/traces", body)
	expectError(t, rec, http.StatusServiceUnavailable, "storage is temporarily unavailable; retry shortly")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}

	h.server.writer = conditionWriter{codedError{19 | 8<<8}} // SQLITE_CONSTRAINT_UNIQUE
	rec = h.post(t, "/v1/traces", body)
	expectError(t, rec, http.StatusInternalServerError, "failed to store spans")
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q on the batch's own failure, want none", got)
	}
}

// A JSON write — a score from an SDK — that a database condition failed gets
// the same retryable answer an export does (spec 043 #2).
func TestADatabaseConditionIsRetryableOnTheJSONAPI(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.writer = conditionWriter{codedError{5}} // SQLITE_BUSY
	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"trace_id": traceHex(1), "name": "accuracy", "value": 1}})
	expectError(t, rec, http.StatusServiceUnavailable, "storage is temporarily unavailable; retry shortly")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
}

// A client that hangs up while its credential is looked up is gone, not a
// storage failure: nothing is answered and nothing is logged at error (spec
// 043 #24), as ingest already treats a hang-up.
func TestAClientThatHangsUpDuringTheLookupIsNotAFailure(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/traces", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusServiceUnavailable {
		t.Errorf("status = 503 for a client that hung up, want nothing answered")
	}
	if strings.Contains(logged.String(), "lookup failed") {
		t.Errorf("a hang-up was logged as a failed lookup:\n%s", logged.String())
	}
}

// A credential lookup a database condition failed is logged once a minute per
// condition, as the writer logs the same condition (spec 043 #24): while a
// lock is held past the busy timeout every exporter's retry fails its lookup,
// and a line each would bury the one that says why. Anything else is logged
// every time.
func TestAFailedLookupOnAConditionIsLoggedOnceAMinute(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	lookupLog = &logpace.Keyed{Every: time.Minute}

	lookup := func(err error) {
		rec := httptest.NewRecorder()
		_, ok := guardLookup(rec, httptest.NewRequest("GET", "/api/v1/traces", nil), "key",
			func(context.Context) (int, error) { return 0, err })
		if ok || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("lookup = %v, status %d, want a 503", ok, rec.Code)
		}
	}
	for range 10 {
		lookup(codedError{5}) // SQLITE_BUSY
	}
	if lines := strings.Count(logged.String(), "condition=SQLITE_BUSY"); lines != 1 {
		t.Errorf("logged %d lines for 10 lookups a held lock failed, want 1:\n%s", lines, logged.String())
	}
	for range 2 {
		lookup(errors.New("no such table: api_keys"))
	}
	if lines := strings.Count(logged.String(), "no such table"); lines != 2 {
		t.Errorf("logged %d lines for 2 lookups that failed on their own, want 2", lines)
	}
}

// A guard lookup that ran out of its deadline is logged once a minute, as a
// condition is (spec 043 #24 u): whatever held one lookup past five seconds
// holds every exporter's retry too. A driver reports it as an interrupt or as
// the context's error; either way the lookup's context is what expired.
func TestALookupPastItsDeadlineIsLoggedOnceAMinute(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	lookupLog = &logpace.Keyed{Every: time.Minute}
	deadline := credentialDeadline
	credentialDeadline = time.Millisecond
	t.Cleanup(func() { credentialDeadline = deadline })

	for _, reported := range []error{context.DeadlineExceeded, errors.New("interrupted (9)"), context.DeadlineExceeded} {
		rec := httptest.NewRecorder()
		_, ok := guardLookup(rec, httptest.NewRequest("GET", "/api/v1/traces", nil), "key",
			func(ctx context.Context) (int, error) {
				<-ctx.Done()
				return 0, reported
			})
		if ok || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("lookup = %v, status %d, want a 503", ok, rec.Code)
		}
	}
	if lines := strings.Count(logged.String(), "lookup failed"); lines != 1 {
		t.Errorf("logged %d lines for 3 lookups past their deadline, want 1:\n%s", lines, logged.String())
	}
	if !strings.Contains(logged.String(), "condition=deadline") {
		t.Errorf("the line does not say the deadline passed:\n%s", logged.String())
	}
}

// A handler's own lookup of the project or account its route names answers a
// database condition as the guard does (spec 043 #24 u): `503` with
// `Retry-After`, logged once a minute per condition. Anything else is still a
// `500` — here the admin token's read of the project it names.
func TestAHandlersLookupOnAConditionIs503(t *testing.T) {
	h := newAdminHarness(t)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	lookupLog = &logpace.Keyed{Every: time.Minute}
	t.Cleanup(func() { projectByID = (*store.Store).ProjectByID })
	failWith := func(err error) {
		projectByID = func(*store.Store, context.Context, string) (*store.Project, error) { return nil, err }
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/projects/"+h.project.ID, nil)
		asAdmin(req)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		return rec
	}

	failWith(fmt.Errorf("read project: %w", codedError{13})) // SQLITE_FULL
	for range 3 {
		rec := get()
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("status %d, Retry-After %q, want 503 with Retry-After",
				rec.Code, rec.Header().Get("Retry-After"))
		}
	}
	if lines := strings.Count(logged.String(), "condition=SQLITE_FULL"); lines != 1 {
		t.Errorf("logged %d lines for 3 lookups a full disk failed, want 1:\n%s", lines, logged.String())
	}

	failWith(errors.New("no such table: projects"))
	if rec := get(); rec.Code != http.StatusInternalServerError {
		t.Errorf("status %d for a lookup that failed on its own, want 500", rec.Code)
	}
}

// A slide a database condition failed is not logged by the handler (spec 043
// #24 u): the writer logs the condition once a minute, and a line per request
// from every signed-in person would bury it. The request still succeeds.
func TestASlideADatabaseConditionFailedIsNotLogged(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	if err := h.writer.Submit(t.Context(), &store.SessionSlide{
		SessionID: store.SessionID(who.cookie),
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixNano(),
		Now:       time.Now().Add(-2 * sessionSlideAfter).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h.server.writer = conditionWriter{fmt.Errorf("commit write transaction: %w", codedError{13})}

	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who)), 200)
	if strings.Contains(logged.String(), "slide") {
		t.Errorf("a slide the database refused was logged by the handler:\n%s", logged.String())
	}
}

// A lookup outside the guard whose client hung up is not a storage failure
// either (spec 043 #24): nothing answered, nothing logged — here the admin
// token's own read of the project it names.
func TestAHangUpDuringAHandlersLookupIsNotAFailure(t *testing.T) {
	h := newAdminHarness(t)
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/projects/"+h.project.ID, nil).WithContext(ctx)
	asAdmin(req)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Errorf("status = 500 for a client that hung up, want nothing answered")
	}
	if strings.Contains(logged.String(), "lookup failed") {
		t.Errorf("a hang-up was logged as a failed lookup:\n%s", logged.String())
	}
}

// Encoding the answer whole before writing it changed nothing a client reads
// (spec 043 #3, #24 u): the bytes are json.Encoder's — HTML-escaped, with
// U+2028 escaped, ending in a newline — for a plain value and for an object.
func TestAnAnswerIsTheBytesTheEncoderWrote(t *testing.T) {
	for _, body := range []any{
		map[string]any{"html": "<b>&</b>", "line": "a\u2028b", "n": 1.5},
		object{}.put("html", "<script>").put("cost", math.Inf(1)).put("list", []int{1, 2}),
		[]string{"x"},
	} {
		var want bytes.Buffer
		if err := json.NewEncoder(&want).Encode(body); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		writeJSON(rec, http.StatusOK, body)
		if rec.Body.String() != want.String() {
			t.Errorf("wrote %q, want %q", rec.Body.String(), want.String())
		}
	}
}

// A response is encoded before its status is written, so a value that does
// not encode is a `500` that says so rather than a `200` with an empty body
// (spec 043 #3). An infinity inside a map is what the render backstop of #7
// does not reach.
func TestAResponseThatDoesNotEncodeIs500(t *testing.T) {
	captureLogs(t)
	rec := httptest.NewRecorder()
	// What a prompt read sets before it renders: a failure must not be
	// kept for the minute its answer would have been.
	rec.Header().Set("Cache-Control", promptCacheControl)
	rec.Header().Set("ETag", `"v1"`)
	rec.Header().Set("Last-Modified", "Sat, 26 Sep 2026 10:00:00 GMT")
	writeJSON(rec, http.StatusOK, map[string]any{"nested": map[string]any{"cost": math.Inf(1)}})
	expectError(t, rec, http.StatusInternalServerError, "failed to render the response")
	if !json.Valid(rec.Body.Bytes()) {
		t.Errorf("body = %q, want JSON", rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on a failed render, want no-store", got)
	}
	for _, name := range []string{"ETag", "Last-Modified"} {
		if got := rec.Header().Get(name); got != "" {
			t.Errorf("%s = %q on a failed render, want none", name, got)
		}
	}

	// And a body that encodes is the bytes it always was.
	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]any{"a": 1})
	if rec.Code != http.StatusCreated || rec.Body.String() != "{\"a\":1}\n" {
		t.Errorf("status = %d, body = %q, want 201 and the encoder's bytes", rec.Code, rec.Body)
	}
}

// A non-finite number in a rendered object is `null`, in a float64 and a
// *float64 alike (spec 043 #7).
func TestANonFiniteNumberRendersAsNull(t *testing.T) {
	captureLogs(t)
	inf := math.Inf(1)
	var none *float64
	body, err := json.Marshal(object{}.
		put("a", math.Inf(-1)).put("b", &inf).put("c", math.NaN()).
		put("d", 1.5).put("e", none))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":null,"b":null,"c":null,"d":1.5,"e":null}`; string(body) != want {
		t.Errorf("rendered %s, want %s", body, want)
	}
}

// expectJSON asserts a 200 whose body is a JSON document — the answer a
// project with a poisoned number used to give was a 200 with nothing in it.
func expectJSON(t *testing.T, rec *httptest.ResponseRecorder, path string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("%s: status = %d, body = %.200q, want 200 with a JSON body", path, rec.Code, rec.Body)
	}
}

// Two costs near the largest double used to sum to an infinity in the trace's
// total, and every listing, statistic and user page of the project answered
// `200` with an empty body (spec 043 #4). Neither counts now, live or rolled.
func TestNoCostPoisonsAProject(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	start := statsHour * int64(time.Second)
	trace := &model.Trace{ID: traceHex(1), UserID: "u1", SessionID: "s1", Environment: "production"}
	generation := func(n int) *model.Observation {
		return &model.Observation{TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
			Level: model.LevelDefault, Model: "m", StartTime: start + int64(n)*ms, EndTime: start + 100*ms,
			CostDetails: map[string]any{"total": 1.5e308}}
	}
	h.seed(t, trace, generation(1), generation(2))

	check := func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/traces", "/api/v1/traces/" + trace.ID,
			"/api/v1/stats?group_by=hour", "/api/v1/stats?group_by=model",
			"/api/v1/users", "/api/v1/users/u1", "/api/v1/sessions", "/api/v1/sessions/s1",
		} {
			expectJSON(t, h.get(t, path), path)
		}
		detail := decodeJSON[map[string]any](t, h.get(t, "/api/v1/traces/"+trace.ID))
		if cost, ok := detail["total_cost"]; ok {
			t.Errorf("total_cost = %v, want the key absent: costs nothing can add up are no data", cost)
		}
	}
	t.Run("live", check)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	t.Run("rolled", check)
}

// Two components near the largest double summed to an infinity the store
// could not encode, and the whole export failed with them (spec 043 #5). The
// components are kept; no total is derived.
func TestComponentsThatDoNotSumAreKept(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	span := otlptest.ProbeSpan("gen_ai.system", "anthropic",
		"langfuse.observation.cost_details", `{"input": 1e308, "output": 1e308}`)
	body, err := mapping.EncodeExportRequest(otlptest.Export(span))
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.post(t, "/v1/traces", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want the export stored", rec.Code, rec.Body)
	}
	observations, err := h.store.Observations(h.project.ID, "00112233445566778899aabbccddeeff", store.WithIO)
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations = %d, %v", len(observations), err)
	}
	cost := observations[0].CostDetails
	if cost["input"] != 1e308 || cost["output"] != 1e308 {
		t.Errorf("cost_details = %v, want the components as sent", cost)
	}
	if total, ok := cost["total"]; ok {
		t.Errorf("total = %v, want none derived from a sum that is not finite", total)
	}
}

// A `total` sent as a string failed the scan of every pass over its hour, so
// the project's statistics stopped for good and the trace could not be
// deleted (spec 043 #4, #8). It is no data now, and both go through.
func TestAStringCostStopsNothing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	start := statsHour * int64(time.Second)
	h.seed(t, &model.Trace{ID: traceHex(1), Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Level: model.LevelDefault, Model: "m", StartTime: start, EndTime: start + 100*ms,
			CostDetails: map[string]any{"total": "abc"}})

	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	state, err := h.store.RollupState(h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil <= statsHour {
		t.Errorf("rolled_until = %d, want past the hour the string cost sits in", state.RolledUntil)
	}
	buckets := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(buckets) != 1 || buckets[0].Count != 1 {
		t.Errorf("buckets = %+v, want the hour rolled with its one trace", buckets)
	}

	path := "/api/v1/traces/" + traceHex(1) + "?confirm=" + traceHex(1)
	expectStatus(t, h.call(t, "DELETE", path, nil), http.StatusOK)
}

// A token count of 1e300 was cast to the largest int64 and summed: the live
// scan raised `integer overflow`, the rollup wrapped negative (spec 043 #4).
// It is not a count now, on either half.
func TestATokenCountOutOfRangeIsNoData(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedTokens(t, statsHour, 1, "production", "m", map[string]any{"input_tokens": 1e300, "output_tokens": 5})
	h.seedTokens(t, statsHour, 2, "production", "m", map[string]any{"input_tokens": 1e300, "output_tokens": 5})

	check := func(t *testing.T) {
		for _, groupBy := range []string{"day", "model"} {
			buckets := h.tokensBuckets(t, "/api/v1/stats?group_by="+groupBy)
			if len(buckets) != 1 || buckets[0].Tokens == nil {
				t.Fatalf("%s buckets = %+v, want one with tokens", groupBy, buckets)
			}
			tokens := buckets[0].Tokens
			if tokens.Input != nil {
				t.Errorf("%s input = %d, want no data for counts out of range", groupBy, *tokens.Input)
			}
			if tokens.Output == nil || *tokens.Output != 10 {
				t.Errorf("%s output = %v, want 10", groupBy, tokens.Output)
			}
		}
	}
	t.Run("live", check)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	t.Run("rolled", check)
}

// A completion start of the smallest int64, minus a positive start, overflowed
// into a real the STRICT `ttft_ms` column refused, and the batch failed with
// it (spec 043 #5). Only a positive completion start takes part now.
func TestACompletionStartBeforeTheEpochHasNoWait(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	span := otlptest.ProbeSpan("gen_ai.system", "anthropic", "gen_ai.request.model", "m")
	span.Attributes = append(span.Attributes,
		otlptest.Int("langfuse.observation.completion_start_time", math.MinInt64))
	body, err := mapping.EncodeExportRequest(otlptest.Export(span))
	if err != nil {
		t.Fatal(err)
	}
	if rec := h.post(t, "/v1/traces", body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want the export stored", rec.Code, rec.Body)
	}
	detail := decodeJSON[map[string]any](t, h.get(t, "/api/v1/traces/00112233445566778899aabbccddeeff"))
	if ttft, ok := detail["ttft_ms"]; ok {
		t.Errorf("trace ttft_ms = %v, want none", ttft)
	}
	observations, _ := detail["observations"].([]any)
	if len(observations) != 1 {
		t.Fatalf("observations = %v", detail["observations"])
	}
	if ttft, ok := observations[0].(map[string]any)["ttft_ms"]; ok {
		t.Errorf("observation ttft_ms = %v, want none", ttft)
	}
}
