package server

import (
	"net/http"
)

// The router is a table (spec 004 Decision 25). One list of routes feeds three
// things that would otherwise drift apart: what the mux serves, what
// `GET /api/v1` tells an agent it can call, and what the OpenAPI document
// promises. A route added to the table appears in all three; a route added
// anywhere else fails the parity test.

// route is one served path with the one-line description an agent orients by.
type route struct {
	Method      string
	Path        string
	Description string
	handler     http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/health", "Liveness and version, no authentication required", s.handleHealth},

		// Ingest: the canonical OTLP path and the Langfuse-SDK alias are
		// one endpoint (spec 002 #2).
		{"POST", "/v1/traces", "OTLP/HTTP protobuf trace ingest", s.handleTraces},
		{"POST", "/api/public/otel/v1/traces", "OTLP ingest under the Langfuse SDK's path", s.handleTraces},

		// Self-description: where an agent that has never seen this API
		// starts (design §3.2).
		{"GET", "/api/v1", "This endpoint map", s.handleAPIIndex},
		{"GET", "/api/v1/openapi.json", "The OpenAPI 3.1 document for this API", s.handleOpenAPI},
		{"GET", "/api/v1/system", "Version, uptime, database size and ingest counters since start", s.handleSystem},

		// Traces.
		{"GET", "/api/v1/traces", "List traces newest first, filtered and cursor-paginated", s.handleListTraces},
		{"GET", "/api/v1/traces/last", "The newest trace matching the filters, whole", s.handleLastTrace},
		{"GET", "/api/v1/traces/{id}", "One trace with its observations as a nested tree", s.handleGetTrace},
		{"GET", "/api/v1/observations/{id}/io", "The whole input, output and metadata of one observation", s.handleObservationIO},

		// Sessions and statistics.
		{"GET", "/api/v1/sessions/{id}", "One session: its totals and its traces", s.handleGetSession},
		{"GET", "/api/v1/stats", "Counts, errors, cost and latency percentiles per bucket", s.handleStats},

		// Scores (spec 003).
		{"POST", "/api/v1/scores", "Write one score or an array of them", s.handleCreateScores},
		{"GET", "/api/v1/scores", "List scores, filtered and cursor-paginated", s.handleListScores},
		{"GET", "/api/v1/scores/{id}", "Fetch one score", s.handleGetScore},

		// Prompts (spec 003) and the version diff (spec 004 #21).
		{"GET", "/api/v1/prompts", "List prompt names with where their labels point", s.handleListPrompts},
		{"GET", "/api/v1/prompts/{name}", "Fetch one prompt by version, by label, or the latest", s.handleGetPrompt},
		{"POST", "/api/v1/prompts/{name}/versions", "Append a version to a prompt", s.handleCreatePromptVersion},
		{"GET", "/api/v1/prompts/{name}/versions", "List a prompt's versions, newest first", s.handleListPromptVersions},
		{"GET", "/api/v1/prompts/{name}/diff", "Unified diff between two versions of a prompt", s.handlePromptDiff},
		{"PUT", "/api/v1/prompts/{name}/labels/{label}", "Point a label at a version", s.handlePutPromptLabel},
		{"DELETE", "/api/v1/prompts/{name}/labels/{label}", "Remove a label", s.handleDeletePromptLabel},
	}
}

// handleAPIIndex serves the endpoint map. An agent that lands on this API
// without documentation gets the whole surface in one response, in the order
// the table declares it (design §3.2).
//
// No authentication, for the same reason as the OpenAPI document: this says
// what the API is, never what is in it, and a consumer deciding whether to
// talk to this server at all should not need a key to find out what it would
// be talking to.
func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request) {
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	endpoints := make([]object, 0, len(s.routes()))
	for _, route := range s.routes() {
		endpoints = append(endpoints, object{}.
			put("method", route.Method).
			put("path", route.Path).
			put("description", route.Description))
	}
	writeJSON(w, http.StatusOK, object{}.
		put("version", s.version).
		put("openapi", "/api/v1/openapi.json").
		put("endpoints", endpoints))
}
