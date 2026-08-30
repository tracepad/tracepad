package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The read API's centre (spec 004): the trace listing, one trace as a tree,
// the "last trace" shortcut and the one budget-exempt payload endpoint.

// spanID is the shape of an observation id — the 16 lower-case hex characters
// of an OTLP span id (schema 0002).
var spanID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// traceID is the shape of a trace id: 32 lower-case hex characters.
var traceID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// traceRowFields is the full shape of a list row, in the order a row renders
// it. `?fields=` selects from this list, so it is also what an unknown field
// is reported against.
// `match` is last because it is the only row field that is not a column of the
// trace: it says where the search hit, and it is present only with `q`
// (spec 011 #6).
var traceRowFields = []string{
	"id", "name", "user_id", "session_id", "environment", "release", "version", "tags",
	"timestamp", "total_cost", "latency_ms", "ttft_ms", "error_count", "observation_count",
	"match",
}

// traceListFilters is every query parameter the listing accepts. `traces/last`
// accepts the same set (#7) plus the two that shape a single trace.
var traceListFilters = []string{
	"from", "to", "environment", "user_id", "session_id", "name", "tag",
	"status", "min_cost", "q", "release", "version", "type", "prompt",
}

// handleListTraces serves the filtered, cursor-paginated listing, newest
// first. Rows carry the aggregate columns and never a payload: a listing is
// for choosing what to fetch, and payloads are what make that choice
// expensive (#2).
func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, traceListFilters...),
		"fields", "limit", "cursor", "direction", "count")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	counting, err := wantsCount(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fields, err := parseSelection(values.Get("fields"), traceRowFields)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter, err := traceFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// One row beyond the page tells us whether there is another one in the
	// direction we are scanning.
	filter.Limit = limit + 1
	filter.Backward = backward
	raw := values.Get("cursor")
	if raw != "" {
		cursor, err := decodeTraceCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}

	traces, err := s.store.Traces(project.ID, filter)
	if err != nil {
		slog.Error("list traces failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list traces")
		return
	}

	traces, prev, next := trimPage(traces, limit, backward, raw,
		func(row *store.TraceRow) string {
			return encodeCursor(strconv.FormatInt(row.Timestamp, 10), row.ID)
		})
	// Where each row matched: one statement for the page and one payload
	// read per matching row, paid only when there is a `q` and only when the
	// caller kept `match` in its `?fields=` (spec 011 #6).
	var matches map[string]*store.TraceMatch
	if filter.Search != nil && fields.wants("match") {
		ids := make([]string, 0, len(traces))
		for _, row := range traces {
			ids = append(ids, row.ID)
		}
		matches, err = s.store.SearchMatches(project.ID, ids, filter.Search)
		if err != nil {
			slog.Error("read the search matches failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to list traces")
			return
		}
	}

	rows := make([]object, 0, len(traces))
	for _, row := range traces {
		rendered := renderTraceRow(row)
		if match := matches[row.ID]; match != nil {
			rendered = rendered.put("match", renderMatch(match))
		}
		rows = append(rows, fields.apply(rendered))
	}
	answer := object{}.
		put("traces", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
	if counting {
		// Counted over the filters and not over the page: the cursor is
		// where the reader is, not what there is.
		total, err := s.store.CountTraces(project.ID, filter, countCap+1)
		if err != nil {
			slog.Error("count traces failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to count traces")
			return
		}
		value, stopped := capped(total)
		answer = answer.put("total", value).put("total_capped", stopped)
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleGetTrace serves one trace with its observations as a nested tree: the
// tree is the mental model of a trace, and a flat list pushes assembly onto
// every consumer (#5).
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "expand", "budget")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expand, budget, err := s.expansion(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if !traceID.MatchString(id) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("trace id must be 32 lower-case hex characters, got %q", id))
		return
	}

	trace, err := s.store.Trace(project.ID, id)
	if err != nil {
		slog.Error("read trace failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the trace")
		return
	}
	if trace == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("trace %q not found", id))
		return
	}
	body, ok := s.renderTrace(w, project.ID, trace, expand, budget)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// handleLastTrace answers "the last failed trace, whole" in one round trip
// (#7): every list filter, the newest match, rendered exactly as
// `GET /traces/{id}` renders it.
func (s *Server) handleLastTrace(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, traceListFilters...), "expand", "budget")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expand, budget, err := s.expansion(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter, err := traceFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.Limit = 1

	traces, err := s.store.Traces(project.ID, filter)
	if err != nil {
		slog.Error("read last trace failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the last trace")
		return
	}
	if len(traces) == 0 {
		// The message names the filters, because "not found" about a
		// query the caller composed is only useful if it says which
		// query (edge cases).
		writeError(w, http.StatusNotFound, "no trace matches "+describeFilters(values))
		return
	}
	// The listing row carries no metadata (it is a payload); the shortcut
	// promises the same shape as GET /traces/{id}, so the trace is read
	// again in full.
	trace, err := s.store.Trace(project.ID, traces[0].ID)
	if err != nil {
		slog.Error("read last trace failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the last trace")
		return
	}
	if trace == nil {
		// Retention or a concurrent delete between the two reads.
		writeError(w, http.StatusNotFound, "no trace matches "+describeFilters(values))
		return
	}
	body, ok := s.renderTrace(w, project.ID, trace, expand, budget)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// renderTrace assembles the tree and, when asked, spends the payload budget on
// it. It answers the client itself on a read failure.
func (s *Server) renderTrace(w http.ResponseWriter, projectID string, trace *store.TraceRow,
	expand bool, budgetBytes int) (object, bool) {
	io := store.SkipIO
	if expand {
		io = store.WithIO
	}
	observations, err := s.store.Observations(projectID, trace.ID, io)
	if err != nil {
		slog.Error("read observations failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the observations")
		return nil, false
	}
	roots := buildTree(observations)

	body := renderTraceDetail(trace, renderNodes(roots, payloadBudget{}, false))
	if !expand {
		return body, true
	}
	// The skeleton is measured before any payload is inlined, so what the
	// payloads get is what is genuinely left of the budget (#6).
	skeleton, err := json.Marshal(body)
	if err != nil {
		slog.Error("render trace failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to render the trace")
		return nil, false
	}
	slots := countPayloads(observations)
	budget := newPayloadBudget(budgetBytes, len(skeleton), slots)
	if !budget.affordable {
		// A trace wide enough that even bare markers would not fit gets
		// none of them, and one line saying so instead: the tree
		// already carries every observation id, so nothing here is a
		// dead end, and `budget_needed` is the number to retry with
		// (Decision 31).
		needed, retryable := budgetNeeded(len(skeleton), slots)
		reason := fmt.Sprintf(
			"a budget of %d bytes cannot carry markers for %d payloads; retry with ?budget=%d, "+
				"or read one payload at a time from /api/v1/observations/{id}/io",
			budgetBytes, slots, needed)
		if !retryable {
			// Naming a budget the server would refuse would be a
			// loop rather than a next step.
			reason = fmt.Sprintf(
				"no budget can carry markers for %d payloads (%d bytes needed, %d is the maximum); "+
					"read payloads one at a time from /api/v1/observations/{id}/io",
				slots, needed, config.MaxResponseBudgetBytes)
		}
		return body.put("expansion", object{}.
			put("expanded", false).
			put("payloads", slots).
			put("budget_needed", needed).
			put("retryable", retryable).
			put("reason", reason)), true
	}
	return renderTraceDetail(trace, renderNodes(roots, budget, true)), true
}

func renderTraceDetail(trace *store.TraceRow, observations []object) object {
	return renderTraceRow(trace).
		putSome("metadata", trace.Metadata).
		put("observations", observations)
}

// renderTraceRow renders the list-row fields of a trace, in the order
// traceRowFields declares them.
func renderTraceRow(row *store.TraceRow) object {
	return object{}.
		put("id", row.ID).
		putSome("name", row.Name).
		putSome("user_id", row.UserID).
		putSome("session_id", row.SessionID).
		put("environment", row.Environment).
		putSome("release", row.Release).
		putSome("version", row.Version).
		putSome("tags", row.Tags).
		putSome("timestamp", formatInstant(row.Timestamp)).
		putSome("total_cost", row.TotalCost).
		putSome("latency_ms", row.LatencyMs).
		putSome("ttft_ms", row.TTFTMs).
		put("error_count", row.ErrorCount).
		put("observation_count", row.ObservationCount)
}

// renderMatch renders where a search hit. `observation_id` is null rather than
// absent when the trace's own name matched: a client reads the field to decide
// which observation to open, and "the trace itself" is an answer to that.
//
// The snippet is plain text, hits unmarked: the API answers with data, not
// markup, and a client that highlights folds the query terms the way the
// tokenizer does (spec 011 #6).
func renderMatch(match *store.TraceMatch) object {
	observation := any(nil)
	if match.ObservationID != "" {
		observation = match.ObservationID
	}
	return object{}.
		put("observation_id", observation).
		put("field", match.Field).
		put("snippet", match.Snippet)
}

// observationNode is one span with the spans that named it as their parent.
type observationNode struct {
	row      *store.ObservationRow
	children []*observationNode
}

// buildTree nests observations under their parents, siblings by start time.
// Rows arrive already ordered by (start_time, id), so appending preserves that
// order at every level.
//
// A span whose parent is not in this trace renders at the root with its
// `parent_observation_id` intact: the parent may still be in flight, and
// hiding the child until it lands would make a live trace look empty (edge
// cases).
//
// A parent cycle is broken rather than merely re-rooted. `parent_span_id` is
// client bytes stored verbatim (spec 002), so two spans naming each other is
// something a caller can produce, and a cyclic `children` graph is not a
// rendering glitch — the renderer recurses into it until the goroutine stack
// is exhausted, which in Go is a fatal error the server cannot recover from
// (found in review of PR #5). The cycle's entry node is detached from its
// parent and rendered at the root, keeping every span visible and the graph
// finite.
func buildTree(rows []*store.ObservationRow) []*observationNode {
	index := make(map[string]*observationNode, len(rows))
	nodes := make([]*observationNode, 0, len(rows))
	for _, row := range rows {
		node := &observationNode{row: row}
		nodes = append(nodes, node)
		index[row.ID] = node
	}

	var roots []*observationNode
	for _, node := range nodes {
		parent, nested := index[node.row.ParentObservationID]
		if !nested || parent == node {
			roots = append(roots, node)
			continue
		}
		parent.children = append(parent.children, node)
	}

	// Anything unreachable from a root is in a cycle. Cutting the edge that
	// leads *into* such a node — rather than only adding it to the roots —
	// is what makes the result a tree: the node keeps its own children, so
	// nothing is lost, and its parent no longer points back at it.
	reachable := make(map[*observationNode]bool, len(nodes))
	var walk func(*observationNode)
	walk = func(node *observationNode) {
		if reachable[node] {
			return
		}
		reachable[node] = true
		for _, child := range node.children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	for _, node := range nodes {
		if reachable[node] {
			continue
		}
		if parent, nested := index[node.row.ParentObservationID]; nested {
			parent.children = slices.DeleteFunc(parent.children,
				func(child *observationNode) bool { return child == node })
		}
		roots = append(roots, node)
		walk(node)
	}
	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].row.StartTime != roots[j].row.StartTime {
			return roots[i].row.StartTime < roots[j].row.StartTime
		}
		return roots[i].row.ID < roots[j].row.ID
	})
	return roots
}

// countPayloads counts the payload slots an expanded tree wants to inline —
// the divisor of the equal share every observation gets (#6).
//
// The count has to agree with what renderNode actually emits, which is why it
// goes through the same asAny: a nil `map[string]any` placed in an `any` is
// not a nil `any`, so counting metadata by `!= nil` counted a slot for every
// observation whether it had metadata or not, shrinking everyone's share and
// truncating payloads that would have fit (found in review of PR #5).
func countPayloads(rows []*store.ObservationRow) int {
	slots := 0
	for _, row := range rows {
		for _, payload := range []any{row.Input, row.Output, asAny(row.Metadata)} {
			if payload != nil {
				slots++
			}
		}
	}
	return slots
}

func renderNodes(nodes []*observationNode, budget payloadBudget, expand bool) []object {
	out := make([]object, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, renderNode(node, budget, expand))
	}
	return out
}

// renderNode renders one observation and its subtree. Payloads ride only with
// `?expand=io`, metadata included: it is a payload row like the other two, it
// is what `/observations/{id}/io` returns alongside them (#3), and inlining it
// unasked would break the promise that a trace of hundreds of observations
// still fits the budget (Decision 24).
func renderNode(node *observationNode, budget payloadBudget, expand bool) object {
	row := node.row
	out := object{}.
		put("id", row.ID).
		putSome("parent_observation_id", row.ParentObservationID).
		put("type", row.Type).
		putSome("name", row.Name).
		putSome("start_time", formatInstant(row.StartTime)).
		putSome("end_time", formatInstant(row.EndTime)).
		putSome("completion_start_time", formatInstant(row.CompletionStartTime)).
		putSome("ttft_ms", observationTTFT(row)).
		putSome("model", row.Model).
		putSome("model_parameters", row.ModelParameters).
		put("level", row.Level).
		putSome("status_message", row.StatusMessage).
		putSome("usage", row.Usage).
		putSome("cost_details", row.CostDetails).
		putSome("prompt", renderPromptLink(row)).
		putSome("input_bytes", row.InputBytes).
		putSome("output_bytes", row.OutputBytes)
	if expand {
		for _, payload := range []struct {
			key   string
			value any
		}{
			{"input", row.Input},
			{"output", row.Output},
			{"metadata", asAny(row.Metadata)},
		} {
			if payload.value == nil {
				continue
			}
			out = out.put(payload.key, budget.render(payload.value, row.TraceID, row.ID))
		}
	}
	if len(node.children) > 0 {
		out = out.put("children", renderNodes(node.children, budget, expand))
	}
	return out
}

// observationTTFT is the wait this observation's caller had: the completion
// start minus the span's own start, in milliseconds (spec 012 #3). The
// difference is derived here rather than stored, because the raw fact is the
// instant and the subtraction is one line.
//
// A span that never said when it started has no wait to report, and neither
// does one that carried no completion start.
func observationTTFT(row *store.ObservationRow) *int64 {
	if row.CompletionStartTime == 0 || row.StartTime <= 0 {
		return nil
	}
	ttft := (row.CompletionStartTime - row.StartTime) / int64(time.Millisecond)
	return &ttft
}

// renderPromptLink renders the prompt an observation ran, or nothing when it
// carried no name. The version is null rather than absent when the client
// labelled a name without a usable version: a client reads the field to
// decide whether it can link to one version, and "the prompt, unversioned" is
// an answer to that (spec 012 #5).
func renderPromptLink(row *store.ObservationRow) any {
	if row.PromptName == "" {
		return nil
	}
	version := any(nil)
	if row.PromptVersion != nil {
		version = *row.PromptVersion
	}
	return object{}.put("name", row.PromptName).put("version", version)
}

// asAny keeps a nil map nil through an interface conversion, so that "no
// metadata" stays absent instead of becoming an empty object.
func asAny(metadata map[string]any) any {
	if metadata == nil {
		return nil
	}
	return metadata
}

// handleObservationIO serves the whole payloads of one span. It is the one
// endpoint no budget applies to (#3): it exists to be the `full` target of
// every truncation marker, and a budget here would recurse. Payload size is
// already bounded at ingest by TRACEPAD_MAX_BODY_BYTES.
func (s *Server) handleObservationIO(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "trace_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if !spanID.MatchString(id) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("observation id must be 16 lower-case hex characters, got %q", id))
		return
	}
	trace := values.Get("trace_id")
	if trace != "" && !traceID.MatchString(trace) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("trace_id must be 32 lower-case hex characters, got %q", trace))
		return
	}

	if trace == "" {
		// A span id is unique only inside its trace, so without one
		// the answer may be ambiguous. Truncation markers always embed
		// `trace_id`, so an agent following a marker never lands here
		// (API contract).
		candidates, err := s.store.ObservationTraces(project.ID, id)
		if err != nil {
			slog.Error("resolve observation failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the observation")
			return
		}
		switch len(candidates) {
		case 0:
			writeError(w, http.StatusNotFound, fmt.Sprintf("observation %q not found", id))
			return
		case 1:
			trace = candidates[0]
		default:
			writeError(w, http.StatusConflict, fmt.Sprintf(
				"observation %q appears in %d traces of this project; add ?trace_id= to say which (candidates: %s)",
				id, len(candidates), strings.Join(candidates, ", ")))
			return
		}
	}

	observation, err := s.store.Observation(project.ID, trace, id)
	if err != nil {
		slog.Error("read observation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the observation")
		return
	}
	if observation == nil {
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("observation %q not found in trace %q", id, trace))
		return
	}
	writeJSON(w, http.StatusOK, object{}.
		put("trace_id", observation.TraceID).
		put("observation_id", observation.ID).
		putSome("input", observation.Input).
		putSome("output", observation.Output).
		putSome("metadata", observation.Metadata))
}

// expansion reads `?expand=io` and `?budget=`. A budget without an expansion
// is accepted: "answer in at most N bytes" is true of a response that was
// already smaller, it simply does not bind.
func (s *Server) expansion(values url.Values) (bool, int, error) {
	expand := false
	if raw := values.Get("expand"); raw != "" {
		if raw != "io" {
			return false, 0, fmt.Errorf("expand must be io, got %q", raw)
		}
		expand = true
	}
	budget := int(s.responseBudget)
	if raw := values.Get("budget"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < config.MinResponseBudgetBytes || parsed > config.MaxResponseBudgetBytes {
			return false, 0, fmt.Errorf("budget must be a whole number of bytes between %d and %d",
				config.MinResponseBudgetBytes, config.MaxResponseBudgetBytes)
		}
		budget = parsed
	}
	return expand, budget, nil
}

// traceFilter reads the filters the listing and the shortcut share.
func traceFilter(values url.Values) (store.TraceFilter, error) {
	filter := store.TraceFilter{
		Environment: values.Get("environment"),
		UserID:      values.Get("user_id"),
		SessionID:   values.Get("session_id"),
		Name:        values.Get("name"),
		Release:     values.Get("release"),
		Version:     values.Get("version"),
		Tags:        values["tag"],
		Status:      values.Get("status"),
	}
	if raw := values.Get("type"); raw != "" {
		// A spelling outside the vocabulary is a 400 rather than an
		// empty listing: "no trace contains a tol call" would be a
		// well-formed answer to a typo (spec 012, API contract).
		if !model.IsObservationType(raw) {
			return filter, fmt.Errorf("type must be one of %s, got %q",
				strings.Join(model.ObservationTypes, ", "), raw)
		}
		filter.Type = raw
	}
	if raw := values.Get("prompt"); raw != "" {
		prompt, err := parsePrompt(raw)
		if err != nil {
			return filter, err
		}
		filter.Prompt = prompt
	}
	switch filter.Status {
	case "", store.TraceStatusError, store.TraceStatusOK:
	default:
		return filter, fmt.Errorf("status must be %s or %s, got %q",
			store.TraceStatusError, store.TraceStatusOK, filter.Status)
	}
	// Half-open, `from` inclusive and `to` exclusive, so walking a
	// timeline a day at a time never reports a trace twice.
	for _, bound := range []struct {
		name   string
		target **int64
	}{
		{"from", &filter.From},
		{"to", &filter.To},
	} {
		raw := values.Get(bound.name)
		if raw == "" {
			continue
		}
		instant, err := parseTime(bound.name, raw)
		if err != nil {
			return filter, err
		}
		*bound.target = &instant
	}
	if raw := values.Get("q"); raw != "" {
		// The user's text never reaches FTS5 as written (spec 011 #4);
		// what a query with no word in it gets is a 400, because
		// "nothing matched" would be a lie about a search that was
		// never asked.
		query, err := store.ParseSearch(raw)
		if err != nil {
			return filter, fmt.Errorf("q: %w", err)
		}
		filter.Search = query
	}
	if raw := values.Get("min_cost"); raw != "" {
		cost, err := strconv.ParseFloat(raw, 64)
		// NaN needs naming separately: every comparison against it is
		// false, so `min_cost=NaN` would pass a `cost < 0` guard and
		// then answer a typo with a well-formed empty listing (spec 003
		// #21, found in review of PR #5).
		if err != nil || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
			return filter, fmt.Errorf("min_cost must be a non-negative number, got %q", raw)
		}
		filter.MinCost = &cost
	}
	return filter, nil
}

// parsePrompt reads `name` or `name@version`. The version has to be a whole
// number, because that is what the column holds: `name@latest` is a label the
// client may well use, but it is not something this filter can answer, and a
// 400 says so rather than returning nothing (spec 012, API contract).
//
// The name is split on the last `@` so that a prompt whose own name contains
// one keeps it.
func parsePrompt(raw string) (*store.PromptFilter, error) {
	name, version := raw, ""
	at := strings.LastIndex(raw, "@")
	versioned := at >= 0
	if versioned {
		name, version = raw[:at], raw[at+1:]
	}
	if name == "" {
		return nil, fmt.Errorf("prompt must name a prompt, got %q", raw)
	}
	if !versioned {
		return &store.PromptFilter{Name: name}, nil
	}
	number, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("prompt version must be a whole number, got %q", version)
	}
	return &store.PromptFilter{Name: name, Version: &number}, nil
}

func decodeTraceCursor(raw string) (*store.TraceCursor, error) {
	parts, err := decodeCursor(raw, 2)
	if err != nil {
		return nil, err
	}
	timestamp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &store.TraceCursor{Timestamp: timestamp, ID: parts[1]}, nil
}

// describeFilters renders the query the caller composed, so a 404 says what
// found nothing rather than only that nothing was found.
func describeFilters(values url.Values) string {
	var parts []string
	for _, name := range traceListFilters {
		for _, value := range values[name] {
			parts = append(parts, fmt.Sprintf("%s=%s", name, value))
		}
	}
	if len(parts) == 0 {
		return "an empty project"
	}
	return strings.Join(parts, " ")
}

// formatInstant renders Unix nanoseconds, or nothing at all for the zero
// instant: a span that never said when it started did not start in 1970, and
// rendering it that way is a stored lie the consumer would act on (spec 003
// #23).
func formatInstant(nanoseconds int64) any {
	if nanoseconds <= 0 {
		return nil
	}
	return formatTime(nanoseconds)
}
