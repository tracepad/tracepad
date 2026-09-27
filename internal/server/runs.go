package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/tracepad/tracepad/internal/store"
)

// Reading an eval back over HTTP (spec 014, part 2): a run with its summary,
// the items it ran with the attempts they got, and two runs compared.
//
// The comparison is a server endpoint on purpose (#18): the CLI, the MCP tools
// and — later — the UI all ask "did this change make it better", and a compare
// implemented twice is the first place two clients disagree (spec 004 #1).

// renderSummary renders a run's summary. Numbers a client could not compute
// from the rows it can see — the percentiles, the means, the coverage — are
// computed here, once.
func renderSummary(summary *store.RunSummary) object {
	scores := object{}
	for _, stat := range summary.Scores {
		entry := object{}.
			put("data_type", stat.DataType).
			put("direction", nullable(stat.Direction)).
			put("count", stat.Count)
		switch {
		case stat.Distribution != nil:
			entry = entry.put("distribution", renderDistribution(stat.Distribution))
		case stat.Mean != nil:
			entry = entry.put("mean", stat.Mean).put("min", stat.Min).put("max", stat.Max)
		}
		scores = scores.put(stat.Name, entry)
	}

	prompts := make([]object, 0, len(summary.Prompts))
	for _, prompt := range summary.Prompts {
		prompts = append(prompts, object{}.put("name", prompt.Name).put("version", prompt.Version))
	}

	return object{}.
		put("items", object{}.
			put("total", summary.Items.Total).
			put("covered", summary.Items.Covered).
			put("missing", summary.Items.Missing).
			put("unknown", summary.Items.Unknown)).
		put("traces", object{}.
			put("count", summary.Traces.Count).
			put("attempts_max", summary.Traces.AttemptsMax).
			put("error_count", summary.Traces.ErrorCount).
			put("total_cost", summary.Traces.TotalCost).
			put("latency_ms", object{}.
				put("p50", summary.Traces.LatencyP50).
				put("p95", summary.Traces.LatencyP95))).
		put("scores", scores).
		put("models", summary.Models).
		put("prompts", prompts)
}

// renderDistribution renders a categorical name's counts in value order, so
// two reads of the same run render the same bytes (spec 004 #16, which the MCP
// parity test holds this to).
func renderDistribution(distribution map[string]int64) object {
	values := make([]string, 0, len(distribution))
	for value := range distribution {
		values = append(values, value)
	}
	sort.Strings(values)
	out := object{}
	for _, value := range values {
		out = out.put(value, distribution[value])
	}
	return out
}

// handleRunItems serves the items of a run's dataset version with the attempts
// the run made at each (API contract → Runs).
func (s *Server) handleRunItems(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction", "unknown", "budget")
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
	unknown, err := boolParam(values, "unknown")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, budgetBytes, err := s.expansion(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, ok := hexPathID(w, r, "run id")
	if !ok {
		return
	}
	run, ok := s.loadRun(w, r, project.ID, id)
	if !ok {
		return
	}

	filter := store.RunItemFilter{Limit: limit + 1, Backward: backward, IncludeUnknown: unknown}
	raw := values.Get("cursor")
	if raw != "" {
		parts, err := decodeCursor(raw, 2)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		bucket, err := strconv.Atoi(parts[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		filter.After = &store.RunItemCursor{Bucket: bucket, Key: parts[1]}
	}

	items, err := s.store.RunItems(r.Context(), project.ID, run, filter)
	if err != nil {
		readFailed(w, r, "failed to read the run's items", err)
		return
	}
	items, prev, next := trimPage(items, limit, backward, raw, func(item *store.RunItem) string {
		key := store.RunItemKey(item)
		return encodeCursor(strconv.Itoa(key.Bucket), key.Key)
	})

	// The skeleton is measured with the payloads left out, so what they get
	// is what is genuinely left of the budget (spec 004 #6). Every item
	// spends one slot on its expected output and one per attempt on the
	// answer it produced.
	slots := 0
	for _, item := range items {
		if item.Item != nil && len(item.Item.ExpectedOutput) > 0 {
			slots++
		}
		for _, attempt := range item.Attempts {
			if attempt.Output != nil {
				slots++
			}
		}
	}
	skeleton, err := json.Marshal(renderRunItems(run, items, payloadBudget{}, false, prev, next))
	if err != nil {
		slog.Error("render run items failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to render the run's items")
		return
	}
	budget := newPayloadBudget(budgetBytes, len(skeleton), slots)
	if !budget.affordable {
		needed, retryable := budgetNeeded(len(skeleton), slots)
		reason := "a smaller `limit` fits more of each item"
		if retryable {
			reason = "retry with ?budget=" + strconv.Itoa(needed) + ", or a smaller `limit`"
		}
		writeError(w, http.StatusBadRequest,
			"this page's payloads do not fit the response budget: "+reason)
		return
	}
	writeJSON(w, http.StatusOK, renderRunItems(run, items, budget, true, prev, next))
}

// renderRunItems renders one page of the item view. It is called twice — once
// with `inline` false to measure the skeleton, once for real — so it must not
// depend on anything but its arguments.
func renderRunItems(run *store.DatasetRun, items []*store.RunItem,
	budget payloadBudget, inline bool, prev, next *string) object {
	rows := make([]object, 0, len(items))
	for _, item := range items {
		var row object
		switch {
		case item.Item != nil:
			row = object{}.
				put("id", item.Item.ID).
				put("seq", item.Item.Seq).
				put("input", rawJSON(item.Item.Input))
			row = putPayload(row, "expected_output",
				rawJSON(item.Item.ExpectedOutput), budget, inline, "", "")
		// An unknown item has no case to show: the traces named an id
		// the dataset does not have at this version, and inventing an
		// empty body for it would read as "the case is blank" rather
		// than "there is no case" (#3). The row is keyed by the id the
		// traces named, and by nothing when they named none
		// (Decision 29).
		case item.ItemID != "":
			row = object{}.put("id", item.ItemID).put("seq", nil).put("unknown", true)
		default:
			row = object{}.put("id", nil).put("seq", nil).put("unknown", true)
		}

		attempts := make([]object, 0, len(item.Attempts))
		for _, attempt := range item.Attempts {
			entry := object{}.
				put("trace_id", attempt.TraceID).
				putSome("timestamp", formatInstant(attempt.Timestamp)).
				put("error_count", attempt.ErrorCount).
				put("total_cost", attempt.TotalCost).
				put("latency_ms", attempt.LatencyMs)
			entry = putPayload(entry, "output", attempt.Output, budget, inline,
				attempt.TraceID, attempt.ObservationID)
			attempts = append(attempts, entry.put("scores", renderAttemptScores(attempt.Scores)))
		}
		rows = append(rows, row.put("attempts", attempts))
	}
	return object{}.
		put("run", run.ID).
		put("dataset", run.Dataset).
		put("dataset_version", run.DatasetVersion).
		put("items", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
}

// putPayload adds one budgeted payload: the value when it fits, a marker when
// it does not. The pair of ids in the marker is what `/observations/{id}/io`
// takes, so a cut answer still names where the whole of it lives (spec 004
// #2); an item's expected output has no observation behind it and is cut
// without one.
//
// While the skeleton is being measured the key is left out entirely, the way
// `?expand=io` leaves it out of a trace's skeleton: a marker counted into the
// skeleton is charged twice — once as structure, once out of the share it
// shrank — and at fifty items that phantom weight is enough to refuse a page
// that fits, with a `?budget=` to retry with that is wrong by the same amount
// (found in review of PR #31). A payload that is simply absent still costs its
// `null`, which is structure and stays.
func putPayload(o object, key string, value any, budget payloadBudget, inline bool,
	traceID, observationID string) object {
	if value == nil {
		return o.put(key, nil)
	}
	if !inline {
		return o
	}
	return o.put(key, budget.render(value, traceID, observationID))
}

// renderAttemptScores renders an attempt's scores whole: a score is a name and
// a number, and cutting one would save nothing worth the ambiguity.
func renderAttemptScores(scores []*store.Score) []object {
	out := make([]object, 0, len(scores))
	for _, score := range scores {
		entry := object{}.
			put("id", score.ID).
			put("name", score.Name).
			put("data_type", score.DataType)
		if score.Value != nil {
			entry = entry.put("value", *score.Value)
		}
		if score.StringValue != nil {
			entry = entry.put("string_value", *score.StringValue)
		}
		out = append(out, entry.putSome("comment", score.Comment))
	}
	return out
}

// rawJSON turns stored JSON bytes into a value the renderer can embed, and nil
// into nil: the three item bodies are opaque to the store (#4) and stay opaque
// here.
func rawJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}

// Verdicts a comparison can reach about one item's score (#16). `improved`
// and `regressed` are only sayable when a config gave the name a direction;
// without one the honest words are `changed` and `same`.
const (
	verdictImproved  = "improved"
	verdictRegressed = "regressed"
	verdictChanged   = "changed"
	verdictSame      = "same"
)

// handleCompareRuns compares two runs of one dataset (#18).
func (s *Server) handleCompareRuns(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction")
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
	first, ok := hexPathValue(w, r, "a", "run id")
	if !ok {
		return
	}
	second, ok := hexPathValue(w, r, "b", "run id")
	if !ok {
		return
	}
	if first == second {
		// A run against itself has no comparison in it: every delta is
		// zero by construction, and answering with a wall of zeroes
		// would dress a mistake up as a result (edge cases).
		writeError(w, http.StatusBadRequest, "a run cannot be compared with itself")
		return
	}
	a, ok := s.loadRun(w, r, project.ID, first)
	if !ok {
		return
	}
	b, ok := s.loadRun(w, r, project.ID, second)
	if !ok {
		return
	}
	if a.Dataset != b.Dataset {
		// Different datasets have no items in common, so an
		// item-by-item comparison cannot mean anything (#18).
		writeError(w, http.StatusBadRequest,
			"runs of different datasets cannot be compared: "+a.Dataset+" and "+b.Dataset)
		return
	}

	summaryA, err := s.store.RunSummary(r.Context(), project.ID, a)
	if err != nil {
		readFailed(w, r, "failed to summarize the runs", err)
		return
	}
	summaryB, err := s.store.RunSummary(r.Context(), project.ID, b)
	if err != nil {
		readFailed(w, r, "failed to summarize the runs", err)
		return
	}
	valuesA, err := s.store.RunValues(r.Context(), project.ID, a.ID)
	if err != nil {
		readFailed(w, r, "failed to compare the runs", err)
		return
	}
	valuesB, err := s.store.RunValues(r.Context(), project.ID, b.ID)
	if err != nil {
		readFailed(w, r, "failed to compare the runs", err)
		return
	}
	items, err := s.store.CompareItems(r.Context(), project.ID, a, b)
	if err != nil {
		readFailed(w, r, "failed to compare the runs", err)
		return
	}

	directions := map[string]string{}
	types := map[string]string{}
	for _, summary := range []*store.RunSummary{summaryA, summaryB} {
		for _, stat := range summary.Scores {
			types[stat.Name] = stat.DataType
			if stat.Direction != "" {
				directions[stat.Name] = stat.Direction
			}
		}
	}

	// The verdict counts are statements about the whole pair, so they are
	// counted over every compared item before the page is cut out of it
	// (Decision 31).
	tally := map[string]map[string]int64{}
	for _, item := range items {
		for name, verdict := range itemVerdicts(item, valuesA, valuesB, directions, types) {
			if tally[name] == nil {
				tally[name] = map[string]int64{}
			}
			tally[name][verdict.verdict]++
		}
	}

	page, prev, next, err := pageCompared(items, limit, backward, values.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows := make([]object, 0, len(page))
	for _, item := range page {
		scores := object{}
		verdicts := itemVerdicts(item, valuesA, valuesB, directions, types)
		for _, name := range sortedKeys(verdicts) {
			scores = scores.put(name, verdicts[name].render())
		}
		rows = append(rows, object{}.
			put("id", item.ItemID).
			put("seq", item.Seq).
			put("in", comparedIn(item, valuesA, valuesB)).
			put("scores", scores))
	}

	writeJSON(w, http.StatusOK, object{}.
		put("a", renderCompareSide(a)).
		put("b", renderCompareSide(b)).
		put("dataset", a.Dataset).
		put("same_version", a.DatasetVersion == b.DatasetVersion).
		put("metadata", metadataDiff(a.Metadata, b.Metadata)).
		put("models", object{}.put("a", summaryA.Models).put("b", summaryB.Models)).
		put("prompts", object{}.
			put("a", renderPrompts(summaryA.Prompts)).
			put("b", renderPrompts(summaryB.Prompts))).
		put("traces", compareTraces(summaryA.Traces, summaryB.Traces)).
		put("scores", compareScores(summaryA, summaryB, tally)).
		put("items", rows).
		put("next_cursor", next).
		put("prev_cursor", prev))
}

// renderCompareSide renders what a comparison says about one of its runs:
// enough to tell them apart, not the whole summary, which is already in the
// blocks below it.
func renderCompareSide(run *store.DatasetRun) object {
	return object{}.
		put("id", run.ID).
		put("name", nullable(run.Name)).
		put("dataset_version", run.DatasetVersion).
		put("status", run.Status).
		put("created_at", formatTime(run.CreatedAt))
}

func renderPrompts(prompts []store.PromptRef) []object {
	out := make([]object, 0, len(prompts))
	for _, prompt := range prompts {
		out = append(out, object{}.put("name", prompt.Name).put("version", prompt.Version))
	}
	return out
}

// compareTraces renders the traffic side of a comparison: both numbers and,
// for cost, the delta — the number somebody is going to quote.
func compareTraces(a, b store.RunTraceStats) object {
	return object{}.
		put("count", pair(a.Count, b.Count)).
		put("error_count", pair(a.ErrorCount, b.ErrorCount)).
		put("total_cost", object{}.
			put("a", a.TotalCost).
			put("b", b.TotalCost).
			put("delta", floatDelta(a.TotalCost, b.TotalCost))).
		put("latency_ms", object{}.
			put("p50", object{}.put("a", a.LatencyP50).put("b", b.LatencyP50)).
			put("p95", object{}.put("a", a.LatencyP95).put("b", b.LatencyP95)))
}

func pair[T any](a, b T) object { return object{}.put("a", a).put("b", b) }

// floatDelta is b - a, and nothing when either side has no number: a delta
// against an absent cost would be a claim that the cost was zero
// (spec 002 #14).
func floatDelta(a, b *float64) *float64 {
	if a == nil || b == nil {
		return nil
	}
	delta := *b - *a
	return &delta
}

// compareScores renders one row per score name either run carried, with each
// side's aggregate, the delta between the means and how many items moved.
func compareScores(a, b *store.RunSummary, tally map[string]map[string]int64) []object {
	statsA := statsByName(a.Scores)
	statsB := statsByName(b.Scores)
	names := make([]string, 0, len(statsA)+len(statsB))
	for name := range statsA {
		names = append(names, name)
	}
	for name := range statsB {
		if _, both := statsA[name]; !both {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]object, 0, len(names))
	for _, name := range names {
		left, right := statsA[name], statsB[name]
		row := object{}.
			put("name", name).
			put("data_type", firstDataType(left, right)).
			put("direction", nullable(firstDirection(left, right))).
			put("a", scoreSide(left)).
			put("b", scoreSide(right)).
			put("delta", meanDelta(left, right))
		counts := tally[name]
		for _, verdict := range []string{verdictImproved, verdictRegressed, verdictChanged, verdictSame} {
			// Only the words this name can actually reach are
			// reported: a name with no direction cannot improve, and
			// a zero under `improved` would suggest it could.
			if _, reachable := counts[verdict]; !reachable && !isCountedVerdict(left, right, verdict) {
				continue
			}
			row = row.put(verdict, counts[verdict])
		}
		out = append(out, row)
	}
	return out
}

// isCountedVerdict says whether a name reports a verdict at all: a directed
// name reports improved/regressed/same, an undirected one changed/same.
func isCountedVerdict(a, b *store.RunScoreStat, verdict string) bool {
	directed := firstDirection(a, b) == store.DirectionHigher ||
		firstDirection(a, b) == store.DirectionLower
	switch verdict {
	case verdictImproved, verdictRegressed:
		return directed
	case verdictChanged:
		return !directed
	default:
		return true
	}
}

func statsByName(stats []store.RunScoreStat) map[string]*store.RunScoreStat {
	out := map[string]*store.RunScoreStat{}
	for i := range stats {
		out[stats[i].Name] = &stats[i]
	}
	return out
}

func firstDataType(a, b *store.RunScoreStat) string {
	if a != nil {
		return a.DataType
	}
	if b != nil {
		return b.DataType
	}
	return ""
}

func firstDirection(a, b *store.RunScoreStat) string {
	if a != nil && a.Direction != "" {
		return a.Direction
	}
	if b != nil {
		return b.Direction
	}
	return ""
}

// scoreSide renders one run's aggregate for a name, or null when that run
// never carried it — which is a fact about the run, not a zero.
func scoreSide(stat *store.RunScoreStat) any {
	if stat == nil {
		return nil
	}
	side := object{}.put("count", stat.Count)
	if stat.Distribution != nil {
		return side.put("distribution", renderDistribution(stat.Distribution))
	}
	return side.put("mean", stat.Mean)
}

func meanDelta(a, b *store.RunScoreStat) *float64 {
	if a == nil || b == nil {
		return nil
	}
	return floatDelta(a.Mean, b.Mean)
}

// comparedVerdict is one item's before and after for one score name.
type comparedVerdict struct {
	dataType string
	a, b     *store.ItemScore
	verdict  string
}

func (v comparedVerdict) render() object {
	out := object{}
	if v.dataType == store.ScoreCategorical || v.dataType == store.ScoreText {
		return out.
			put("a", nullableScoreText(v.a)).
			put("b", nullableScoreText(v.b)).
			put("verdict", v.verdict)
	}
	return out.
		put("a", scoreMean(v.a)).
		put("b", scoreMean(v.b)).
		put("delta", floatDelta(scoreMean(v.a), scoreMean(v.b))).
		put("verdict", v.verdict)
}

func scoreMean(score *store.ItemScore) *float64 {
	if score == nil {
		return nil
	}
	return score.Mean
}

func nullableScoreText(score *store.ItemScore) any {
	if score == nil {
		return nil
	}
	return score.Text
}

// itemVerdicts decides what happened to one item, name by name. A name only
// one side scored has no verdict — there is nothing to compare it with — and
// is left out rather than reported as a change from nothing.
func itemVerdicts(item *store.CompareItem, a, b *store.RunItemValues,
	directions, types map[string]string) map[string]comparedVerdict {
	out := map[string]comparedVerdict{}
	for name := range unionNames(a.Scores[item.ItemID], b.Scores[item.ItemID]) {
		left, hasLeft := a.Scores[item.ItemID][name]
		right, hasRight := b.Scores[item.ItemID][name]
		if !hasLeft || !hasRight {
			continue
		}
		dataType := types[name]
		if dataType == "" {
			dataType = left.DataType
		}
		out[name] = comparedVerdict{
			dataType: dataType,
			a:        &left,
			b:        &right,
			verdict:  verdictOf(dataType, directions[name], left, right),
		}
	}
	return out
}

// verdictOf applies #16: a direction turns a difference into a judgement, and
// without one the most a comparison can honestly say is that something changed.
// Equality is exact, which the docs state, because a tolerance would be a
// number the store invented.
func verdictOf(dataType, direction string, a, b store.ItemScore) string {
	if dataType == store.ScoreCategorical || dataType == store.ScoreText {
		if a.Text == b.Text {
			return verdictSame
		}
		return verdictChanged
	}
	if a.Mean == nil || b.Mean == nil {
		return verdictSame
	}
	switch {
	case *a.Mean == *b.Mean:
		return verdictSame
	case direction == store.DirectionHigher:
		if *b.Mean > *a.Mean {
			return verdictImproved
		}
		return verdictRegressed
	case direction == store.DirectionLower:
		if *b.Mean < *a.Mean {
			return verdictImproved
		}
		return verdictRegressed
	default:
		return verdictChanged
	}
}

func unionNames(a, b map[string]store.ItemScore) map[string]bool {
	names := make(map[string]bool, len(a)+len(b))
	for name := range a {
		names[name] = true
	}
	for name := range b {
		names[name] = true
	}
	return names
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// comparedIn labels which side of the comparison an item belongs to. The
// version labels come first: an item missing from the other run's version was
// never that run's to answer, and saying merely "a" would blame the run for
// the dataset's history (#18).
func comparedIn(item *store.CompareItem, a, b *store.RunItemValues) string {
	switch {
	case !item.InVersionB:
		return "only_in_version_a"
	case !item.InVersionA:
		return "only_in_version_b"
	case a.Attempted[item.ItemID] && b.Attempted[item.ItemID]:
		return "both"
	case a.Attempted[item.ItemID]:
		return "a"
	default:
		return "b"
	}
}

// pageCompared cuts the page out of the compared items. The keyset is `seq`,
// the same order the item view walks, so a reader can hold one cursor in mind
// for both.
//
// A cursor it cannot read is an error, not a silent restart from the first
// page: that is spec 003 #23's rule, and the walk it would restart never ends
// (the reasoning under `addCursor` in the CLI). The item view refuses the same
// input, and one of the two answering 200 was the inconsistency review of PR
// #31 caught.
func pageCompared(items []*store.CompareItem, limit int, backward bool, cursor string) (
	page []*store.CompareItem, prev, next *string, err error,
) {
	if cursor != "" {
		parts, err := decodeCursor(cursor, 1)
		if err != nil {
			return nil, nil, nil, err
		}
		seq, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, nil, nil, errors.New("invalid cursor")
		}
		items = filterBySeq(items, seq, backward)
	}
	if backward && len(items) > limit {
		items = items[len(items)-limit-1:]
	}
	page, prev, next = trimPage(items, limit, backward, cursor, func(item *store.CompareItem) string {
		return encodeCursor(strconv.FormatInt(item.Seq, 10))
	})
	return page, prev, next, nil
}

func filterBySeq(items []*store.CompareItem, seq int64, backward bool) []*store.CompareItem {
	kept := make([]*store.CompareItem, 0, len(items))
	for _, item := range items {
		if (backward && item.Seq < seq) || (!backward && item.Seq > seq) {
			kept = append(kept, item)
		}
	}
	return kept
}

// metadataDiff lists the metadata keys the two runs disagree about, both
// values, compact (#18). Keys they share and agree on are noise: the question
// the block answers is "what was different about these two attempts".
func metadataDiff(a, b []byte) object {
	left, right := decodeMetadata(a), decodeMetadata(b)
	diff := object{}
	for _, key := range sortedKeys(unionValues(left, right)) {
		leftValue, inLeft := left[key]
		rightValue, inRight := right[key]
		if inLeft && inRight && string(leftValue) == string(rightValue) {
			continue
		}
		diff = diff.put(key, object{}.
			put("a", rawJSON(leftValue)).
			put("b", rawJSON(rightValue)))
	}
	return diff
}

// decodeMetadata reads a run's metadata into its top-level keys, each still
// raw JSON: the store never looks inside a value (#4), and the diff compares
// the compact bytes it stored.
func decodeMetadata(raw []byte) map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func unionValues(a, b map[string]json.RawMessage) map[string]bool {
	keys := make(map[string]bool, len(a)+len(b))
	for key := range a {
		keys[key] = true
	}
	for key := range b {
		keys[key] = true
	}
	return keys
}

// boolParam reads a flag parameter that is spelled out: `true` or `false` and
// nothing else, because a parameter that silently reads "1" as true and "yes"
// as false is a trap (spec 003 #23).
func boolParam(values interface{ Get(string) string }, name string) (bool, error) {
	switch raw := values.Get(name); raw {
	case "":
		return false, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false, got %q", name, raw)
	}
}
