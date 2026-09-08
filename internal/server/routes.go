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

		// The raw archive (spec 019): what arrived, in the order it
		// arrived, and one body exactly as the client sent it. This is
		// what `tracepad export --otlp` replays, and what any other
		// client — a script, a backup job, a second Tracepad — reads to
		// take the data out (#2, #9).
		{"GET", "/api/v1/raw", "List the raw export bodies oldest first, cursor-paginated", s.handleListRaw},
		{"GET", "/api/v1/raw/{id}", "One raw export body, in the Content-Type it was received in", s.handleGetRawBatch},

		// Sessions and statistics.
		{"GET", "/api/v1/sessions", "List sessions by most recent activity, filtered and cursor-paginated", s.handleListSessions},
		{"GET", "/api/v1/sessions/{id}", "One session: its totals and its traces", s.handleGetSession},
		{"GET", "/api/v1/stats", "Counts, errors, cost and latency percentiles per bucket", s.handleStats},

		// Users (spec 023): the rollup one dimension over. The listing
		// answers from it alone and trails the raw rows by the rollup's
		// lag; one user merges the live tail and is exact (#4).
		{"GET", "/api/v1/users", "List users by last seen, traffic, cost or errors, cursor-paginated", s.handleListUsers},
		{"GET", "/api/v1/users/{id}", "One user: traffic, sessions, cost, errors and latency", s.handleGetUser},

		// Scores (spec 003).
		{"POST", "/api/v1/scores", "Write one score or an array of them", s.handleCreateScores},
		{"GET", "/api/v1/scores", "List scores, filtered and cursor-paginated", s.handleListScores},
		{"GET", "/api/v1/scores/{id}", "Fetch one score", s.handleGetScore},
		{"DELETE", "/api/v1/scores/{id}", "Retract one score; no dry run, a re-POST puts it back", s.handleDeleteScore},

		// Prompts (spec 003) and the version diff (spec 004 #21).
		{"GET", "/api/v1/prompts", "List prompt names with where their labels point", s.handleListPrompts},
		{"GET", "/api/v1/prompts/{name}", "Fetch one prompt by version, by label, or the latest", s.handleGetPrompt},
		{"POST", "/api/v1/prompts/{name}/versions", "Append a version to a prompt", s.handleCreatePromptVersion},
		{"GET", "/api/v1/prompts/{name}/versions", "List a prompt's versions, newest first", s.handleListPromptVersions},
		{"GET", "/api/v1/prompts/{name}/diff", "Unified diff between two versions of a prompt", s.handlePromptDiff},
		{"DELETE", "/api/v1/prompts/{name}", "Delete a prompt with every version and label; a dry run until `?confirm=` echoes the name", s.handleDeletePrompt},
		{"PUT", "/api/v1/prompts/{name}/labels/{label}", "Point a label at a version", s.handlePutPromptLabel},
		{"DELETE", "/api/v1/prompts/{name}/labels/{label}", "Remove a label", s.handleDeletePromptLabel},

		// Datasets, items and runs (spec 014): the cases an eval ran,
		// versioned, and the container that groups the traces one pass
		// produced. The store executes nothing; the harness stays the
		// client's (spec 014 #1).
		{"GET", "/api/v1/datasets", "List datasets by name, cursor-paginated", s.handleListDatasets},
		{"PUT", "/api/v1/datasets/{name}", "Create a dataset or replace its description and metadata", s.handlePutDataset},
		{"GET", "/api/v1/datasets/{name}", "One dataset: its version and counts", s.handleGetDataset},
		{"DELETE", "/api/v1/datasets/{name}", "Delete a dataset with its items and runs; a dry run until `?confirm=` echoes the name", s.handleDeleteDataset},
		{"POST", "/api/v1/datasets/{name}/items", "Add or edit items, one or an array, one version tick for the batch", s.handleCreateItems},
		{"GET", "/api/v1/datasets/{name}/items", "The items at a version, whole, in first-appearance order", s.handleListItems},
		{"GET", "/api/v1/datasets/{name}/items/{id}", "One item as of a version", s.handleGetItem},
		{"GET", "/api/v1/datasets/{name}/items/{id}/versions", "Every row of one item's history, newest first", s.handleListItemVersions},
		{"DELETE", "/api/v1/datasets/{name}/items/{id}", "Archive an item at a new version", s.handleDeleteItem},
		{"POST", "/api/v1/datasets/{name}/runs", "Open a run over the dataset at its current or a named version", s.handleCreateRun},
		{"GET", "/api/v1/datasets/{name}/runs", "List a dataset's runs newest first, cursor-paginated", s.handleListRuns},
		{"GET", "/api/v1/runs", "List the project's runs newest first, filtered by dataset and status, cursor-paginated", s.handleListProjectRuns},
		{"GET", "/api/v1/runs/{id}", "One run with its summary: coverage, traffic, scores, models and prompts", s.handleGetRun},
		{"GET", "/api/v1/runs/{id}/items", "The run's items with the attempts it made at each", s.handleRunItems},
		{"GET", "/api/v1/runs/{a}/compare/{b}", "Two runs of one dataset side by side, per score name and per item", s.handleCompareRuns},
		{"POST", "/api/v1/runs/{id}/finish", "Close a run as finished, or as failed with a reason", s.handleFinishRun},
		{"DELETE", "/api/v1/runs/{id}", "Delete a run, releasing its traces to the retention window", s.handleDeleteRun},

		// Score configs (spec 014 #15–#17): what a score's name means,
		// bound by name and checked on every write that uses it.
		{"GET", "/api/v1/score-configs", "List score configs by name", s.handleListScoreConfigs},
		{"PUT", "/api/v1/score-configs/{name}", "Create or replace the config that binds a score name", s.handlePutScoreConfig},
		{"GET", "/api/v1/score-configs/{name}", "One score config", s.handleGetScoreConfig},
		{"DELETE", "/api/v1/score-configs/{name}", "Remove a score config; the scores it admitted stay", s.handleDeleteScoreConfig},

		// Annotation queues (spec 024): what to review, who reviewed
		// it, and the hand-out that keeps two people off one trace.
		// `next` is a GET that writes — it claims — because claiming is
		// what being handed an item means (#5).
		{"GET", "/api/v1/queues", "List the annotation queues with their progress", s.handleListQueues},
		{"PUT", "/api/v1/queues/{name}", "Create an annotation queue or replace it whole", s.handlePutQueue},
		{"GET", "/api/v1/queues/{name}", "One queue: its score configs and its counts", s.handleGetQueue},
		{"DELETE", "/api/v1/queues/{name}", "Delete a queue with its items; a dry run until `?confirm=` echoes the name. The scores stay", s.handleDeleteQueue},
		{"POST", "/api/v1/queues/{name}/items", "Add one target or an array of them; a target already queued counts as existing", s.handleAddItems},
		{"POST", "/api/v1/queues/{name}/items/from-traces", "Add the newest traces a listing filter matches, capped by `limit`", s.handleAddItemsFromTraces},
		{"GET", "/api/v1/queues/{name}/items", "The queue's items oldest first, filtered and cursor-paginated", s.handleListQueueItems},
		{"GET", "/api/v1/queues/{name}/next", "The next item to annotate, claimed for ten minutes", s.handleNextItem},
		{"GET", "/api/v1/queues/{name}/items/{id}", "One item", s.handleGetQueueItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/complete", "Mark an item done; refused unless every score the queue asks for is on its target", s.handleCompleteItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/skip", "Mark an item skipped, with the reason", s.handleSkipItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/reopen", "Return a completed or skipped item to pending", s.handleReopenItem},
		{"DELETE", "/api/v1/queues/{name}/items/{id}", "Remove one item from the queue", s.handleDeleteQueueItem},

		// Administration (spec 005). Every destructive one is a dry run
		// until `?confirm=` echoes the name — or the user id — of what
		// it destroys (spec 005 #8). None of this reaches MCP, which
		// stays a read-only surface (spec 005 #13).
		{"GET", "/api/v1/projects", "List projects: all with the admin token, its own with a project key", s.handleListProjects},
		{"POST", "/api/v1/projects", "Create a project and its first key pair (admin token)", s.handleCreateProject},
		{"GET", "/api/v1/projects/{id}", "One project with its retention windows", s.handleGetProject},
		{"PATCH", "/api/v1/projects/{id}", "Rename a project or move its retention windows", s.handlePatchProject},
		{"DELETE", "/api/v1/projects/{id}", "Soft-delete a project, restorable for seven days (admin token)", s.handleDeleteProject},
		{"POST", "/api/v1/projects/{id}/restore", "Undo a soft delete inside its grace window", s.handleRestoreProject},
		{"GET", "/api/v1/projects/{id}/keys", "List a project's public keys", s.handleListKeys},
		{"POST", "/api/v1/projects/{id}/keys", "Mint a key pair; the secret is shown once", s.handleCreateKey},
		{"DELETE", "/api/v1/projects/{id}/keys/{public_key}", "Revoke one key pair", s.handleRevokeKey},
		{"DELETE", "/api/v1/projects/{id}/users/{user_id}/data", "Erase everything stored about one user", s.handleEraseUserData},
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
