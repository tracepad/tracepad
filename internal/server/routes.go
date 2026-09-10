package server

import (
	"net/http"
)

// The router is a table (spec 004 Decision 25). One list of routes feeds three
// things that would otherwise drift apart: what the mux serves, what
// `GET /api/v1` tells an agent it can call, and what the OpenAPI document
// promises. A route added to the table appears in all three; a route added
// anywhere else fails the parity test.
//
// Since spec 028 it feeds a fourth: the policy column *is* the permission
// matrix (Decision 7). One guard reads it before every handler (auth.go), a
// parity test fails for a route that declares none, and a six-caller test
// walks the whole table asserting the matrix status by status — which is what
// turns "a viewer cannot revoke keys" from a belief into a fact.

// route is one served path with the one-line description an agent orients by
// and the policy that says who may call it.
type route struct {
	Method      string
	Path        string
	Policy      policy
	Description string
	handler     http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/health", public, "Liveness and version, no authentication required", s.handleHealth},

		// Ingest: the canonical OTLP path and the Langfuse-SDK alias are
		// one endpoint (spec 002 #2). A project key and nothing else: a
		// person's browser never writes spans (spec 028 Decision 3).
		{"POST", "/v1/traces", ingest, "OTLP/HTTP protobuf trace ingest", s.handleTraces},
		{"POST", "/api/public/otel/v1/traces", ingest, "OTLP ingest under the Langfuse SDK's path", s.handleTraces},

		// Self-description: where an agent that has never seen this API
		// starts (design §3.2).
		{"GET", "/api/v1", public, "This endpoint map", s.handleAPIIndex},
		{"GET", "/api/v1/openapi.json", public, "The OpenAPI 3.1 document for this API", s.handleOpenAPI},
		{"GET", "/api/v1/system", member, "Version, uptime, database size and ingest counters since start", s.handleSystem},

		// Signing in (spec 028 Decisions 8–10). The three public ones are
		// the three ways in — the first owner, a password, an invitation
		// — and the rest are about the person already signed in, which is
		// what `session` means: a key or the admin token is told "not a
		// session" rather than "unauthorized".
		{"GET", "/api/v1/setup", public, "Whether this server still needs its first owner", s.handleGetSetup},
		{"POST", "/api/v1/setup", public, "Create the first owner from the token the server printed", s.handleSetup},
		{"POST", "/api/v1/auth/login", public, "Sign in with an email and a password", s.handleLogin},
		{"POST", "/api/v1/auth/accept-invite", public, "Set a password from an invitation link and sign in", s.handleAcceptInvite},
		{"POST", "/api/v1/auth/logout", session, "End this session", s.handleLogout},
		{"GET", "/api/v1/auth/me", session, "The signed-in account and the projects it can reach", s.handleMe},
		{"PATCH", "/api/v1/auth/me", session, "Change this account's display name or password", s.handlePatchMe},
		{"GET", "/api/v1/auth/sessions", session, "This account's sessions, the current one marked", s.handleListSessionsOfAccount},
		{"DELETE", "/api/v1/auth/sessions", session, "End every session of this account but the current one", s.handleEndOtherSessions},

		// Traces.
		{"GET", "/api/v1/traces", member, "List traces newest first, filtered and cursor-paginated", s.handleListTraces},
		{"GET", "/api/v1/traces/last", member, "The newest trace matching the filters, whole", s.handleLastTrace},
		{"GET", "/api/v1/traces/{id}", member, "One trace with its observations as a nested tree", s.handleGetTrace},
		{"GET", "/api/v1/observations/{id}/io", member, "The whole input, output and metadata of one observation", s.handleObservationIO},

		// The raw archive (spec 019): what arrived, in the order it
		// arrived, and one body exactly as the client sent it. This is
		// what `tracepad export --otlp` replays, and what any other
		// client — a script, a backup job, a second Tracepad — reads to
		// take the data out (#2, #9).
		{"GET", "/api/v1/raw", member, "List the raw export bodies oldest first, cursor-paginated", s.handleListRaw},
		{"GET", "/api/v1/raw/{id}", member, "One raw export body, in the Content-Type it was received in", s.handleGetRawBatch},

		// Sessions and statistics.
		{"GET", "/api/v1/sessions", member, "List sessions by most recent activity, filtered and cursor-paginated", s.handleListSessions},
		{"GET", "/api/v1/sessions/{id}", member, "One session: its totals and its traces", s.handleGetSession},
		{"GET", "/api/v1/stats", member, "Counts, errors, cost and latency percentiles per bucket", s.handleStats},
		// Quality beside the traffic (spec 025): a series per score
		// name over the same seam, the same filters and the same
		// buckets as the statistics next to it.
		{"GET", "/api/v1/stats/scores", member, "Score means, rates and category shares per bucket, one series per score name", s.handleScoreTrends},
		// The values the three many-valued filters can take (spec
		// 027 #2), answered through the same seam: the rollup behind
		// the watermark and the live scan past it, so a value first
		// seen a minute ago is already on the list.
		{"GET", "/api/v1/facets", member, "The environments, releases and trace names in a range, each with its trace count", s.handleFacets},

		// Users (spec 023): the rollup one dimension over. The listing
		// answers from it alone and trails the raw rows by the rollup's
		// lag; one user merges the live tail and is exact (#4).
		{"GET", "/api/v1/users", member, "List users by last seen, traffic, cost or errors, cursor-paginated", s.handleListUsers},
		{"GET", "/api/v1/users/{id}", member, "One user: traffic, sessions, cost, errors and latency", s.handleGetUser},

		// Scores (spec 003). Writing one is `member`, not `ingest` and
		// not `editor`: annotation is a viewer's job — the helper this
		// spec exists for scores traces and works a queue (spec 028
		// Decision 3) — and a project key still writes them as before.
		{"POST", "/api/v1/scores", member, "Write one score or an array of them", s.handleCreateScores},
		{"GET", "/api/v1/scores", member, "List scores, filtered and cursor-paginated", s.handleListScores},
		{"GET", "/api/v1/scores/{id}", member, "Fetch one score", s.handleGetScore},
		{"DELETE", "/api/v1/scores/{id}", member, "Retract one score; no dry run, a re-POST puts it back", s.handleDeleteScore},

		// Prompts (spec 003) and the version diff (spec 004 #21).
		{"GET", "/api/v1/prompts", member, "List prompt names with where their labels point", s.handleListPrompts},
		{"GET", "/api/v1/prompts/{name}", member, "Fetch one prompt by version, by label, or the latest", s.handleGetPrompt},
		{"POST", "/api/v1/prompts/{name}/versions", editor, "Append a version to a prompt", s.handleCreatePromptVersion},
		{"GET", "/api/v1/prompts/{name}/versions", member, "List a prompt's versions, newest first", s.handleListPromptVersions},
		{"GET", "/api/v1/prompts/{name}/diff", member, "Unified diff between two versions of a prompt", s.handlePromptDiff},
		{"DELETE", "/api/v1/prompts/{name}", editor, "Delete a prompt with every version and label; a dry run until `?confirm=` echoes the name", s.handleDeletePrompt},
		{"PUT", "/api/v1/prompts/{name}/labels/{label}", editor, "Point a label at a version", s.handlePutPromptLabel},
		{"DELETE", "/api/v1/prompts/{name}/labels/{label}", editor, "Remove a label", s.handleDeletePromptLabel},

		// Datasets, items and runs (spec 014): the cases an eval ran,
		// versioned, and the container that groups the traces one pass
		// produced. The store executes nothing; the harness stays the
		// client's (spec 014 #1).
		{"GET", "/api/v1/datasets", member, "List datasets by name, cursor-paginated", s.handleListDatasets},
		{"PUT", "/api/v1/datasets/{name}", editor, "Create a dataset or replace its description and metadata", s.handlePutDataset},
		{"GET", "/api/v1/datasets/{name}", member, "One dataset: its version and counts", s.handleGetDataset},
		{"DELETE", "/api/v1/datasets/{name}", editor, "Delete a dataset with its items and runs; a dry run until `?confirm=` echoes the name", s.handleDeleteDataset},
		{"POST", "/api/v1/datasets/{name}/items", editor, "Add or edit items, one or an array, one version tick for the batch", s.handleCreateItems},
		{"GET", "/api/v1/datasets/{name}/items", member, "The items at a version, whole, in first-appearance order", s.handleListItems},
		{"GET", "/api/v1/datasets/{name}/items/{id}", member, "One item as of a version", s.handleGetItem},
		{"GET", "/api/v1/datasets/{name}/items/{id}/versions", member, "Every row of one item's history, newest first", s.handleListItemVersions},
		{"DELETE", "/api/v1/datasets/{name}/items/{id}", editor, "Archive an item at a new version", s.handleDeleteItem},
		{"POST", "/api/v1/datasets/{name}/runs", editor, "Open a run over the dataset at its current or a named version", s.handleCreateRun},
		{"GET", "/api/v1/datasets/{name}/runs", member, "List a dataset's runs newest first, cursor-paginated", s.handleListRuns},
		{"GET", "/api/v1/runs", member, "List the project's runs newest first, filtered by dataset and status, cursor-paginated", s.handleListProjectRuns},
		{"GET", "/api/v1/runs/{id}", member, "One run with its summary: coverage, traffic, scores, models and prompts", s.handleGetRun},
		{"GET", "/api/v1/runs/{id}/items", member, "The run's items with the attempts it made at each", s.handleRunItems},
		{"GET", "/api/v1/runs/{a}/compare/{b}", member, "Two runs of one dataset side by side, per score name and per item", s.handleCompareRuns},
		{"POST", "/api/v1/runs/{id}/finish", editor, "Close a run as finished, or as failed with a reason", s.handleFinishRun},
		{"DELETE", "/api/v1/runs/{id}", editor, "Delete a run, releasing its traces to the retention window", s.handleDeleteRun},

		// Score configs (spec 014 #15–#17): what a score's name means,
		// bound by name and checked on every write that uses it.
		{"GET", "/api/v1/score-configs", member, "List score configs by name", s.handleListScoreConfigs},
		{"PUT", "/api/v1/score-configs/{name}", editor, "Create or replace the config that binds a score name", s.handlePutScoreConfig},
		{"GET", "/api/v1/score-configs/{name}", member, "One score config", s.handleGetScoreConfig},
		{"DELETE", "/api/v1/score-configs/{name}", editor, "Remove a score config; the scores it admitted stay", s.handleDeleteScoreConfig},

		// Annotation queues (spec 024): what to review, who reviewed
		// it, and the hand-out that keeps two people off one trace.
		// `next` is a GET that writes — it claims — because claiming is
		// what being handed an item means (#5).
		//
		// Managing a queue is `editor`; working one is `member`, for the
		// reason writing a score is (spec 028 Decision 3).
		{"GET", "/api/v1/queues", member, "List the annotation queues with their progress", s.handleListQueues},
		{"PUT", "/api/v1/queues/{name}", editor, "Create an annotation queue or replace it whole", s.handlePutQueue},
		{"GET", "/api/v1/queues/{name}", member, "One queue: its score configs and its counts", s.handleGetQueue},
		{"DELETE", "/api/v1/queues/{name}", editor, "Delete a queue with its items; a dry run until `?confirm=` echoes the name. The scores stay", s.handleDeleteQueue},
		{"POST", "/api/v1/queues/{name}/items", editor, "Add one target or an array of them; a target already queued counts as existing", s.handleAddItems},
		{"POST", "/api/v1/queues/{name}/items/from-traces", editor, "Add the newest traces a listing filter matches, capped by `limit`", s.handleAddItemsFromTraces},
		{"GET", "/api/v1/queues/{name}/items", member, "The queue's items oldest first, filtered and cursor-paginated", s.handleListQueueItems},
		{"GET", "/api/v1/queues/{name}/next", member, "The next item to annotate, claimed for ten minutes", s.handleNextItem},
		{"GET", "/api/v1/queues/{name}/items/{id}", member, "One item", s.handleGetQueueItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/complete", member, "Mark an item done; refused unless every score the queue asks for is on its target", s.handleCompleteItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/skip", member, "Mark an item skipped, with the reason", s.handleSkipItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/reopen", member, "Return a completed or skipped item to pending", s.handleReopenItem},
		{"DELETE", "/api/v1/queues/{name}/items/{id}", editor, "Remove one item from the queue", s.handleDeleteQueueItem},

		// Administration (spec 005). Every destructive one is a dry run
		// until `?confirm=` echoes the name — or the user id — of what
		// it destroys (spec 005 #8). None of this reaches MCP, which
		// stays a read-only surface (spec 005 #13).
		//
		// The project *is* the thing an owner owns, so creating,
		// deleting, restoring and renaming one moved to `owner` (spec
		// 028 Decision 3); its settings — retention, keys, erasure — are
		// what an editor changes about a project it already has.
		{"GET", "/api/v1/projects", member, "List projects: all with the admin token, the caller's own with a key or a session", s.handleListProjects},
		{"POST", "/api/v1/projects", owner, "Create a project and its first key pair", s.handleCreateProject},
		{"GET", "/api/v1/projects/{id}", member, "One project with its retention windows", s.handleGetProject},
		{"PATCH", "/api/v1/projects/{id}", editor, "Rename a project (owners) or move its retention windows", s.handlePatchProject},
		{"DELETE", "/api/v1/projects/{id}", owner, "Soft-delete a project, restorable for seven days", s.handleDeleteProject},
		{"POST", "/api/v1/projects/{id}/restore", owner, "Undo a soft delete inside its grace window", s.handleRestoreProject},
		{"GET", "/api/v1/projects/{id}/keys", editor, "List a project's public keys", s.handleListKeys},
		{"POST", "/api/v1/projects/{id}/keys", editor, "Mint a key pair; the secret is shown once", s.handleCreateKey},
		{"DELETE", "/api/v1/projects/{id}/keys/{public_key}", editor, "Revoke one key pair", s.handleRevokeKey},
		{"DELETE", "/api/v1/projects/{id}/users/{user_id}/data", editor, "Erase everything stored about one user", s.handleEraseUserData},
		{"GET", "/api/v1/projects/{id}/members", owner, "Who has a role in this project; owners are not listed", s.handleProjectMembers},

		// Accounts (spec 028 Decision 12): an owner manages people from
		// the account side — a person and their projects — and reads
		// from the project side, which is the route above.
		{"GET", "/api/v1/accounts", owner, "Every account with its standing and its projects", s.handleListAccounts},
		{"POST", "/api/v1/accounts", owner, "Invite an account; the link is shown once", s.handleCreateAccount},
		{"GET", "/api/v1/accounts/{id}", owner, "One account with its projects", s.handleGetAccount},
		{"PATCH", "/api/v1/accounts/{id}", owner, "Change an account's name, owner standing or disabled flag", s.handlePatchAccount},
		{"DELETE", "/api/v1/accounts/{id}", owner, "Delete an account; a dry run until `?confirm=` echoes its email", s.handleDeleteAccount},
		{"POST", "/api/v1/accounts/{id}/invite", owner, "Mint a fresh invitation link; this is also the password reset", s.handleInviteAccount},
		{"PUT", "/api/v1/accounts/{id}/projects/{project_id}", owner, "Give an account a role in a project", s.handlePutMembership},
		{"DELETE", "/api/v1/accounts/{id}/projects/{project_id}", owner, "Take a project away from an account", s.handleDeleteMembership},
	}
}

// handleAPIIndex serves the endpoint map. An agent that lands on this API
// without documentation gets the whole surface in one response, in the order
// the table declares it (design §3.2), with the policy word that says what it
// would need to call each one (spec 028, API contract).
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
			put("policy", route.Policy.String()).
			put("description", route.Description))
	}
	writeJSON(w, http.StatusOK, object{}.
		put("version", s.version).
		put("openapi", "/api/v1/openapi.json").
		put("endpoints", endpoints))
}
