package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Self-description and self-diagnosis (spec 004 #9, #10).

// TestOpenAPIMatchesRouter is the parity check Decision 9 asks for: the
// hand-authored document and the route table describe the same server, in both
// directions. A new endpoint that nobody documented fails here, and so does a
// documented endpoint nobody serves.
func TestOpenAPIMatchesRouter(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatalf("the embedded document is not JSON: %v", err)
	}

	documented := map[string]bool{}
	for path, operations := range document.Paths {
		for method := range operations {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}
	served := map[string]bool{}
	for _, route := range h.server.routes() {
		served[route.Method+" "+route.Path] = true
	}

	for operation := range served {
		if !documented[operation] {
			t.Errorf("%s is served but not in openapi.json", operation)
		}
	}
	for operation := range documented {
		if !served[operation] {
			t.Errorf("%s is in openapi.json but not served", operation)
		}
	}

	// The document is what an agent orients by, so every route also owes
	// it a description in the endpoint map.
	for _, route := range h.server.routes() {
		if route.Description == "" {
			t.Errorf("%s %s has no description", route.Method, route.Path)
		}
	}

	// And the scope a key needs, on every operation, in the table's word
	// (spec 045 #2, Testing #11).
	for _, route := range h.server.routes() {
		var operation struct {
			Scope string `json:"x-tracepad-scope"`
		}
		raw := document.Paths[route.Path][strings.ToLower(route.Method)]
		if raw == nil || json.Unmarshal(raw, &operation) != nil {
			continue // reported above
		}
		if operation.Scope != route.Scope.String() {
			t.Errorf("%s %s: x-tracepad-scope = %q, the table says %q",
				route.Method, route.Path, operation.Scope, route.Scope)
		}
	}
}

// TestSelfDescriptionIsServedWithoutAKey: deciding whether to talk to this
// server at all should not require a credential. Both endpoints describe the
// shape of the API and never the data in it. The second half of the test is
// what keeps that honest — a document that claimed a key was needed where the
// server asks for none is a lie a reader would act on.
func TestSelfDescriptionIsServedWithoutAKey(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, path := range []string{"/api/v1", "/api/v1/openapi.json"} {
		rec := h.call(t, "GET", path, nil, func(r *http.Request) {
			r.Header.Del("Authorization")
		})
		expectStatus(t, rec, 200)
		if !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s did not answer with JSON", path)
		}
	}

	// And the document agrees: both are marked as needing no security.
	var document struct {
		Paths map[string]map[string]struct {
			Security *[]any `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1", "/api/v1/openapi.json"} {
		security := document.Paths[path]["get"].Security
		if security == nil || len(*security) != 0 {
			t.Errorf("openapi.json says %s needs a key; the server does not ask for one", path)
		}
	}
}

// TestDocumentedQueryParametersAreAccepted closes the other half of the parity
// gap. The router test proves the paths agree; this proves the parameters do.
//
// The API refuses any query parameter it does not know (spec 003 #21), so a
// parameter that only exists in the document is not a documentation nit — it
// is a 400 for whoever believed the document. Every documented query parameter
// is sent, and the only answer refused here is "unknown query parameter": a
// complaint about the *value* means the server knew the name, which is what is
// being checked.
//
// Every method, not only GET (spec 005): `?confirm=` is documented on a PATCH
// and on three DELETEs, and a value that is not the echo the server wants
// changes nothing — which is exactly what makes it safe to send here.
func TestDocumentedQueryParametersAreAccepted(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
				Ref  string `json:"$ref"`
			} `json:"parameters"`
		} `json:"paths"`
		Components struct {
			Parameters map[string]struct {
				Name string `json:"name"`
				In   string `json:"in"`
			} `json:"parameters"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}

	// Path templates filled with values of the right shape, so the request
	// reaches the query parsing rather than stopping at the path.
	fill := strings.NewReplacer(
		"/traces/{id}", "/traces/"+traceHex(1),
		"/observations/{id}", "/observations/"+spanHex(1),
		"/sessions/{id}", "/sessions/s1",
		"/scores/{id}", "/scores/"+strings.Repeat("a", 32),
		"/prompts/{name}", "/prompts/support",
		"{label}", "production",
		"/projects/{id}", "/projects/"+h.project.ID,
		"{public_key}", testPublic,
		"{user_id}", "u1",
		"/datasets/{name}", "/datasets/golden",
		"/items/{id}", "/items/"+strings.Repeat("b", 32),
		"/runs/{id}", "/runs/"+strings.Repeat("c", 32),
		"/score-configs/{name}", "/score-configs/accuracy",
		"/queues/{name}", "/queues/review",
	)

	for path, operations := range document.Paths {
		for method, operation := range operations {
			for _, parameter := range operation.Parameters {
				name, in := parameter.Name, parameter.In
				if reference, found := strings.CutPrefix(parameter.Ref, "#/components/parameters/"); found {
					shared := document.Components.Parameters[reference]
					name, in = shared.Name, shared.In
				}
				if in != "query" || name == "" {
					continue
				}
				t.Run(strings.ToUpper(method)+" "+path+"?"+name, func(t *testing.T) {
					rec := h.call(t, strings.ToUpper(method),
						fill.Replace(path)+"?"+name+"=1", nil)
					if rec.Code != http.StatusBadRequest {
						return
					}
					message := decodeJSON[map[string]string](t, rec)["error"]
					if strings.Contains(message, "unknown query parameter") {
						t.Fatalf("%s %s documents %q but refuses it: %s",
							strings.ToUpper(method), path, name, message)
					}
				})
			}
		}
	}
}

func TestAPIIndexListsEveryRoute(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/api/v1")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Version   string `json:"version"`
		OpenAPI   string `json:"openapi"`
		Endpoints []struct {
			Method      string `json:"method"`
			Path        string `json:"path"`
			Description string `json:"description"`
		} `json:"endpoints"`
	}](t, rec)

	if body.OpenAPI != "/api/v1/openapi.json" {
		t.Errorf("openapi = %q, want the document's path", body.OpenAPI)
	}
	if len(body.Endpoints) != len(h.server.routes()) {
		t.Fatalf("endpoints = %d, want all %d routes", len(body.Endpoints), len(h.server.routes()))
	}
	found := slices.ContainsFunc(body.Endpoints, func(e struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Description string `json:"description"`
	}) bool {
		return e.Method == "GET" && e.Path == "/api/v1/traces/last"
	})
	if !found {
		t.Errorf("the endpoint map does not mention the task shortcut")
	}
}

// TestSystemReportsIngest checks the counter half of spec 002 #17: what the
// server saw, per dialect, since it started.
func TestSystemReportsIngest(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	// One real export, so the dialect counters have something to report.
	h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"),
		func(r *http.Request) { r.Header.Set("x-langfuse-ingestion-version", "3.6.1") })

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Version  string `json:"version"`
		Database struct {
			SizeBytes int64            `json:"size_bytes"`
			Rows      map[string]int64 `json:"rows"`
		} `json:"database"`
		WriterQueue struct {
			Capacity int `json:"capacity"`
		} `json:"writer_queue"`
		Counters struct {
			Since    string `json:"since"`
			Dialects map[string]struct {
				Batches     int64 `json:"batches"`
				SpansStored int64 `json:"spans_stored"`
			} `json:"dialects"`
			SDKVersions []string `json:"langfuse_ingestion_versions"`
		} `json:"counters"`
	}](t, rec)

	if body.Version != "test" {
		t.Errorf("version = %q, want the build's", body.Version)
	}
	if body.Database.SizeBytes == 0 {
		t.Errorf("database size = 0, want the file on disk")
	}
	if body.Database.Rows["traces"] < 2 {
		t.Errorf("rows = %v, want both the seeded and the exported trace", body.Database.Rows)
	}
	if body.WriterQueue.Capacity == 0 {
		t.Errorf("writer queue = %+v, want the depth that says whether writes keep up", body.WriterQueue)
	}
	langfuse := body.Counters.Dialects["langfuse"]
	if langfuse.Batches != 1 || langfuse.SpansStored != 2 {
		t.Errorf("langfuse counters = %+v, want the one export's two spans", langfuse)
	}
	if !slices.Contains(body.Counters.SDKVersions, "3.6.1") {
		t.Errorf("sdk versions = %v, want the header that was sent", body.Counters.SDKVersions)
	}
	if body.Counters.Since == "" {
		t.Errorf("counters do not say what window they cover")
	}
}

// TestSystemCountsOnlyTheAskingProject: a project key is a tenant credential,
// and whole-table counts told one tenant how much data the others held and how
// many keys they had. Cross-project access is what the admin token is for, and
// it does not exist yet (Decision 33, found in review of PR #5).
func TestSystemCountsOnlyTheAskingProject(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	// A second tenant with traces of its own, and two keys.
	other, err := h.store.CreateProject("other",
		store.KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		err := h.writer.Submit(t.Context(), &store.IngestBatch{
			ProjectID: other.ID,
			Traces:    []*model.Trace{{ID: traceHex(100 + i)}},
			Observations: []*model.Observation{{TraceID: traceHex(100 + i), ID: spanHex(i),
				Type: model.TypeSpan, Level: model.LevelDefault,
				StartTime: seedBase, EndTime: seedBase + ms}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// One trace for the asking project.
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Database struct {
			Rows map[string]int64 `json:"rows"`
		} `json:"database"`
	}](t, rec)

	if got := body.Database.Rows["traces"]; got != 1 {
		t.Errorf("traces = %d, want only this project's 1", got)
	}
	if got := body.Database.Rows["observations"]; got != 1 {
		t.Errorf("observations = %d, want only this project's 1", got)
	}
	if got := body.Database.Rows["api_keys"]; got != 1 {
		t.Errorf("api_keys = %d, want only this project's own key", got)
	}
	// How many tenants share the process is an operator fact and names
	// none of them; how much data they hold is not.
	if got := body.Database.Rows["projects"]; got != 2 {
		t.Errorf("projects = %d, want the count of tenants", got)
	}
	if _, reported := body.Database.Rows["payloads"]; reported {
		t.Errorf("payloads is reported, but it cannot be attributed to a project")
	}
}

// TestSystemCountersAreScopedToTheProject: the ingest counters are the same
// class of aggregate as the row counts. Left process-wide they would tell one
// tenant how much traffic another was sending and which SDKs it ran
// (Decision 33).
func TestSystemCountersAreScopedToTheProject(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	if _, err := h.store.CreateProject("other",
		store.KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"}); err != nil {
		t.Fatal(err)
	}

	// The other tenant exports, with an SDK version of its own.
	h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"),
		func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer tp-sk-other")
			r.Header.Set("x-langfuse-ingestion-version", "9.9.9-theirs")
		})

	// The asking project has sent nothing, and must be told exactly that.
	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	mine := decodeJSON[struct {
		Counters struct {
			Dialects    map[string]any `json:"dialects"`
			SDKVersions []string       `json:"langfuse_ingestion_versions"`
		} `json:"counters"`
	}](t, rec)

	if len(mine.Counters.Dialects) != 0 {
		t.Errorf("dialects = %v, want none: this project exported nothing", mine.Counters.Dialects)
	}
	if slices.Contains(mine.Counters.SDKVersions, "9.9.9-theirs") {
		t.Errorf("sdk versions = %v, want no sight of another tenant's SDK", mine.Counters.SDKVersions)
	}

	// And the tenant that did export sees its own.
	theirs := h.call(t, "GET", "/api/v1/system", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer tp-sk-other")
	})
	expectStatus(t, theirs, 200)
	body := decodeJSON[struct {
		Counters struct {
			Dialects map[string]struct {
				Batches int64 `json:"batches"`
			} `json:"dialects"`
			SDKVersions []string `json:"langfuse_ingestion_versions"`
		} `json:"counters"`
	}](t, theirs)
	if body.Counters.Dialects["langfuse"].Batches != 1 {
		t.Errorf("dialects = %+v, want the export it actually sent", body.Counters.Dialects)
	}
	if !slices.Contains(body.Counters.SDKVersions, "9.9.9-theirs") {
		t.Errorf("sdk versions = %v, want its own", body.Counters.SDKVersions)
	}
}

// TestSystemCountsRejections: a body that never decoded has no dialect, and
// saying so is the point of a separate counter.
func TestSystemCountsRejections(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.post(t, "/v1/traces", []byte("not protobuf at all"))

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Counters struct {
			RejectedBatches int64 `json:"rejected_batches"`
		} `json:"counters"`
	}](t, rec)
	if body.Counters.RejectedBatches != 1 {
		t.Errorf("rejected_batches = %d, want the one undecodable body", body.Counters.RejectedBatches)
	}
}

// TestOrphanRunLoggingIsOncePerBatch: the counter takes every trace, and the
// ids handed back to be logged are deduplicated within the call as well as
// across calls. Both halves matter past the bound, where an id is no longer
// remembered between exports: a hundred traces of one wrong id are one
// mistake and must not be a hundred log lines (found in review of PR #30).
func TestOrphanRunLoggingIsOncePerBatch(t *testing.T) {
	c := newCounters()
	const project = "p"

	repeated := make([]string, 20)
	for i := range repeated {
		repeated[i] = "run-a"
	}
	if fresh := c.observeOrphanRuns(project, repeated); len(fresh) != 1 {
		t.Errorf("first batch logged %d lines, want one for the one id", len(fresh))
	}
	if fresh := c.observeOrphanRuns(project, repeated); len(fresh) != 0 {
		t.Errorf("second batch logged %d lines, want none: the id is known", len(fresh))
	}
	if got := c.orphanTraces(project); got != 40 {
		t.Errorf("orphan_traces = %d, want every trace counted", got)
	}

	// Past the bound the map stops growing, and the per-call dedup is what
	// is left standing.
	for i := range maxTrackedUnknownRuns * 2 {
		c.observeOrphanRuns(project, []string{fmt.Sprintf("run-%d", i)})
	}
	if tracked := len(c.projects[project].unknownRuns); tracked > maxTrackedUnknownRuns {
		t.Fatalf("tracked %d ids, want at most %d: the id is client-controlled",
			tracked, maxTrackedUnknownRuns)
	}
	beyond := make([]string, 20)
	for i := range beyond {
		beyond[i] = "run-past-the-bound"
	}
	if fresh := c.observeOrphanRuns(project, beyond); len(fresh) != 1 {
		t.Errorf("a batch past the bound logged %d lines, want one", len(fresh))
	}
}

func TestSDKVersionTrackingIsBounded(t *testing.T) {
	c := newCounters()
	for i := range maxTrackedSDKVersions * 2 {
		c.observeSDKVersion("project", fmt.Sprintf("v%d", i))
	}
	if tracked := len(c.projects["project"].sdkVersions); tracked > maxTrackedSDKVersions {
		t.Fatalf("tracked %d versions, want at most %d: the header is client-controlled",
			tracked, maxTrackedSDKVersions)
	}
}

// TestSystemReportsTheSweeper is the observability half of spec 005 #14: with
// no run-now endpoint, `GET /api/v1/system` is where an operator finds out
// whether retention is running and what it has taken.
func TestSystemReportsTheSweeper(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Sweeper struct {
			Enabled           bool    `json:"enabled"`
			IntervalSeconds   int64   `json:"interval_seconds"`
			LastRun           *string `json:"last_run"`
			NextRun           string  `json:"next_run"`
			TracesDeleted     int64   `json:"traces_deleted"`
			RawBatchesDeleted int64   `json:"raw_batches_deleted"`
		} `json:"sweeper"`
	}](t, rec)

	if !body.Sweeper.Enabled {
		t.Fatalf("sweeper = %+v, want it reported as running", body.Sweeper)
	}
	if body.Sweeper.IntervalSeconds != int64(store.DefaultSweepInterval.Seconds()) {
		t.Errorf("interval = %ds, want the default cadence", body.Sweeper.IntervalSeconds)
	}
	// A sweeper that has not run says so, rather than reporting the epoch.
	if body.Sweeper.LastRun != nil {
		t.Errorf("last_run = %q before the first pass, want null", *body.Sweeper.LastRun)
	}
	if body.Sweeper.NextRun == "" {
		t.Errorf("sweeper = %+v, want the next pass named", body.Sweeper)
	}

	// After a pass that removed something, the counts are this project's.
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	if err := h.setRetention(h.project.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	rec = h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	after := decodeJSON[struct {
		Sweeper struct {
			LastRun       *string `json:"last_run"`
			TracesDeleted int64   `json:"traces_deleted"`
		} `json:"sweeper"`
	}](t, rec)
	if after.Sweeper.LastRun == nil {
		t.Errorf("last_run is still null after a pass")
	}
	if after.Sweeper.TracesDeleted != 1 {
		t.Errorf("traces_deleted = %d, want the one that expired", after.Sweeper.TracesDeleted)
	}
}
