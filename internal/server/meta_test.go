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
	)

	for path, operations := range document.Paths {
		operation, documented := operations["get"]
		if !documented {
			continue
		}
		for _, parameter := range operation.Parameters {
			name, in := parameter.Name, parameter.In
			if reference, found := strings.CutPrefix(parameter.Ref, "#/components/parameters/"); found {
				shared := document.Components.Parameters[reference]
				name, in = shared.Name, shared.In
			}
			if in != "query" || name == "" {
				continue
			}
			t.Run(path+"?"+name, func(t *testing.T) {
				rec := h.get(t, fill.Replace(path)+"?"+name+"=1")
				if rec.Code != http.StatusBadRequest {
					return
				}
				message := decodeJSON[map[string]string](t, rec)["error"]
				if strings.Contains(message, "unknown query parameter") {
					t.Fatalf("%s documents %q but refuses it: %s", path, name, message)
				}
			})
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

func TestSDKVersionTrackingIsBounded(t *testing.T) {
	c := newCounters()
	for i := range maxTrackedSDKVersions * 2 {
		c.observeSDKVersion(fmt.Sprintf("v%d", i))
	}
	if len(c.sdkVersions) > maxTrackedSDKVersions {
		t.Fatalf("tracked %d versions, want at most %d: the header is client-controlled",
			len(c.sdkVersions), maxTrackedSDKVersions)
	}
}
