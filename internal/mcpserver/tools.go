package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools (#17). Each maps 1:1 onto one endpoint.
//
// `search` is the tenth, and it is the one spec 004 #17 declined to ship until
// there was a search endpoint behind it — so that the tool would never claim a
// capability the server lacked. Spec 011 is that endpoint. It is a tool of its
// own rather than one more parameter on `list_traces` because the descriptions
// are triggers: "the user quotes text they saw" is a different question from
// "the user asks what ran", and a model choosing by description is better
// served by two. Both map onto the same endpoint.
//
// Descriptions are written as when-to-use triggers rather than as restatements
// of the endpoint — what question this answers, what comes back, what it does
// *not* do, and which neighbouring tool to reach for instead. That last part
// is what keeps a model from calling `list_traces` when it wanted the payloads
// of one run.

// toolset holds what every handler needs.
type toolset struct {
	api API
}

// call performs the endpoint request and packages its answer: the endpoint's
// own bytes as structuredContent (#16), and a short line of text for clients
// and models that read `content` (#18).
func (t *toolset) call(ctx context.Context, req *mcp.CallToolRequest, path string,
	query url.Values, summarize func(json.RawMessage) string) (*mcp.CallToolResult, any, error) {
	body, err := t.api.Get(ctx, path, query, credential(req))
	if err != nil {
		// A refusal by the API is a tool error, not a protocol error:
		// the model has to see it to correct itself, and the server's
		// own message already says what to fix.
		result := &mcp.CallToolResult{IsError: true}
		result.Content = []mcp.Content{&mcp.TextContent{Text: err.Error()}}
		return result, nil, nil
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: summarize(body)}},
		StructuredContent: json.RawMessage(body),
	}, nil, nil
}

// credential is the caller's own authorization, forwarded verbatim so that the
// read API applies exactly the permissions the MCP request arrived with. In
// stdio mode there is no HTTP request and the remote client uses its own key
// (#15, #20).
func credential(req *mcp.CallToolRequest) string {
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return ""
	}
	return req.Extra.Header.Get("Authorization")
}

// traceFilterProperties are the filters the listing and the shortcut share.
// They are the endpoint's own query parameters, with the endpoint's own names
// and meanings (MCP contract).
func traceFilterProperties() map[string]*jsonschema.Schema {
	return map[string]*jsonschema.Schema{
		"from":        timestamp("Only traces at or after this RFC 3339 instant."),
		"to":          timestamp("Only traces strictly before this RFC 3339 instant."),
		"environment": text("Exact match on the environment a trace ran in, e.g. \"production\"."),
		"user_id":     text("Exact match on the end user the trace was attributed to."),
		"session_id":  text("Exact match on the session the trace belongs to."),
		"name":        text("Exact match on the trace name."),
		"tag":         list(text("A tag."), "A trace must carry every tag listed."),
		"status": oneOf("\"error\" keeps traces with at least one failed observation, \"ok\" keeps the rest.",
			"error", "ok"),
		"min_cost": atLeast(0, "Only traces costing at least this much. A trace whose client reported no cost never matches."),
		// Descriptions are triggers, not field lists (spec 004 #17): they
		// say when to reach for the filter, because a model choosing
		// between nine of them is answering "which one does this
		// question need".
		"release": text("Use when the user names a deployment or asks whether a release changed something — " +
			"\"did 2026.8.30 make it slower\". Exact match on the release the trace ran in."),
		"version": text("Use when the user names a version of the application's own logic rather than a " +
			"deployment. Exact match."),
		"type": oneOf("Use when the user asks about a kind of step — a tool call, a guardrail, a retrieval — "+
			"and wants the traces that contain one. Exact: \"generation\" does not match \"embedding\".",
			"span", "generation", "event", "agent", "tool", "chain",
			"retriever", "guardrail", "evaluator", "embedding"),
		"prompt": text("Use when the user names a prompt and wants what it produced — \"which traces ran " +
			"support-answer\". Either \"name\" for every version of it, or \"name@7\" for one. A version is " +
			"a number, so pass a name containing an \"@\" as it stands."),
	}
}

// traceFilterInput is the Go side of those filters.
type traceFilterInput struct {
	From        string   `json:"from"`
	To          string   `json:"to"`
	Environment string   `json:"environment"`
	UserID      string   `json:"user_id"`
	SessionID   string   `json:"session_id"`
	Name        string   `json:"name"`
	Tag         []string `json:"tag"`
	Status      string   `json:"status"`
	MinCost     *float64 `json:"min_cost"`
	Release     string   `json:"release"`
	Version     string   `json:"version"`
	Type        string   `json:"type"`
	Prompt      string   `json:"prompt"`
}

func (f traceFilterInput) query() url.Values {
	query := url.Values{}
	set(query, "from", f.From)
	set(query, "to", f.To)
	set(query, "environment", f.Environment)
	set(query, "user_id", f.UserID)
	set(query, "session_id", f.SessionID)
	set(query, "name", f.Name)
	set(query, "status", f.Status)
	set(query, "release", f.Release)
	set(query, "version", f.Version)
	set(query, "type", f.Type)
	set(query, "prompt", f.Prompt)
	for _, tag := range f.Tag {
		if tag != "" {
			query.Add("tag", tag)
		}
	}
	if f.MinCost != nil {
		query.Set("min_cost", strconv.FormatFloat(*f.MinCost, 'f', -1, 64))
	}
	return query
}

// expansionProperties are `?expand=io` and `?budget=`, mirrored by name.
func expansionProperties(properties map[string]*jsonschema.Schema) map[string]*jsonschema.Schema {
	properties["expand"] = oneOf(
		"Set to \"io\" to inline each observation's input, output and metadata. "+
			"Leave unset to get the tree without payloads, which is much smaller.", "io")
	properties["budget"] = bounded(4096, 5242880,
		"Byte budget for the payloads of this response, default 51200. "+
			"The structure is never truncated; only payloads are.")
	return properties
}

type expansionInput struct {
	Expand string `json:"expand"`
	Budget *int   `json:"budget"`
}

func (e expansionInput) apply(query url.Values) url.Values {
	set(query, "expand", e.Expand)
	if e.Budget != nil {
		query.Set("budget", strconv.Itoa(*e.Budget))
	}
	return query
}

// pagingProperties are the page size and the cursor, shared by every listing.
func pagingProperties(properties map[string]*jsonschema.Schema) map[string]*jsonschema.Schema {
	properties["limit"] = bounded(minLimit, maxLimit, "Rows per page, 1 to 500. Default 50.")
	properties["cursor"] = text("A `next_cursor` or `prev_cursor` of a previous page. Opaque; pass it back unchanged.")
	return properties
}

// walkProperties are the two the trace and session listings add on top: which
// way to page, and whether to count (spec 009). Not part of `pagingProperties`
// on purpose — the scores and prompts listings take neither, and offering a
// parameter their endpoint answers with a 400 would be a tool that lies about
// itself.
func walkProperties(properties map[string]*jsonschema.Schema, counting bool) map[string]*jsonschema.Schema {
	properties["direction"] = oneOf(
		"Which way to page from the cursor. With no cursor, `next` is the newest page and `prev` the oldest — "+
			"use `prev` alone to jump straight to the far end of the listing. Rows come back newest first either way.",
		"next", "prev")
	if counting {
		properties["count"] = text("Pass `1` to also get `total`: how many rows the filters match, counted up to 1000. " +
			"`total_capped` says the real number is larger. Ask when the size of the result matters; skip it while paging.")
	}
	return properties
}

type pagingInput struct {
	Limit  *int   `json:"limit"`
	Cursor string `json:"cursor"`
}

func (p pagingInput) apply(query url.Values) url.Values {
	if p.Limit != nil {
		query.Set("limit", strconv.Itoa(*p.Limit))
	}
	set(query, "cursor", p.Cursor)
	return query
}

// walkInput is embedded beside pagingInput by the listings that take it.
type walkInput struct {
	Direction string `json:"direction"`
	Count     string `json:"count"`
}

func (w walkInput) apply(query url.Values) url.Values {
	set(query, "direction", w.Direction)
	set(query, "count", w.Count)
	return query
}

func set(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

// register adds every tool, in the order they are declared here. The SDK
// serves `tools/list` sorted by name, so the wire order is deterministic
// whatever this order is (#18) — a test holds that.
func register(server *mcp.Server, api API) {
	t := &toolset{api: api}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_traces",
		Annotations: readOnly("List traces"),
		Description: "Find traces by filter — the user asks what ran recently, which runs failed, " +
			"what a given user or session did, or how much something cost. " +
			"Returns a page of trace summaries (id, name, environment, timestamp, latency, cost, error count), newest first. " +
			"Does NOT return the observations inside a trace, or any prompt or completion text: call get_trace with an id for that, " +
			"or get_last_trace to go straight to the newest match without listing first. " +
			"Page by passing the returned next_cursor back as cursor, or prev_cursor with direction=prev to go back.",
		InputSchema: object(walkProperties(pagingProperties(withFields(traceFilterProperties())), true)),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"traces":       list(traceRowSchema(), "The page, newest first."),
			"next_cursor":  text("Pass back as `cursor` for the next page; null on the oldest page."),
			"prev_cursor":  text("Pass back as `cursor` with `direction=prev` for the page before; null on the newest page."),
			"total":        integer("Only with `count=1`: how many traces the filters match, capped at 1000."),
			"total_capped": boolean("Only with `count=1`: true when the count stopped at the cap and the real number is larger."),
		}, "traces"),
	}, t.listTraces)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Annotations: readOnly("Search traces by their text"),
		Description: "Find traces by what was said in them — the user quotes or paraphrases text they saw: " +
			"an error message, a phrase in an answer, an id from a support ticket — and wants the traces where it appears. " +
			"Searches the prompts, completions, metadata, names and status messages of every observation, and the trace names. " +
			"Words, not substrings: `error` does not find `errors`, `err*` finds both; \"quoted words\" must be adjacent; " +
			"all the words must occur in the same field of the same observation. Case and diacritics are folded. " +
			"Returns the same rows as list_traces, newest first, each carrying `match`: where the hit was and a short snippet, " +
			"so the next call can be get_observation_io on that observation. " +
			"Takes every filter list_traces takes, so a search can be narrowed to an environment, a user or a time window. " +
			"Use list_traces instead when the question is about labels — what ran, what failed, what it cost — rather than about text.",
		InputSchema: object(walkProperties(pagingProperties(withFields(withSearch(traceFilterProperties()))), true), "q"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"traces":       list(searchRowSchema(), "The page, newest first — not by relevance."),
			"next_cursor":  text("Pass back as `cursor` for the next page; null on the oldest page. A cursor is valid only with the same `q`."),
			"prev_cursor":  text("Pass back as `cursor` with `direction=prev` for the page before; null on the newest page."),
			"total":        integer("Only with `count=1`: how many traces the search matches, capped at 1000."),
			"total_capped": boolean("Only with `count=1`: true when the count stopped at the cap and the real number is larger."),
		}, "traces"),
	}, t.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_trace",
		Annotations: readOnly("Get one trace"),
		Description: "Read one trace whole — the user names a trace id, or you picked one out of list_traces and now need what happened inside it. " +
			"Returns the trace's fields and its observations as a nested tree, children inside their parents. " +
			"Payload text (input, output, metadata) comes back only with expand=io. " +
			"A payload too large for the response budget arrives as {\"truncated\": true, …} carrying trace_id and observation_id: " +
			"pass that pair to get_observation_io to read it whole. " +
			"Does NOT search — without an id, use list_traces or get_last_trace.",
		InputSchema: object(expansionProperties(map[string]*jsonschema.Schema{
			"trace_id": matching(traceIDPattern, "The 32 lower-case hex characters of the trace id."),
		}), "trace_id"),
		OutputSchema: traceDetailSchema(),
	}, t.getTrace)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_last_trace",
		Annotations: readOnly("Get the newest matching trace"),
		Description: "Answer \"why did the last run fail\" or \"show me the most recent run\" in one call. " +
			"Takes every filter list_traces takes and returns the newest match as a whole tree, exactly as get_trace would. " +
			"Prefer this over list_traces followed by get_trace whenever only the newest match matters. " +
			"Returns an error naming the filters when nothing matches.",
		InputSchema:  object(expansionProperties(traceFilterProperties())),
		OutputSchema: traceDetailSchema(),
	}, t.getLastTrace)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_observation_io",
		Annotations: readOnly("Get one observation's payloads"),
		Description: "Read one observation's full input, output and metadata. " +
			"Use it after a truncation marker in get_trace or get_last_trace output, passing the observation_id and trace_id the marker carries. " +
			"This is the only call no response budget applies to, so the payload comes back whole and may be large. " +
			"Omitting trace_id is allowed but returns an error when the same span id appears in more than one trace of the project.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"observation_id": matching(observationIDPattern, "The 16 lower-case hex characters of the observation id."),
			"trace_id": matching(traceIDPattern,
				"The trace the observation belongs to. Optional, but always present in a truncation marker — pass it."),
		}, "observation_id"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"trace_id":       text("The trace the observation belongs to."),
			"observation_id": text("The observation."),
			"input":          anything("Whatever the client logged, whole."),
			"output":         anything("Whatever the client logged, whole."),
			"metadata":       anything("The observation's metadata, whole."),
		}, "trace_id", "observation_id"),
	}, t.getObservationIO)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_sessions",
		Annotations: readOnly("List sessions"),
		Description: "Find conversations or agent sessions — the user asks which sessions ran, which were busiest or most expensive, " +
			"or what a given user has been doing lately. " +
			"Returns a page of session roll-ups (id, trace count, how many of those traces failed, cost, first and last activity), " +
			"most recently active first. Every number counts traces, not observations. " +
			"Does NOT return the traces themselves: follow with get_session for one session's traces, then get_trace for what happened inside one. " +
			"Page by passing the returned next_cursor back as cursor.",
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"from":        timestamp("Only traces at or after this RFC 3339 instant. A session appears when any of its traces falls in the window."),
			"to":          timestamp("Only traces strictly before this RFC 3339 instant."),
			"environment": text("Exact match on the environment the session's traces ran in, e.g. \"production\"."),
			"user_id":     text("Exact match on the end user the session's traces were attributed to."),
		}), true)),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"sessions":     list(sessionRowSchema(), "The page, most recent activity first."),
			"next_cursor":  text("Pass back as `cursor` for the next page; null on the oldest page."),
			"prev_cursor":  text("Pass back as `cursor` with `direction=prev` for the page before; null on the newest page."),
			"total":        integer("Only with `count=1`: how many sessions the filters match, capped at 1000."),
			"total_capped": boolean("Only with `count=1`: true when the count stopped at the cap."),
		}, "sessions"),
	}, t.listSessions)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_session",
		Annotations: readOnly("Get one session"),
		Description: "Summarize a conversation or an agent session — the user asks about a session id, or about everything that happened under one session. " +
			"Returns the session's totals (how many traces, how many of them failed, cost, first and last activity) and a page of its traces. " +
			"Every number counts traces, not observations. " +
			"Does NOT return what happened inside those traces: follow with get_trace.",
		// `direction` but no `count`: `trace_count` below is that number
		// already, and exactly (spec 009, API contract).
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"session_id": text("The session id, as the application set it."),
		}), false), "session_id"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"id":          text("The session id."),
			"trace_count": integer("How many traces name this session."),
			"total_cost":  number("Summed over the traces that reported a cost; absent when none did."),
			"error_count": integer("How many of the session's traces have a failed observation."),
			"first_seen":  timestamp("When the session's earliest trace started."),
			"last_seen":   timestamp("When its latest trace started."),
			"traces":      list(traceRowSchema(), "A page of the session's traces, newest first."),
			"next_cursor": text("Pass back as `cursor` for the next page of traces."),
		}, "id", "trace_count", "error_count", "traces"),
	}, t.getSession)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_users",
		Annotations: readOnly("List users"),
		Description: "Find end users — the user asks who the heaviest, most expensive or most error-prone users are, " +
			"or who has been active lately. " +
			"Returns a page of user roll-ups (id, traces, sessions, how many of those traces failed, cost, first and last activity), " +
			"sorted by the chosen key, always descending. Every number counts traces, not observations. " +
			"Answered from an hourly rollup, so it trails live traffic by a few minutes: a user first seen just now may not be listed, " +
			"and get_user is exact for any id. " +
			"Does NOT return the traces themselves: follow with list_traces filtered by user_id, or get_user for one user's totals. " +
			"Page by passing the returned next_cursor back as cursor.",
		InputSchema: object(walkProperties(pagingProperties(map[string]*jsonschema.Schema{
			"sort": oneOf("Which question this is. Default \"last_seen\".",
				"last_seen", "traces", "cost", "errors"),
			"prefix": text("Keeps ids starting with this, case-sensitively. A prefix, not a substring, and not a search."),
		}), true)),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"users":        list(userRowSchema(), "The page, in the chosen order."),
			"next_cursor":  text("Pass back as `cursor` for the next page; null on the last page."),
			"prev_cursor":  text("Pass back as `cursor` with `direction=prev` for the page before; null on the first page."),
			"total":        integer("Only with `count=1`: how many users match, capped at 1000."),
			"total_capped": boolean("Only with `count=1`: true when the count stopped at the cap."),
		}, "users"),
	}, t.listUsers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_user",
		Annotations: readOnly("Get one user"),
		Description: "Summarize one end user — the user asks what an account has been doing, what it costs, or when it was last seen. " +
			"Returns that user's totals (traces, sessions, failures, cost, first and last activity, p50/p95 latency), " +
			"exact including traffic too recent for the rollup, and 404 when nothing was ever filed under the id. " +
			"Does NOT return their traces or a timeline: use list_traces with user_id for the traces, " +
			"and get_stats with user_id for activity over time or a breakdown by model or environment.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"user_id": text("The user id, as the application set it."),
		}, "user_id"),
		OutputSchema: userSummarySchema(),
	}, t.getUser)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_prompt",
		Annotations: readOnly("Get a prompt version"),
		Description: "Read a stored prompt version — the user asks what prompt is in production, what a named prompt says, or which version a label points at. " +
			"Returns the prompt body verbatim with its config, its labels and its version number. " +
			"Selects by version, or by label, or the newest version when neither is given. " +
			"Does NOT write prompts and does NOT interpolate variables into them.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"name":    matching(namePattern, "The prompt's name."),
			"version": bounded(1, 1_000_000, "An exact version number. Mutually exclusive with label."),
			"label": text("A label such as \"production\". Mutually exclusive with version. " +
				"\"latest\" always resolves to the newest version."),
		}, "name"),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"name":           text("The prompt's name."),
			"version":        integer("Which version this is."),
			"type":           oneOf("A plain string, or an array of chat messages.", "text", "chat"),
			"prompt":         anything("The prompt body, verbatim as it was published."),
			"config":         anything("Model parameters published with this version."),
			"commit_message": text("Why this version was published."),
			"labels":         list(text("A label."), "Labels currently pointing at this version."),
			"created_at":     timestamp("When this version was published."),
		}, "name", "version", "type", "prompt", "labels", "created_at"),
	}, t.getPrompt)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_scores",
		Annotations: readOnly("List scores"),
		Description: "Read quality judgements attached to traces — the user asks how an eval scored, what a judge decided, or whether a metric moved. " +
			"Returns a page of scores with their values, data types and comments, newest first. " +
			"Does NOT compute aggregates: use get_stats for counts, cost and latency over many traces.",
		InputSchema: object(pagingProperties(map[string]*jsonschema.Schema{
			"trace_id":       text("Only scores about this trace."),
			"observation_id": text("Only scores about this observation."),
			"session_id":     text("Only scores about this session."),
			"name":           text("Only scores filed under this name, e.g. \"helpfulness\"."),
			"data_type":      oneOf("Only scores of this type.", "numeric", "boolean", "categorical", "text"),
			"from":           timestamp("Only scores at or after this RFC 3339 instant."),
			"to":             timestamp("Only scores strictly before this RFC 3339 instant."),
		})),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"scores": list(object(map[string]*jsonschema.Schema{
				"id":             text("The score's id."),
				"trace_id":       text("What it grades."),
				"observation_id": text("What it grades, more precisely."),
				"session_id":     text("What it grades, when it grades a whole session."),
				"name":           text("What is being measured."),
				"data_type":      oneOf("How to read the value.", "numeric", "boolean", "categorical", "text"),
				"value":          number("The number, for numeric and boolean scores."),
				"string_value":   text("The string, for categorical and text scores."),
				"comment":        text("Free text: a judge's rationale, a reviewer's note."),
				"timestamp":      timestamp("When the graded interaction happened."),
				"created_at":     timestamp("When the score was written."),
			}, "id", "name", "data_type", "timestamp", "created_at"), "The page, newest first."),
			"next_cursor": text("Pass back as `cursor` for the next page."),
		}, "scores"),
	}, t.listScores)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_stats",
		Annotations: readOnly("Aggregate traffic, cost and latency"),
		Description: "Aggregate traffic, failures, cost and latency — the user asks how many runs there were, how much they cost, how slow they are, " +
			"or how any of that changed over time or differs per model. " +
			"Returns buckets with count, error_count, total_cost and exact p50/p95 latency. " +
			"Read the `unit` field before comparing counts: grouping by hour, day, environment or release " +
			"counts traces, grouping by model counts observations, because a trace has no model. " +
			"Group by release when the user asks whether a deployment moved cost or latency. " +
			"Pass user_id when the question is about one end user — the same buckets, restricted to them, " +
			"which is how to answer \"what does this account cost me\" or \"when is this user active\". " +
			"Does NOT return individual traces — use list_traces for those.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"group_by": oneOf("What each bucket collects. Default \"day\".",
				"hour", "day", "model", "environment", "release"),
			"from":        timestamp("Only traces at or after this RFC 3339 instant."),
			"to":          timestamp("Only traces strictly before this RFC 3339 instant."),
			"environment": text("Only traces from this environment."),
			"user_id": text("Only traces attributed to this end user. With an \"hour\" or \"day\" grouping, " +
				"each bucket also carries `sessions`: how many of that user's sessions began in it."),
		}),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"group_by": oneOf("What each bucket collects.", "hour", "day", "model", "environment", "release"),
			"unit": oneOf("What `count` counts. Counts of different units are not comparable.",
				"trace", "observation"),
			"buckets": list(object(map[string]*jsonschema.Schema{
				"key": text("The hour, day, model, environment or release this bucket is. " +
					"Empty when grouping by release and the traces named none."),
				"count":       integer("How many of `unit` fell in this bucket."),
				"error_count": integer("How many of those failed."),
				"total_cost":  number("Summed over what reported a cost; absent when nothing did."),
				"sessions": integer("Only with `user_id` and an \"hour\" or \"day\" grouping: how many of that " +
					"user's sessions began in this bucket. A session is counted where it starts, so a sum is exact."),
				"latency_ms": object(map[string]*jsonschema.Schema{
					"p50": integer("Median latency in milliseconds; null when nothing timed."),
					"p95": integer("95th percentile latency in milliseconds; null when nothing timed."),
				}),
			}, "key", "count", "error_count", "latency_ms"), "One bucket per group, ascending by key."),
		}, "group_by", "unit", "buckets"),
	}, t.getStats)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_score_trends",
		Annotations: readOnly("Trend the scores"),
		Description: "Trend quality over time — the user asks whether a score moved, whether a release made the judge happier, " +
			"which model scores best, or how a metric has been doing lately. " +
			"Returns one series per score name, each with buckets: a numeric name carries mean, min and max, " +
			"a boolean name the rate of true, a categorical name the count of each value. " +
			"A score is counted in the hour of the trace it grades, so these buckets line up with get_stats'. " +
			"Read `targets`: grouping by model counts only scores that name an observation, because a trace-level score has no model. " +
			"Pass name to trend one score; without it every name in the range comes back. " +
			"Does NOT return the individual scores — use list_scores for those — and never counts text scores or " +
			"scores that name only a session, which have no trace to place them.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"group_by": oneOf("What each bucket collects. Default \"day\".",
				"hour", "day", "environment", "release", "model"),
			"name":        text("Only the score filed under this name, e.g. \"hallucination\". Absent, every name is a series."),
			"from":        timestamp("Only scores on traces at or after this RFC 3339 instant."),
			"to":          timestamp("Only scores on traces strictly before this RFC 3339 instant."),
			"environment": text("Only scores on traces from this environment."),
			"limit": bounded(minLimit, maxLimit, "How many series to return, 1 to 500. Default 50, "+
				"the busiest names first; `omitted` says how many were left out."),
		}),
		OutputSchema: object(map[string]*jsonschema.Schema{
			"group_by": oneOf("What each bucket collects.",
				"hour", "day", "environment", "release", "model"),
			"targets": oneOf("What was counted: \"observation\" when grouping by model, where a trace-level score cannot appear; "+
				"\"any\" otherwise, where every score counts once.", "any", "observation"),
			"omitted": integer("How many score names the limit left out; 0 when every name is here. " +
				"Raise limit, or narrow the range, to see them."),
			"series": list(object(map[string]*jsonschema.Schema{
				"name":      text("What is being measured."),
				"data_type": oneOf("Which summary each bucket carries.", "numeric", "boolean", "categorical"),
				"buckets": list(object(map[string]*jsonschema.Schema{
					"key":   text("The hour, day, environment, release or model this bucket is."),
					"count": integer("How many scores fell in this bucket."),
					"mean":  number("Numeric names only: the mean of the values."),
					"min":   number("Numeric names only: the smallest value."),
					"max":   number("Numeric names only: the largest value."),
					"rate":  number("Boolean names only: the share whose value is 1, between 0 and 1."),
					"categories": counters("Categorical names only: how many scores carried each value seen. " +
						"Past the twentieth value the rest are summed under \"other\", so the counts still add up to `count`."),
				}, "key", "count"), "One bucket per group, ascending by key."),
			}, "name", "data_type", "buckets"), "One series per score name, by name."),
		}, "group_by", "targets", "omitted", "series"),
	}, t.getScoreTrends)

	// The eval tools are declared in evals.go, in their own file because
	// they are their own surface: six reads over datasets and runs.
	registerEvals(server, t)
}

// readOnly is the annotation every tool here carries: the whole surface reads,
// and a read-only tool should not prompt a host the way a write would (#18).
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true}
}

// withFields adds `?fields=`, which only the trace listing offers.
func withFields(properties map[string]*jsonschema.Schema) map[string]*jsonschema.Schema {
	properties["fields"] = text("Comma-separated subset of the row fields to return, e.g. \"id,name,error_count\". " +
		"Use it to keep a wide listing small. An unknown field name is an error.")
	return properties
}

// withSearch adds `q`, which only the search tool offers: `list_traces` does
// not grow one (spec 011 #9).
func withSearch(properties map[string]*jsonschema.Schema) map[string]*jsonschema.Schema {
	properties["q"] = &jsonschema.Schema{Type: "string", MaxLength: pointer(512), Description: "The text to find. " +
		"Words — all of them must occur in one field of one observation — plus \"quoted phrases\" for adjacent words " +
		"and a trailing * for a prefix. Everything else is literal text: there is no operator syntax to escape, " +
		"and no way to write a query this server refuses except one with no word in it."}
	return properties
}

func pointer[T any](value T) *T { return &value }

// traceRowSchema is a listing row: aggregates, never payloads.
func traceRowSchema() *jsonschema.Schema {
	return object(map[string]*jsonschema.Schema{
		"id":                matching(traceIDPattern, "The trace id, for get_trace."),
		"name":              text("What the application called this trace."),
		"user_id":           text("The end user it was attributed to."),
		"session_id":        text("The session it belongs to, for get_session."),
		"environment":       text("Where it ran."),
		"release":           text("The deployment it ran in, which list_traces takes as its release filter."),
		"version":           text("The version of the application's own logic."),
		"tags":              list(text("A tag."), "Tags the application set."),
		"timestamp":         timestamp("When its earliest observation started."),
		"total_cost":        number("Summed over observations whose client reported a cost; absent when none did."),
		"latency_ms":        integer("End to end, in milliseconds."),
		"ttft_ms":           integer("The wait before the first token of its earliest completion; absent when no observation reported one."),
		"error_count":       integer("How many of its observations failed."),
		"observation_count": integer("How many observations it has."),
	}, "id", "environment", "error_count", "observation_count")
}

// searchRowSchema is a listing row with the one field a search adds: where the
// trace matched. Its own schema rather than a field on `traceRowSchema`,
// because `match` is never present without a `q` and a listing tool that
// advertised it would be describing a row it cannot return.
func searchRowSchema() *jsonschema.Schema {
	row := traceRowSchema()
	row.Properties["match"] = object(map[string]*jsonschema.Schema{
		"observation_id": text("The observation whose text matched, for get_observation_io. " +
			"Null when it was the trace's own name."),
		"field": oneOf("Which of the observation's fields matched.",
			"input", "output", "metadata", "name", "status_message", "trace_name"),
		"snippet": text("Up to 160 characters of the field's text around the hit, cut on word boundaries. " +
			"Plain text: the hit is not marked up, and only the first 64 KiB of a payload is searched at all."),
	}, "field", "snippet")
	row.Properties["match"].Description = "Where this trace matched: the observation and field with the best score " +
		"among its hits, and the text around the first term."
	return row
}

// sessionRowSchema is a session roll-up, the same six fields the listing and
// the single-session tool both report.
func sessionRowSchema() *jsonschema.Schema {
	return object(map[string]*jsonschema.Schema{
		"id":          text("The session id, for get_session."),
		"trace_count": integer("How many traces name this session."),
		"error_count": integer("How many of those traces have a failed observation."),
		"total_cost":  number("Summed over the traces that reported a cost; absent when none did."),
		"first_seen":  timestamp("When the session's earliest trace started."),
		"last_seen":   timestamp("When its latest trace started."),
	}, "id", "trace_count", "error_count")
}

// userRowSchema is one end user's roll-up, the shape both user tools report
// (spec 023 #5).
func userRowSchema() *jsonschema.Schema {
	return object(map[string]*jsonschema.Schema{
		"user_id":     text("The user id, for get_user and as list_traces' user_id filter."),
		"traces":      integer("How many traces are attributed to this user."),
		"error_count": integer("How many of those traces have a failed observation."),
		"total_cost":  number("Summed over the traces that reported a cost; absent when none did."),
		"sessions":    integer("How many sessions of this user have begun."),
		"first_seen":  timestamp("When the earliest hour the rollup still holds for them starts."),
		"last_seen":   timestamp("When they were last active."),
	}, "user_id", "traces", "error_count", "sessions")
}

// userSummarySchema is that row with the latency the single-user read adds.
func userSummarySchema() *jsonschema.Schema {
	schema := userRowSchema()
	schema.Properties["latency_ms"] = object(map[string]*jsonschema.Schema{
		"p50": integer("Median trace latency in milliseconds; null when nothing timed."),
		"p95": integer("95th percentile trace latency in milliseconds; null when nothing timed."),
	})
	return schema
}

// traceDetailSchema is one trace with its tree. `observations` is recursive,
// so the node shape is a $defs entry the array refers to.
func traceDetailSchema() *jsonschema.Schema {
	node := object(map[string]*jsonschema.Schema{
		"id": matching(observationIDPattern, "The observation id, for get_observation_io."),
		"parent_observation_id": text("The observation this one ran under, whenever it named one — " +
			"nested children carry it too, so it is not a sign that the parent is missing. " +
			"An observation whose parent is not in this trace renders at the root and still carries it."),
		"type": oneOf("What kind of work this was.",
			"span", "generation", "event", "agent", "tool", "chain",
			"retriever", "guardrail", "evaluator", "embedding"),
		"name":       text("What the application called it."),
		"start_time": timestamp("When it started."),
		"end_time":   timestamp("When it ended."),
		"completion_start_time": timestamp("When the first token came back, as the client reported it. " +
			"Uncorrected, so it can precede start_time."),
		"ttft_ms":          integer("The wait before that first token, in milliseconds."),
		"model":            text("The model, for a generation or an embedding."),
		"model_parameters": anything("Temperature, max tokens and the rest, as sent."),
		"level":            oneOf("Its severity.", "DEBUG", "DEFAULT", "WARNING", "ERROR"),
		"status_message":   text("Why it failed, when it did."),
		"usage":            anything("Token counts as the client reported them."),
		"cost_details":     anything("Cost as the client reported it; absent when it reported none."),
		"prompt": object(map[string]*jsonschema.Schema{
			"name":    text("The prompt name, which list_traces takes as its prompt filter."),
			"version": integer("The version, or null when the client named none."),
		}, "name", "version"),
		"input_bytes":  integer("How large the input is, whether or not it was inlined."),
		"output_bytes": integer("How large the output is."),
		"input":        anything("With expand=io: what went in, or a truncation marker."),
		"output":       anything("With expand=io: what came out, or a truncation marker."),
		"metadata":     anything("With expand=io: the observation's metadata, or a truncation marker."),
		"children":     list(&jsonschema.Schema{Ref: "#/$defs/observation"}, "Nested observations; absent for a leaf."),
	}, "id", "type", "level")

	schema := object(map[string]*jsonschema.Schema{
		"id":                matching(traceIDPattern, "The trace id."),
		"name":              text("What the application called this trace."),
		"user_id":           text("The end user it was attributed to."),
		"session_id":        text("The session it belongs to."),
		"environment":       text("Where it ran."),
		"release":           text("The deployment it ran in, which list_traces takes as its release filter."),
		"version":           text("The version of the application's own logic."),
		"tags":              list(text("A tag."), "Tags the application set."),
		"timestamp":         timestamp("When its earliest observation started."),
		"total_cost":        number("Summed over observations whose client reported a cost."),
		"latency_ms":        integer("End to end, in milliseconds."),
		"ttft_ms":           integer("The wait before the first token of its earliest completion; absent when no observation reported one."),
		"error_count":       integer("How many of its observations failed."),
		"observation_count": integer("How many observations it has."),
		"metadata":          anything("The trace's own metadata."),
		"observations":      list(&jsonschema.Schema{Ref: "#/$defs/observation"}, "The tree, roots ordered by start time."),
		"expansion": object(map[string]*jsonschema.Schema{
			"expanded": &jsonschema.Schema{Type: "boolean",
				Description: "Always false; the key is absent when the expansion happened."},
			"payloads": integer("How many payloads the trace holds."),
			"budget_needed": integer("The budget that would carry a marker for each of them. " +
				"Not clamped, so it may be more than `budget` accepts — see retryable."),
			"retryable": &jsonschema.Schema{Type: "boolean",
				Description: "Whether budget_needed is a budget this server would accept. " +
					"False means no budget will do: read the payloads one at a time with get_observation_io."},
			"reason": text("Why nothing was expanded."),
		}, "expanded", "payloads", "budget_needed", "retryable", "reason"),
	}, "id", "environment", "error_count", "observation_count", "observations")
	schema.Properties["expansion"].Description = "Present only when expand=io was refused because the budget " +
		"could not carry a marker for every payload. Nothing is unreachable — every observation id is in the " +
		"tree, so get_observation_io still works — and budget_needed is what to retry `budget` with."
	schema.Defs = map[string]*jsonschema.Schema{"observation": node}
	return schema
}

// --- handlers -------------------------------------------------------------

type listTracesInput struct {
	traceFilterInput
	pagingInput
	walkInput
	Fields string `json:"fields"`
}

func (t *toolset) listTraces(ctx context.Context, req *mcp.CallToolRequest, in listTracesInput) (*mcp.CallToolResult, any, error) {
	query := in.walkInput.apply(in.pagingInput.apply(in.traceFilterInput.query()))
	set(query, "fields", in.Fields)
	return t.call(ctx, req, "/api/v1/traces", query, summarizeTraceList)
}

type searchInput struct {
	listTracesInput
	Q string `json:"q"`
}

func (t *toolset) search(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
	query := in.walkInput.apply(in.pagingInput.apply(in.traceFilterInput.query()))
	set(query, "fields", in.Fields)
	set(query, "q", in.Q)
	return t.call(ctx, req, "/api/v1/traces", query, summarizeSearch)
}

type getTraceInput struct {
	expansionInput
	TraceID string `json:"trace_id"`
}

func (t *toolset) getTrace(ctx context.Context, req *mcp.CallToolRequest, in getTraceInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req, "/api/v1/traces/"+url.PathEscape(in.TraceID),
		in.expansionInput.apply(url.Values{}), summarizeTrace)
}

type getLastTraceInput struct {
	traceFilterInput
	expansionInput
}

func (t *toolset) getLastTrace(ctx context.Context, req *mcp.CallToolRequest, in getLastTraceInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req, "/api/v1/traces/last",
		in.expansionInput.apply(in.traceFilterInput.query()), summarizeTrace)
}

type getObservationIOInput struct {
	ObservationID string `json:"observation_id"`
	TraceID       string `json:"trace_id"`
}

func (t *toolset) getObservationIO(ctx context.Context, req *mcp.CallToolRequest, in getObservationIOInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "trace_id", in.TraceID)
	return t.call(ctx, req, "/api/v1/observations/"+url.PathEscape(in.ObservationID)+"/io",
		query, summarizeIO)
}

type listSessionsInput struct {
	pagingInput
	walkInput
	From        string `json:"from"`
	To          string `json:"to"`
	Environment string `json:"environment"`
	UserID      string `json:"user_id"`
}

func (t *toolset) listSessions(ctx context.Context, req *mcp.CallToolRequest, in listSessionsInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "from", in.From)
	set(query, "to", in.To)
	set(query, "environment", in.Environment)
	set(query, "user_id", in.UserID)
	return t.call(ctx, req, "/api/v1/sessions",
		in.walkInput.apply(in.pagingInput.apply(query)), summarizeSessionList)
}

type getSessionInput struct {
	pagingInput
	// Not `walkInput`: this endpoint takes no `count`, and a field that
	// forwards one would turn a tool call into a 400 for a parameter the
	// schema never offered.
	Direction string `json:"direction"`
	SessionID string `json:"session_id"`
}

func (t *toolset) getSession(ctx context.Context, req *mcp.CallToolRequest, in getSessionInput) (*mcp.CallToolResult, any, error) {
	query := in.pagingInput.apply(url.Values{})
	set(query, "direction", in.Direction)
	return t.call(ctx, req, "/api/v1/sessions/"+url.PathEscape(in.SessionID), query, summarizeSession)
}

type listUsersInput struct {
	pagingInput
	walkInput
	Sort   string `json:"sort"`
	Prefix string `json:"prefix"`
}

func (t *toolset) listUsers(ctx context.Context, req *mcp.CallToolRequest, in listUsersInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "sort", in.Sort)
	set(query, "prefix", in.Prefix)
	return t.call(ctx, req, "/api/v1/users",
		in.walkInput.apply(in.pagingInput.apply(query)), summarizeUserList)
}

type getUserInput struct {
	UserID string `json:"user_id"`
}

func (t *toolset) getUser(ctx context.Context, req *mcp.CallToolRequest, in getUserInput) (*mcp.CallToolResult, any, error) {
	return t.call(ctx, req, "/api/v1/users/"+url.PathEscape(in.UserID), url.Values{}, summarizeUser)
}

type getPromptInput struct {
	Name    string `json:"name"`
	Version *int   `json:"version"`
	Label   string `json:"label"`
}

func (t *toolset) getPrompt(ctx context.Context, req *mcp.CallToolRequest, in getPromptInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "label", in.Label)
	if in.Version != nil {
		query.Set("version", strconv.Itoa(*in.Version))
	}
	return t.call(ctx, req, "/api/v1/prompts/"+url.PathEscape(in.Name), query, summarizePrompt)
}

type listScoresInput struct {
	pagingInput
	TraceID       string `json:"trace_id"`
	ObservationID string `json:"observation_id"`
	SessionID     string `json:"session_id"`
	Name          string `json:"name"`
	DataType      string `json:"data_type"`
	From          string `json:"from"`
	To            string `json:"to"`
}

func (t *toolset) listScores(ctx context.Context, req *mcp.CallToolRequest, in listScoresInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "trace_id", in.TraceID)
	set(query, "observation_id", in.ObservationID)
	set(query, "session_id", in.SessionID)
	set(query, "name", in.Name)
	set(query, "data_type", in.DataType)
	set(query, "from", in.From)
	set(query, "to", in.To)
	return t.call(ctx, req, "/api/v1/scores", in.pagingInput.apply(query), summarizeScores)
}

type getStatsInput struct {
	GroupBy     string `json:"group_by"`
	From        string `json:"from"`
	To          string `json:"to"`
	Environment string `json:"environment"`
	UserID      string `json:"user_id"`
}

func (t *toolset) getStats(ctx context.Context, req *mcp.CallToolRequest, in getStatsInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "group_by", in.GroupBy)
	set(query, "from", in.From)
	set(query, "to", in.To)
	set(query, "environment", in.Environment)
	set(query, "user_id", in.UserID)
	return t.call(ctx, req, "/api/v1/stats", query, summarizeStats)
}

type getScoreTrendsInput struct {
	GroupBy     string `json:"group_by"`
	Name        string `json:"name"`
	From        string `json:"from"`
	To          string `json:"to"`
	Environment string `json:"environment"`
	Limit       *int   `json:"limit"`
}

func (t *toolset) getScoreTrends(ctx context.Context, req *mcp.CallToolRequest, in getScoreTrendsInput) (*mcp.CallToolResult, any, error) {
	query := url.Values{}
	set(query, "group_by", in.GroupBy)
	set(query, "name", in.Name)
	set(query, "from", in.From)
	set(query, "to", in.To)
	set(query, "environment", in.Environment)
	if in.Limit != nil {
		query.Set("limit", strconv.Itoa(*in.Limit))
	}
	return t.call(ctx, req, "/api/v1/stats/scores", query, summarizeScoreTrends)
}

// --- summaries ------------------------------------------------------------
//
// One line of text beside the structured result: enough for a client that
// renders `content` to show something meaningful, and for a model to see what
// it got without parsing. The structured half is the answer.

func summarizeTraceList(body json.RawMessage) string {
	var parsed struct {
		Traces []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Timestamp  string `json:"timestamp"`
			ErrorCount int    `json:"error_count"`
		} `json:"traces"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of traces"
	}
	if len(parsed.Traces) == 0 {
		return "No traces match those filters."
	}
	failed := 0
	for _, trace := range parsed.Traces {
		if trace.ErrorCount > 0 {
			failed++
		}
	}
	summary := fmt.Sprintf("%d traces, newest %s (%s); %d with errors",
		len(parsed.Traces), parsed.Traces[0].ID, parsed.Traces[0].Timestamp, failed)
	if parsed.NextCursor != nil {
		summary += "; more pages available"
	}
	return summary + "."
}

// summarizeSearch says where the hits were, not only how many: the whole point
// of the tool is that the next call is about one observation.
func summarizeSearch(body json.RawMessage) string {
	var parsed struct {
		Traces []struct {
			ID    string `json:"id"`
			Match *struct {
				Field   string `json:"field"`
				Snippet string `json:"snippet"`
			} `json:"match"`
		} `json:"traces"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of matching traces"
	}
	if len(parsed.Traces) == 0 {
		return "Nothing matches that text."
	}
	summary := fmt.Sprintf("%d traces match", len(parsed.Traces))
	if match := parsed.Traces[0].Match; match != nil {
		summary += fmt.Sprintf("; the newest is %s, matched in its %s: %q",
			parsed.Traces[0].ID, match.Field, match.Snippet)
	}
	if parsed.NextCursor != nil {
		summary += "; more pages available"
	}
	return summary + "."
}

func summarizeTrace(body json.RawMessage) string {
	var parsed struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Environment      string `json:"environment"`
		ObservationCount int    `json:"observation_count"`
		ErrorCount       int    `json:"error_count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one trace"
	}
	return fmt.Sprintf("Trace %s (%s) in %s: %d observations, %d failed.",
		parsed.ID, orUnnamed(parsed.Name), orUnnamed(parsed.Environment),
		parsed.ObservationCount, parsed.ErrorCount)
}

func summarizeIO(body json.RawMessage) string {
	var parsed struct {
		ObservationID string          `json:"observation_id"`
		Input         json.RawMessage `json:"input"`
		Output        json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one observation's payloads"
	}
	return fmt.Sprintf("Observation %s: %d bytes of input, %d bytes of output, whole.",
		parsed.ObservationID, len(parsed.Input), len(parsed.Output))
}

func summarizeSessionList(body json.RawMessage) string {
	var parsed struct {
		Sessions []struct {
			ID         string `json:"id"`
			TraceCount int    `json:"trace_count"`
			ErrorCount int    `json:"error_count"`
			LastSeen   string `json:"last_seen"`
		} `json:"sessions"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of sessions"
	}
	if len(parsed.Sessions) == 0 {
		return "No sessions match those filters."
	}
	traces, failing := 0, 0
	for _, session := range parsed.Sessions {
		traces += session.TraceCount
		if session.ErrorCount > 0 {
			failing++
		}
	}
	summary := fmt.Sprintf("%d sessions over %d traces, most recent %s (%s); %d with errors",
		len(parsed.Sessions), traces, parsed.Sessions[0].ID, parsed.Sessions[0].LastSeen, failing)
	if parsed.NextCursor != nil {
		summary += "; more pages available"
	}
	return summary + "."
}

func summarizeSession(body json.RawMessage) string {
	var parsed struct {
		ID         string `json:"id"`
		TraceCount int    `json:"trace_count"`
		ErrorCount int    `json:"error_count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one session"
	}
	return fmt.Sprintf("Session %s: %d traces, %d with errors.",
		parsed.ID, parsed.TraceCount, parsed.ErrorCount)
}

func summarizeUserList(body json.RawMessage) string {
	var parsed struct {
		Users []struct {
			UserID string `json:"user_id"`
			Traces int    `json:"traces"`
		} `json:"users"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of users"
	}
	if len(parsed.Users) == 0 {
		return "No users match that."
	}
	traces := 0
	for _, user := range parsed.Users {
		traces += user.Traces
	}
	summary := fmt.Sprintf("%d users over %d traces, first %s (%d traces)",
		len(parsed.Users), traces, parsed.Users[0].UserID, parsed.Users[0].Traces)
	if parsed.NextCursor != nil {
		summary += "; more pages available"
	}
	return summary + "."
}

func summarizeUser(body json.RawMessage) string {
	var parsed struct {
		UserID     string `json:"user_id"`
		Traces     int    `json:"traces"`
		Sessions   int    `json:"sessions"`
		ErrorCount int    `json:"error_count"`
		LastSeen   string `json:"last_seen"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one user"
	}
	return fmt.Sprintf("User %s: %d traces over %d sessions, %d with errors, last seen %s.",
		parsed.UserID, parsed.Traces, parsed.Sessions, parsed.ErrorCount, orUnnamed(parsed.LastSeen))
}

func summarizePrompt(body json.RawMessage) string {
	var parsed struct {
		Name    string   `json:"name"`
		Version int      `json:"version"`
		Type    string   `json:"type"`
		Labels  []string `json:"labels"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "one prompt version"
	}
	summary := fmt.Sprintf("Prompt %s version %d (%s)", parsed.Name, parsed.Version, parsed.Type)
	if len(parsed.Labels) > 0 {
		summary += ", labelled " + strings.Join(parsed.Labels, ", ")
	}
	return summary + "."
}

func summarizeScores(body json.RawMessage) string {
	var parsed struct {
		Scores []struct {
			Name string `json:"name"`
		} `json:"scores"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "a page of scores"
	}
	if len(parsed.Scores) == 0 {
		return "No scores match those filters."
	}
	return fmt.Sprintf("%d scores, newest %q.", len(parsed.Scores), parsed.Scores[0].Name)
}

func summarizeStats(body json.RawMessage) string {
	var parsed struct {
		GroupBy string `json:"group_by"`
		Unit    string `json:"unit"`
		Buckets []struct {
			Count int `json:"count"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "statistics"
	}
	total := 0
	for _, bucket := range parsed.Buckets {
		total += bucket.Count
	}
	return fmt.Sprintf("%d buckets by %s, %d %ss in total.",
		len(parsed.Buckets), parsed.GroupBy, total, parsed.Unit)
}

func summarizeScoreTrends(body json.RawMessage) string {
	var parsed struct {
		GroupBy string `json:"group_by"`
		Targets string `json:"targets"`
		Omitted int    `json:"omitted"`
		Series  []struct {
			Name    string `json:"name"`
			Buckets []struct {
				Count int `json:"count"`
			} `json:"buckets"`
		} `json:"series"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "score trends"
	}
	if len(parsed.Series) == 0 {
		return "No score names a trace in that range."
	}
	total := 0
	names := make([]string, 0, len(parsed.Series))
	for _, series := range parsed.Series {
		names = append(names, series.Name)
		for _, bucket := range series.Buckets {
			total += bucket.Count
		}
	}
	summary := fmt.Sprintf("%d series by %s (%s), %d scores in total: %s",
		len(parsed.Series), parsed.GroupBy, parsed.Targets, total, strings.Join(names, ", "))
	// A model reading this has to know the list is not the whole list.
	if parsed.Omitted > 0 {
		summary += fmt.Sprintf("; %d rarer names not shown", parsed.Omitted)
	}
	return summary + "."
}

func orUnnamed(value string) string {
	if value == "" {
		return "unnamed"
	}
	return value
}
