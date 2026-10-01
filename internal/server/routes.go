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
//
// Since spec 045 a fifth: the scope column is what a project key must hold to
// be admitted (Decision 2). Declared route by route rather than derived from
// the policy and the method, because the exceptions — the score a production
// application writes, the queue read that claims — are exactly the decisions a
// reviewer should see in the table (Decision 3).

// route is one served path with the one-line description an agent orients by,
// the policy that says who may call it, and the scope a key needs.
type route struct {
	Method      string
	Path        string
	Policy      policy
	Scope       scope
	Description string
	handler     http.HandlerFunc
}

// Two GET routes the read gate treats apart (spec 043 #26, #27), named once for
// the table and for readBoundOf.
const (
	systemPath = "/api/v1/system"
	claimPath  = "/api/v1/queues/{name}/next"
)

// readBound is how the read gate bounds a route (spec 043 #15, #16).
type readBound uint8

const (
	// ungated is every route that is not a read: the public ones, every
	// write, and the one GET that writes.
	ungated readBound = iota
	// sharedSlots is a read: the deadline, and one of the read slots.
	sharedSlots
	// ownLane is the deadline and a slot of its own lane, for the read
	// that reports the slots.
	ownLane
)

// readBoundOf says how the gate bounds a route. It sits beside the table so
// that a route added to it is judged here too: a new GET that writes, or that
// reports the gate's own gauges, is named in this switch, and every other GET
// with a credential is a read.
func readBoundOf(rt route) readBound {
	if rt.Method != http.MethodGet || rt.Policy == public {
		return ungated
	}
	switch rt.Path {
	case claimPath:
		// Handing out the next item claims it through the writer
		// (spec 024 #5), and a claim the deadline answered would still
		// commit (spec 043 #26).
		return ungated
	case systemPath:
		// The gauge answers while every read slot is taken, and its
		// counts take one slot of their own (spec 043 #27).
		return ownLane
	}
	return sharedSlots
}

// bodyBudgetedOf says whether a route's body counts against the body budget
// (spec 043 #13): every route that can carry one — anything but a GET — except
// the public ones, whose bodies are a few KiB and whose callers nobody has
// identified (spec 028 #26). It sits beside the table for the reason
// readBoundOf does.
func bodyBudgetedOf(rt route) bool {
	return rt.Method != http.MethodGet && rt.Policy != public
}

func (s *Server) routes() []route {
	return []route{
		{"GET", "/health", public, scopeAny, "Liveness and version, no authentication required", s.handleHealth},

		// Ingest: the canonical OTLP path and the Langfuse-SDK alias are
		// one endpoint (spec 002 #2). A project key and nothing else: a
		// person's browser never writes spans (spec 028 Decision 3).
		{"POST", "/v1/traces", ingest, scopeIngest, "OTLP/HTTP protobuf trace ingest", s.handleTraces},
		{"POST", "/api/public/otel/v1/traces", ingest, scopeIngest, "OTLP ingest under the Langfuse SDK's path", s.handleTraces},
		// The Langfuse media channel (spec 041 #9): the SDK asks for an
		// upload URL, PUTs the bytes there, and reports back. The PUT is
		// presigned because the SDK sends it no credential — the token in
		// the URL is the check (Decision 14) — and the body is the picture,
		// capped at the length the token grants.
		{"POST", "/api/public/media", ingest, scopeIngest, "Langfuse SDK: an upload URL for one media body, or null when it is already stored", s.handleLangfuseMediaUpload},
		{"PUT", "/api/public/media/{mediaId}/upload", presigned, scopeAny, "Langfuse SDK: the presigned upload of one media body", s.handleLangfuseMediaPut},
		{"PATCH", "/api/public/media/{mediaId}", ingest, scopeIngest, "Langfuse SDK: the report on one media upload", s.handleLangfuseMediaPatch},
		{"GET", "/api/public/media/{mediaId}", ingest, scopeIngest, "Langfuse SDK: one media body's type, size and address", s.handleLangfuseMediaGet},

		// Self-description: where an agent that has never seen this API
		// starts (design §3.2).
		{"GET", "/api/v1", public, scopeAny, "This endpoint map", s.handleAPIIndex},
		{"GET", "/api/v1/openapi.json", public, scopeAny, "The OpenAPI 3.1 document for this API", s.handleOpenAPI},
		// The one route outside the project routes that also admits the
		// admin token, which gets the deployment view and no project's
		// (spec 004 #37).
		{"GET", systemPath, member, scopeRead, "Version and uptime, the deployment's gauges for the admin token or an owner, and the project's figures for its callers", s.handleSystem},

		// Signing in (spec 028 Decisions 8–10). The three public ones are
		// the three ways in — the first owner, a password, an invitation
		// — and the rest are about the person already signed in, which is
		// what `session` means: a key or the admin token is told "not a
		// session" rather than "unauthorized".
		{"GET", "/api/v1/setup", public, scopeAny, "Whether this server still needs its first owner", s.handleGetSetup},
		{"POST", "/api/v1/setup", public, scopeAny, "Create the first owner from the token the server printed", s.handleSetup},
		{"POST", "/api/v1/auth/login", public, scopeAny, "Sign in with an email and a password", s.handleLogin},
		{"POST", "/api/v1/auth/accept-invite", public, scopeAny, "Set a password from an invitation link and sign in", s.handleAcceptInvite},
		{"POST", "/api/v1/auth/logout", session, scopeNone, "End this session", s.handleLogout},
		{"GET", "/api/v1/auth/me", session, scopeNone, "The signed-in account and the projects it can reach", s.handleMe},
		{"PATCH", "/api/v1/auth/me", session, scopeNone, "Change this account's display name or password", s.handlePatchMe},
		{"GET", "/api/v1/auth/sessions", session, scopeNone, "This account's sessions, the current one marked", s.handleListSessionsOfAccount},
		{"DELETE", "/api/v1/auth/sessions", session, scopeNone, "End every session of this account but the current one", s.handleEndOtherSessions},

		// Traces.
		{"GET", "/api/v1/traces", member, scopeRead, "List traces newest first, filtered and cursor-paginated", s.handleListTraces},
		{"GET", "/api/v1/traces/last", member, scopeRead, "The newest trace matching the filters, whole", s.handleLastTrace},
		{"GET", "/api/v1/traces/{id}", member, scopeRead, "One trace with its observations as a nested tree", s.handleGetTrace},
		// Deleting traces (spec 035): a dry run until `confirm` echoes the
		// trace id, or the project's name for the bulk form.
		{"DELETE", "/api/v1/traces/{id}", editor, scopeWrite, "Delete one trace and everything attached to it", s.handleDeleteTrace},
		{"DELETE", "/api/v1/traces", editor, scopeWrite, "Delete every trace a listing filter matches before `to`, in bounded rounds", s.handleDeleteTraces},
		{"GET", "/api/v1/observations/{id}/io", member, scopeRead, "The whole input, output and metadata of one observation", s.handleObservationIO},
		// Media (spec 041 #7): one body by its hash, for a project that
		// points at it; `404` for any other.
		{"GET", "/api/v1/media/{sha256}", member, scopeRead, "One image or file a payload references, by the SHA-256 of its bytes", s.handleGetMedia},

		// The raw archive (spec 019): what arrived, in the order it
		// arrived, and one body exactly as the client sent it. This is
		// what `tracepad export --otlp` replays, and what any other
		// client — a script, a backup job, a second Tracepad — reads to
		// take the data out (#2, #9). `editor`, not `member`: the archive
		// is the bulk way out of a project, every body whole, and taking
		// a project's data out is an operator's act like its windows and
		// its keys — a viewer reads traces and annotates (spec 044 #6). A
		// key passes either way.
		{"GET", "/api/v1/raw", editor, scopeRead, "List the raw export bodies oldest first, cursor-paginated", s.handleListRaw},
		{"GET", "/api/v1/raw/{id}", editor, scopeRead, "One raw export body, in the Content-Type it was received in", s.handleGetRawBatch},

		// Sessions and statistics.
		{"GET", "/api/v1/sessions", member, scopeRead, "List sessions by most recent activity, filtered and cursor-paginated", s.handleListSessions},
		{"GET", "/api/v1/sessions/{id}", member, scopeRead, "One session: its totals and its traces", s.handleGetSession},
		{"GET", "/api/v1/stats", member, scopeRead, "Counts, errors, cost and latency percentiles per bucket", s.handleStats},
		// Quality beside the traffic (spec 025): a series per score
		// name over the same seam, the same filters and the same
		// buckets as the statistics next to it.
		{"GET", "/api/v1/stats/scores", member, scopeRead, "Score means, rates and category shares per bucket, one series per score name", s.handleScoreTrends},
		// The values the three many-valued filters can take (spec
		// 027 #2), answered through the same seam: the rollup behind
		// the watermark and the live scan past it, so a value first
		// seen a minute ago is already on the list.
		{"GET", "/api/v1/facets", member, scopeRead, "The environments, releases and trace names in a range, each with its trace count", s.handleFacets},

		// Users (spec 023): the rollup one dimension over. The listing
		// answers from it alone and trails the raw rows by the rollup's
		// lag; one user merges the live tail and is exact (#4).
		{"GET", "/api/v1/users", member, scopeRead, "List users by last seen, traffic, cost or errors, cursor-paginated", s.handleListUsers},
		{"GET", "/api/v1/users/{id}", member, scopeRead, "One user: traffic, sessions, cost, errors and latency", s.handleGetUser},

		// Scores (spec 003). Writing one is `member`, not `ingest` and
		// not `editor`: annotation is a viewer's job — the helper this
		// spec exists for scores traces and works a queue (spec 028
		// Decision 3) — and a project key still writes them as before.
		// For a key it is `ingest`: an end user's thumbs-up is written by
		// the production application, with the only key it holds (spec
		// 045 #3).
		{"POST", "/api/v1/scores", member, scopeIngest, "Write one score or an array of them", s.handleCreateScores},
		{"GET", "/api/v1/scores", member, scopeRead, "List scores, filtered and cursor-paginated", s.handleListScores},
		{"GET", "/api/v1/scores/{id}", member, scopeRead, "Fetch one score", s.handleGetScore},
		{"DELETE", "/api/v1/scores/{id}", member, scopeWrite, "Retract one score; no dry run, a re-POST puts it back", s.handleDeleteScore},

		// Prompts (spec 003) and the version diff (spec 004 #21). Fetching
		// one is any key's: an application fetches its prompts at run time
		// with the key it sends spans with (spec 045 #3).
		{"GET", "/api/v1/prompts", member, scopeRead, "List prompt names with where their labels point", s.handleListPrompts},
		{"GET", "/api/v1/prompts/{name}", member, scopeAny, "Fetch one prompt by version, by label, or the latest", s.handleGetPrompt},
		{"POST", "/api/v1/prompts/{name}/versions", editor, scopeWrite, "Append a version to a prompt", s.handleCreatePromptVersion},
		{"GET", "/api/v1/prompts/{name}/versions", member, scopeRead, "List a prompt's versions, newest first", s.handleListPromptVersions},
		{"GET", "/api/v1/prompts/{name}/diff", member, scopeRead, "Unified diff between two versions of a prompt", s.handlePromptDiff},
		{"DELETE", "/api/v1/prompts/{name}", editor, scopeWrite, "Delete a prompt with every version and label; a dry run until `?confirm=` echoes the name", s.handleDeletePrompt},
		{"PUT", "/api/v1/prompts/{name}/labels/{label}", editor, scopeWrite, "Point a label at a version", s.handlePutPromptLabel},
		{"DELETE", "/api/v1/prompts/{name}/labels/{label}", editor, scopeWrite, "Remove a label", s.handleDeletePromptLabel},

		// Datasets, items and runs (spec 014): the cases an eval ran,
		// versioned, and the container that groups the traces one pass
		// produced. The store executes nothing; the harness stays the
		// client's (spec 014 #1).
		{"GET", "/api/v1/datasets", member, scopeRead, "List datasets by name, cursor-paginated", s.handleListDatasets},
		{"PUT", "/api/v1/datasets/{name}", editor, scopeWrite, "Create a dataset or replace its description and metadata", s.handlePutDataset},
		{"GET", "/api/v1/datasets/{name}", member, scopeRead, "One dataset: its version and counts", s.handleGetDataset},
		{"DELETE", "/api/v1/datasets/{name}", editor, scopeWrite, "Delete a dataset with its items and runs; a dry run until `?confirm=` echoes the name", s.handleDeleteDataset},
		{"POST", "/api/v1/datasets/{name}/items", editor, scopeWrite, "Add or edit items, one or an array, one version tick for the batch", s.handleCreateItems},
		{"GET", "/api/v1/datasets/{name}/items", member, scopeRead, "The items at a version, whole, in first-appearance order", s.handleListItems},
		{"GET", "/api/v1/datasets/{name}/items/{id}", member, scopeRead, "One item as of a version", s.handleGetItem},
		{"GET", "/api/v1/datasets/{name}/items/{id}/versions", member, scopeRead, "Every row of one item's history, newest first", s.handleListItemVersions},
		{"DELETE", "/api/v1/datasets/{name}/items/{id}", editor, scopeWrite, "Archive an item at a new version", s.handleDeleteItem},
		{"POST", "/api/v1/datasets/{name}/runs", editor, scopeWrite, "Open a run over the dataset at its current or a named version", s.handleCreateRun},
		{"GET", "/api/v1/datasets/{name}/runs", member, scopeRead, "List a dataset's runs newest first, cursor-paginated", s.handleListRuns},
		{"GET", "/api/v1/runs", member, scopeRead, "List the project's runs newest first, filtered by dataset and status, cursor-paginated", s.handleListProjectRuns},
		{"GET", "/api/v1/runs/{id}", member, scopeRead, "One run with its summary: coverage, traffic, scores, models and prompts", s.handleGetRun},
		{"GET", "/api/v1/runs/{id}/items", member, scopeRead, "The run's items with the attempts it made at each", s.handleRunItems},
		{"GET", "/api/v1/runs/{a}/compare/{b}", member, scopeRead, "Two runs of one dataset side by side, per score name and per item", s.handleCompareRuns},
		{"POST", "/api/v1/runs/{id}/finish", editor, scopeWrite, "Close a run as finished, or as failed with a reason", s.handleFinishRun},
		{"DELETE", "/api/v1/runs/{id}", editor, scopeWrite, "Delete a run, releasing its traces to the retention window", s.handleDeleteRun},

		// Score configs (spec 014 #15–#17): what a score's name means,
		// bound by name and checked on every write that uses it.
		{"GET", "/api/v1/score-configs", member, scopeRead, "List score configs by name", s.handleListScoreConfigs},
		{"PUT", "/api/v1/score-configs/{name}", editor, scopeWrite, "Create or replace the config that binds a score name", s.handlePutScoreConfig},
		{"GET", "/api/v1/score-configs/{name}", member, scopeRead, "One score config", s.handleGetScoreConfig},
		{"DELETE", "/api/v1/score-configs/{name}", editor, scopeWrite, "Remove a score config; the scores it admitted stay", s.handleDeleteScoreConfig},

		// Annotation queues (spec 024): what to review, who reviewed
		// it, and the hand-out that keeps two people off one trace.
		// `next` is a GET that writes — it claims — because claiming is
		// what being handed an item means (#5).
		//
		// Managing a queue is `editor`; working one is `member`, for the
		// reason writing a score is (spec 028 Decision 3). A key needs
		// `write` for both, `next` included: a key that changes who is
		// handed what is not a read-only key (spec 045 #3).
		{"GET", "/api/v1/queues", member, scopeRead, "List the annotation queues with their progress", s.handleListQueues},
		{"PUT", "/api/v1/queues/{name}", editor, scopeWrite, "Create an annotation queue or replace it whole", s.handlePutQueue},
		{"GET", "/api/v1/queues/{name}", member, scopeRead, "One queue: its score configs and its counts", s.handleGetQueue},
		{"DELETE", "/api/v1/queues/{name}", editor, scopeWrite, "Delete a queue with its items; a dry run until `?confirm=` echoes the name. The scores stay", s.handleDeleteQueue},
		{"POST", "/api/v1/queues/{name}/items", editor, scopeWrite, "Add one target or an array of them; a target already queued counts as existing", s.handleAddItems},
		{"POST", "/api/v1/queues/{name}/items/from-traces", editor, scopeWrite, "Add the newest traces a listing filter matches, capped by `limit`", s.handleAddItemsFromTraces},
		{"GET", "/api/v1/queues/{name}/items", member, scopeRead, "The queue's items oldest first, filtered and cursor-paginated", s.handleListQueueItems},
		{"GET", claimPath, member, scopeWrite, "The next item to annotate, claimed for ten minutes", s.handleNextItem},
		{"GET", "/api/v1/queues/{name}/items/{id}", member, scopeRead, "One item", s.handleGetQueueItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/complete", member, scopeWrite, "Mark an item done; refused unless every score the queue asks for is on its target", s.handleCompleteItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/skip", member, scopeWrite, "Mark an item skipped, with the reason", s.handleSkipItem},
		{"POST", "/api/v1/queues/{name}/items/{id}/reopen", member, scopeWrite, "Return a completed or skipped item to pending", s.handleReopenItem},
		{"DELETE", "/api/v1/queues/{name}/items/{id}", editor, scopeWrite, "Remove one item from the queue", s.handleDeleteQueueItem},

		// Administration (spec 005). Every destructive one is a dry run
		// until `?confirm=` echoes the name — or the user id — of what
		// it destroys (spec 005 #8). None of this reaches MCP, which
		// stays a read-only surface (spec 005 #13).
		//
		// The project *is* the thing an owner owns, so creating,
		// deleting, restoring and renaming one moved to `owner` (spec
		// 028 Decision 3); its settings — retention, keys, erasure — are
		// what an editor changes about a project it already has. Its keys
		// are an editor's and never a key's, whatever its scopes (spec 045
		// #4); the listing and the read are any key's, which is how a key
		// learns what it holds (#12).
		{"GET", "/api/v1/projects", member, scopeAny, "List projects: all with the admin token, the caller's own with a key or a session", s.handleListProjects},
		{"POST", "/api/v1/projects", owner, scopeNone, "Create a project and its first key pair", s.handleCreateProject},
		{"GET", "/api/v1/projects/{id}", member, scopeAny, "One project with its retention windows", s.handleGetProject},
		{"PATCH", "/api/v1/projects/{id}", editor, scopeWrite, "Rename a project (owners) or move its retention windows", s.handlePatchProject},
		{"DELETE", "/api/v1/projects/{id}", owner, scopeNone, "Soft-delete a project, restorable for seven days", s.handleDeleteProject},
		{"POST", "/api/v1/projects/{id}/restore", owner, scopeNone, "Undo a soft delete inside its grace window", s.handleRestoreProject},
		{"GET", "/api/v1/projects/{id}/keys", editor, scopeNone, "List a project's keys, who minted each and when it was last used; not with a project key", s.handleListKeys},
		{"POST", "/api/v1/projects/{id}/keys", editor, scopeNone, "Mint a key pair; the secret is shown once. Not with a project key", s.handleCreateKey},
		{"DELETE", "/api/v1/projects/{id}/keys/{public_key}", editor, scopeNone, "Revoke one key pair; not with a project key", s.handleRevokeKey},
		{"DELETE", "/api/v1/projects/{id}/users/{user_id}/data", editor, scopeWrite, "Erase everything stored about one user: accepted as a task, or answered once it ends within `wait`", s.handleEraseUserData},
		// Watching an erasure belongs to whoever may start one, and the
		// listing names the user while one runs (spec 047 #14).
		{"GET", "/api/v1/projects/{id}/erasures", editor, scopeWrite, "The project's erasures, those under way first, then newest first, at most 100", s.handleListErasures},
		{"GET", "/api/v1/projects/{id}/erasures/{erasure_id}", editor, scopeWrite, "One erasure: its state, phase, progress and counts", s.handleGetErasure},
		{"GET", "/api/v1/projects/{id}/members", owner, scopeNone, "Who has a role in this project; owners are not listed", s.handleProjectMembers},

		// Accounts (spec 028 Decision 12): an owner manages people from
		// the account side — a person and their projects — and reads
		// from the project side, which is the route above.
		{"GET", "/api/v1/accounts", owner, scopeNone, "Every account with its standing and its projects", s.handleListAccounts},
		{"POST", "/api/v1/accounts", owner, scopeNone, "Invite an account; the link is shown once", s.handleCreateAccount},
		{"GET", "/api/v1/accounts/{id}", owner, scopeNone, "One account with its projects", s.handleGetAccount},
		{"PATCH", "/api/v1/accounts/{id}", owner, scopeNone, "Change an account's name, owner standing or disabled flag", s.handlePatchAccount},
		{"DELETE", "/api/v1/accounts/{id}", owner, scopeNone, "Delete an account; a dry run until `?confirm=` echoes its email", s.handleDeleteAccount},
		{"POST", "/api/v1/accounts/{id}/invite", owner, scopeNone, "Mint a fresh invitation link; this is also the password reset", s.handleInviteAccount},
		{"PUT", "/api/v1/accounts/{id}/projects/{project_id}", owner, scopeNone, "Give an account a role in a project", s.handlePutMembership},
		{"DELETE", "/api/v1/accounts/{id}/projects/{project_id}", owner, scopeNone, "Take a project away from an account", s.handleDeleteMembership},
	}
}

// handleAPIIndex serves the endpoint map. An agent that lands on this API
// without documentation gets the whole surface in one response, in the order
// the table declares it (design §3.2), with the policy word that says what it
// would need to call each one (spec 028, API contract) and the scope a key
// would need (spec 045 #2).
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
			put("scope", route.Scope.String()).
			put("description", route.Description))
	}
	writeJSON(w, http.StatusOK, object{}.
		put("version", s.version).
		put("openapi", "/api/v1/openapi.json").
		put("endpoints", endpoints))
}
