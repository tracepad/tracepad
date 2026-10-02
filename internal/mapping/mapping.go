// Package mapping turns a decoded OTLP trace export into the trace model
// (spec 002). It knows two attribute dialects — OTel GenAI semconv and
// `langfuse.*` — organized as a priority table per target field, and it never
// drops an attribute: whatever no rule claims lands in the observation's
// metadata (spec 002 #10, #11).
//
// Attribute semantics for the `langfuse.*` dialect are derived from Langfuse's
// MIT-licensed OtelIngestionProcessor; see NOTICE.
package mapping

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/model"
)

// Result is one mapped export.
type Result struct {
	// Traces are already merged within this export; merging across
	// exports is the store's job (spec 002 #6).
	Traces []*model.Trace
	// Observations keeps every span that mapped, in arrival order per
	// natural key, so that a span delivered twice in one body still
	// resolves to last-wins.
	Observations []*model.Observation
	// Skipped counts spans that could not be mapped; they are reported in
	// OTLP partial_success and remain recoverable from the raw body
	// (spec 002 #13).
	Skipped int64
	// SkipReason summarizes why, for the partial_success message.
	SkipReason string
	// Unreadable counts ResourceSpans blocks that did not decode. They are
	// reported as a warning rather than added to Skipped: OTLP's
	// rejected_spans counts spans, and how many spans an undecodable block
	// held is exactly what we could not find out.
	Unreadable int
	// Dialect labels the export for `raw_batches`, so a later remap can
	// find the bodies a changed rule affects.
	Dialect string
}

// Dialect labels.
const (
	DialectLangfuse = "langfuse"
	DialectTracepad = "tracepad"
	DialectGenAI    = "genai"
	DialectOTel     = "otel"
)

// spanCtx is a span together with the attribute scopes above it.
type spanCtx struct {
	span     *tracepb.Span
	resource []*commonpb.KeyValue
	scope    []*commonpb.KeyValue
	// scopeName and scopeVersion are the InstrumentationScope's own
	// fields, which are not attributes at all in OTLP and are the answer
	// to "which SDK sent this" (spec 012 #7).
	scopeName    string
	scopeVersion string
}

// Map converts a decoded export into the trace model. It never fails: a span
// it cannot use is counted, not raised, because one bad span must not reject
// the batch (spec 002 #13).
func Map(resourceSpans []*tracepb.ResourceSpans) *Result {
	spans := flatten(resourceSpans)

	// A zero-duration span is an event only if nothing else in the export
	// calls it a parent (spec 002 #12). Cross-batch children are invisible
	// here; that is the same order-tolerance trade-off as trace merging.
	parents := map[string]bool{}
	for _, sc := range spans {
		if id := spanID(sc.span.GetParentSpanId()); id != "" {
			parents[id] = true
		}
	}

	res := &Result{Dialect: DialectOTel}
	traces := map[string]*traceAccumulator{}
	var order []string
	reasons := map[string]int{}

	for _, sc := range spans {
		obs, tf, reason := mapSpan(sc, parents)
		if reason != "" {
			res.Skipped++
			reasons[reason]++
			continue
		}
		res.Observations = append(res.Observations, obs)

		acc, ok := traces[obs.TraceID]
		if !ok {
			acc = &traceAccumulator{trace: &model.Trace{ID: obs.TraceID}}
			traces[obs.TraceID] = acc
			order = append(order, obs.TraceID)
		}
		acc.apply(tf)
		res.Dialect = maxDialect(res.Dialect, tf.dialect)
	}

	for _, id := range order {
		res.Traces = append(res.Traces, boundLabels(traces[id].finish()))
	}
	for _, obs := range res.Observations {
		obs.Name = CutLabel(obs.Name)
		obs.Model = CutLabel(obs.Model)
	}
	res.SkipReason = summarize(reasons)

	sort.SliceStable(res.Traces, func(i, j int) bool { return res.Traces[i].ID < res.Traces[j].ID })
	sort.SliceStable(res.Observations, func(i, j int) bool {
		a, b := res.Observations[i], res.Observations[j]
		if a.TraceID != b.TraceID {
			return a.TraceID < b.TraceID
		}
		return a.ID < b.ID
	})
	return res
}

// CountSpans is how many spans a decoded export carries, the number
// TRACEPAD_MAX_SPANS_PER_REQUEST bounds (spec 043 #10): counted before mapping,
// so a refusal costs no more than the decoding it follows.
func CountSpans(resourceSpans []*tracepb.ResourceSpans) int {
	n := 0
	for _, rs := range resourceSpans {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return n
}

// The bounds of the labels a listing shows (spec 043 #14): each is cut at
// MaxLabelLength characters, and a trace keeps its first MaxTags distinct tags.
// Constants, not settings: they shape what is stored (#22). The raw body keeps
// what was sent, and a remap goes through here again.
const (
	MaxLabelLength = 1000
	MaxTags        = 50
)

// boundLabels applies the label bounds to a merged trace: its name, user,
// session, environment, release and version. Its tags were bounded as the
// spans gave them (UnionTags).
func boundLabels(t *model.Trace) *model.Trace {
	for _, field := range []*string{&t.Name, &t.UserID, &t.SessionID, &t.Environment, &t.Release, &t.Version} {
		*field = CutLabel(*field)
	}
	return t
}

// UnionTags is the one bound on a trace's tags (spec 043 #14, spec 002 #32),
// for the spans of one export and for a delivery onto the stored trace alike:
// tags followed by each of added it does not already hold, every tag cut at
// MaxLabelLength before it is compared — so two that differ only past the cut
// are one tag, as they would be stored — and none past MaxTags, so the first
// tags a trace was given stay. seen is what tags holds, kept up to date for a
// caller that adds again; nil builds it from tags.
func UnionTags(tags []string, seen map[string]bool, added []string) ([]string, map[string]bool) {
	if seen == nil {
		seen = make(map[string]bool, len(tags)+len(added))
		for _, tag := range tags {
			seen[tag] = true
		}
	}
	for _, tag := range added {
		if len(tags) >= MaxTags {
			break
		}
		tag = CutLabel(tag)
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags, seen
}

// CutLabel cuts a label to MaxLabelLength characters, at a character
// boundary. A lookup by a label goes through it too (spec 043 #34): a value
// stored cut and looked up whole would match nothing.
func CutLabel(s string) string {
	if len(s) <= MaxLabelLength {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxLabelLength {
			return s[:i]
		}
		n++
	}
	return s
}

func flatten(resourceSpans []*tracepb.ResourceSpans) []spanCtx {
	var out []spanCtx
	for _, rs := range resourceSpans {
		if rs == nil {
			continue
		}
		for _, ss := range rs.GetScopeSpans() {
			if ss == nil {
				continue
			}
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				out = append(out, spanCtx{
					span:         span,
					resource:     rs.GetResource().GetAttributes(),
					scope:        ss.GetScope().GetAttributes(),
					scopeName:    ss.GetScope().GetName(),
					scopeVersion: ss.GetScope().GetVersion(),
				})
			}
		}
	}
	return out
}

// rankedValue is one trace-level value together with how good the key that
// produced it was: the index of that key in its priority chain, lower being
// better. A value nobody produced has an empty string and is ignored.
type rankedValue struct {
	value string
	rank  int
}

// traceFields are the trace-level values one span contributed.
type traceFields struct {
	name        rankedValue
	rootName    string
	userID      rankedValue
	sessionID   rankedValue
	environment rankedValue
	release     rankedValue
	version     rankedValue
	runID       rankedValue
	itemID      rankedValue
	tags        []string
	metadata    map[string]any
	dialect     string
}

// rankedField merges one trace-level field across the spans of an export.
//
// A chain is resolved per span, but the field belongs to the trace, and the
// two disagree whenever the winning key is on one span and a lower-priority
// key of the same chain is on the Resource — which every span of the export
// can see. Merging by "last non-empty wins" alone would then let the fallback
// beat the explicit key on any trace longer than one span, which is the
// ordinary shape of traffic: `langfuse.release` on the root and
// `service.version` on the resource would store the latter (spec 012 #11).
//
// So a lower-priority source never overwrites a higher-priority one. At equal
// rank the later value still wins, because that is spec 002 #6 and it is the
// right rule for two spans that genuinely disagree about the same key.
type rankedField struct {
	value string
	rank  int
	set   bool
}

func (f *rankedField) merge(v rankedValue) {
	if v.value == "" || (f.set && v.rank > f.rank) {
		return
	}
	f.value, f.rank, f.set = v.value, v.rank, true
}

// traceAccumulator merges per-span contributions field-wise (spec 002 #6,
// spec 012 #11).
type traceAccumulator struct {
	trace       *model.Trace
	rootName    string
	name        rankedField
	userID      rankedField
	sessionID   rankedField
	environment rankedField
	release     rankedField
	version     rankedField
	runID       rankedField
	itemID      rankedField
	// tagSeen is the tags kept so far, as cut: a tag is kept once, and
	// none after the first MaxTags.
	tagSeen map[string]bool
}

func (a *traceAccumulator) apply(tf *traceFields) {
	a.name.merge(tf.name)
	setIf(&a.rootName, tf.rootName)
	a.userID.merge(tf.userID)
	a.sessionID.merge(tf.sessionID)
	a.environment.merge(tf.environment)
	a.release.merge(tf.release)
	a.version.merge(tf.version)
	a.runID.merge(tf.runID)
	a.itemID.merge(tf.itemID)
	// Tags and metadata are sets rather than single values, so a span adds
	// to them instead of replacing them (spec 002 #32): the tags as a union
	// in the order they were first seen — cut and deduplicated as they come,
	// and no more once MaxTags are kept, so a thousand spans repeating their
	// tags hold fifty — the metadata key by key with the later span's value
	// winning.
	if len(tf.tags) > 0 {
		a.trace.Tags, a.tagSeen = UnionTags(a.trace.Tags, a.tagSeen, tf.tags)
	}
	if len(tf.metadata) > 0 {
		if a.trace.Metadata == nil {
			a.trace.Metadata = make(map[string]any, len(tf.metadata))
		}
		for k, v := range tf.metadata {
			a.trace.Metadata[k] = v
		}
	}
}

func (a *traceAccumulator) finish() *model.Trace {
	a.trace.Name = a.name.value
	a.trace.UserID = a.userID.value
	a.trace.SessionID = a.sessionID.value
	a.trace.Environment = a.environment.value
	a.trace.Release = a.release.value
	a.trace.Version = a.version.value
	a.trace.RunID = a.runID.value
	// An item is a position inside a run: without a run on the trace it
	// names nothing, and the column stays empty (spec 014, ingest
	// contract). This is where that rule is applied, because it is the
	// first point at which every span of the export has been seen.
	if a.trace.RunID != "" {
		a.trace.ItemID = a.itemID.value
	}
	// An explicit trace name beats the root span's name; the root span may
	// also simply not be in this export yet.
	if a.trace.Name == "" {
		a.trace.Name = a.rootName
	}
	return a.trace
}

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// mapSpan maps one span. The returned reason is empty on success and names
// the defect otherwise.
func mapSpan(sc spanCtx, parents map[string]bool) (*model.Observation, *traceFields, string) {
	span := sc.span

	traceID := traceID(span.GetTraceId())
	if traceID == "" {
		return nil, nil, "invalid trace id"
	}
	id := spanID(span.GetSpanId())
	if id == "" {
		return nil, nil, "invalid span id"
	}

	a := newAttrs()
	a.merge(originResource, sc.resource)
	a.merge(originScope, sc.scope)
	a.merge(originSpan, span.GetAttributes())

	tf := &traceFields{dialect: dialectOf(a.values)}
	tf.name = a.firstRanked(traceNameKeys...)
	tf.userID = a.firstRanked(traceUserKeys...)
	tf.sessionID = a.firstRanked(traceSessionKeys...)
	tf.environment = a.firstRanked(traceEnvironmentKeys...)
	tf.release = a.firstRanked(traceReleaseKeys...)
	tf.version = a.firstRanked(traceVersionKeys...)
	tf.runID, tf.itemID = mapRunLink(a)
	tf.tags = mapTags(a)
	tf.metadata = mapMetadata(a, traceMetadataKeys...)
	if spanID(span.GetParentSpanId()) == "" {
		tf.rootName = span.GetName()
	}

	obs := &model.Observation{
		TraceID:             traceID,
		ID:                  id,
		ParentObservationID: spanID(span.GetParentSpanId()),
		Name:                span.GetName(),
		StartTime:           int64(span.GetStartTimeUnixNano()),
		EndTime:             int64(span.GetEndTimeUnixNano()),
	}

	level, explicitLevel := mapLevel(a, span.GetStatus())
	obs.Level = level
	obs.StatusMessage = mapStatusMessage(a, span.GetStatus())
	// An exception event is how OTel reports a failure, and a span
	// carrying one is a failed span even when the exporter left the span
	// status unset — otherwise it would miss error_count and every error
	// filter built on it (spec 002 Decision 26).
	if event := exceptionEvent(span); event != nil {
		if !explicitLevel {
			obs.Level = model.LevelError
		}
		if obs.StatusMessage == "" {
			obs.StatusMessage = exceptionMessage(event)
		}
	}
	obs.Model, _ = a.firstString(obsModelKeys...)
	obs.CompletionStartTime = mapCompletionStartTime(a)
	obs.PromptName, obs.PromptVersion = mapPrompt(a)
	obs.ModelParameters = mapModelParameters(a)
	obs.Usage = mapUsage(a)
	obs.CostDetails = mapCost(a)
	obs.Input = mapPayload(a, obsInputKeys, genAIPromptPrefix)
	obs.Output = mapPayload(a, obsOutputKeys, genAICompletion)

	typeMetadata := map[string]any{}
	obs.Type = mapType(a, obs, parents[id], typeMetadata)

	// The scope's own name and version, and the events, go in after the
	// attributes so that an attribute literally called `scope.name` or
	// `events` cannot hide the thing it is named after. Both are facts
	// about the span that OTLP does not carry as attributes at all.
	scopeMetadata := map[string]any{}
	if sc.scopeName != "" {
		scopeMetadata[metadataScopeName] = sc.scopeName
	}
	if sc.scopeVersion != "" {
		scopeMetadata[metadataScopeVersion] = sc.scopeVersion
	}
	eventMetadata := map[string]any{}
	if events := mapEvents(span); events != nil {
		eventMetadata[metadataEventsKey] = events
	}
	obs.Metadata = mergeMetadata(mapMetadata(a, obsMetadataKeys...), typeMetadata, a.rest(),
		scopeMetadata, eventMetadata)

	return obs, tf, ""
}

// mapRunLink reads the run and the item a span says its trace belongs to
// (spec 014 #2). Either is claimed only when it has the shape of an id the
// store hands out — 32 lower-case hex characters — and any other shape stays
// in metadata unclaimed, where a harness that stamped a run *name* rather
// than the id will find it.
//
// The rule for the item is trace-level, not span-level (spec 014, ingest
// contract): a harness may put the run on the root span and the item on the
// span that did the work, and the two meet in the trace merge. So an item on
// a span with no run of its own is still read — but not claimed, because
// whether it names anything is not known until every span has been seen. If
// no run turns up, `traceAccumulator.finish` drops it and the attribute is
// where the harness left it, in the span's metadata; if one does, the item
// links the trace and stays visible beside it, which is the honest price of
// reading it before its run exists.
func mapRunLink(a *attrs) (run, item rankedValue) {
	run = firstHexID(a, true, traceRunKeys...)
	return run, firstHexID(a, run.value != "", traceItemKeys...)
}

// firstHexID is firstRanked for a chain whose value must be a 32-hex id.
// `claim` says whether the winning key leaves the metadata with its value.
func firstHexID(a *attrs, claim bool, keys ...string) rankedValue {
	for rank, key := range keys {
		raw, ok := a.lookup(key)
		if !ok {
			continue
		}
		id, valid := raw.(string)
		if !valid || !IsHexID(id) {
			continue
		}
		if claim {
			a.claim(key)
		}
		return rankedValue{value: id, rank: rank}
	}
	return rankedValue{}
}

// IsHexID reports the shape of a store-issued id: 32 lower-case hex digits.
func IsHexID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// mapLevel: an explicit level wins; otherwise an ERROR span status raises
// the level, and everything else is DEFAULT. A spelling the alias table does
// not know is left unclaimed, so it surfaces in metadata rather than being
// swallowed by the fallback.
//
// The second return says whether the level came from an attribute the client
// set deliberately. Only the fallbacks may be overridden by an exception
// event: a client that said DEBUG meant DEBUG.
func mapLevel(a *attrs, status *tracepb.Status) (string, bool) {
	if key, raw, ok := a.first(obsLevelKeys...); ok {
		if level, known := levelAliases[strings.ToUpper(strings.TrimSpace(asString(raw)))]; known {
			a.claim(key)
			return level, true
		}
	}
	if status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		return model.LevelError, false
	}
	return model.LevelDefault, false
}

// mapEvents renders a span's events for metadata. Event attributes are their
// own namespace — they never took part in the attribute mapping above — so
// they are carried wholesale rather than consumed.
func mapEvents(span *tracepb.Span) []any {
	out := make([]any, 0, len(span.GetEvents()))
	for _, event := range span.GetEvents() {
		if event == nil {
			continue
		}
		entry := map[string]any{}
		for _, kv := range event.GetAttributes() {
			if kv == nil || kv.Key == "" {
				continue
			}
			entry[kv.Key] = anyValue(kv.Value)
		}
		// Written last: an event whose attribute happens to be called
		// "name" would otherwise leave the entry unidentifiable, and
		// the raw body still holds the original either way.
		entry["name"] = event.GetName()
		entry["time"] = int64(event.GetTimeUnixNano())
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// exceptionEvent returns the first exception event on a span, the OTel
// semconv way of recording a failure.
func exceptionEvent(span *tracepb.Span) *tracepb.Span_Event {
	for _, event := range span.GetEvents() {
		if event.GetName() == eventException {
			return event
		}
	}
	return nil
}

// exceptionMessage renders an exception event as a status message: the
// message if there is one, else the type, which at least names the failure.
func exceptionMessage(event *tracepb.Span_Event) string {
	var exceptionType string
	for _, kv := range event.GetAttributes() {
		switch kv.GetKey() {
		case eventExceptionMessage:
			if message := asString(anyValue(kv.Value)); message != "" {
				return message
			}
		case eventExceptionType:
			exceptionType = asString(anyValue(kv.Value))
		}
	}
	return exceptionType
}

func mapStatusMessage(a *attrs, status *tracepb.Status) string {
	if msg, ok := a.firstString(obsStatusMessageKeys...); ok {
		return msg
	}
	return status.GetMessage()
}

// mapType implements spec 002 #12 with the vocabulary spec 012 #2 widened it
// to. An explicit Langfuse type wins and is stored as sent; a model attribute
// in any dialect means a generation; a zero-duration childless span is an
// event; everything else is a span. A spelling outside the ten is preserved
// in metadata and left to the heuristics, exactly as before — the column
// still cannot hold it.
func mapType(a *attrs, obs *model.Observation, hasChildren bool, meta map[string]any) string {
	if key, raw, ok := a.first(obsTypeKeys...); ok {
		spelling := strings.ToLower(strings.TrimSpace(asString(raw)))
		a.claim(key)
		if observationTypes[spelling] {
			return spelling
		}
		// An unknown spelling is preserved and left to the heuristics.
		meta[key] = raw
	}
	if obs.Model != "" {
		return model.TypeGeneration
	}
	if obs.StartTime == obs.EndTime && !hasChildren {
		return model.TypeEvent
	}
	return model.TypeSpan
}

// mapCompletionStartTime resolves when the first token came back, in Unix
// nanoseconds (spec 012 #3). Three shapes are accepted, and the attribute is
// claimed only for the ones that parse — anything else stays visible in
// metadata rather than being silently dropped for having the wrong type.
func mapCompletionStartTime(a *attrs) int64 {
	for _, key := range obsCompletionStartKeys {
		raw, ok := a.lookup(key)
		if !ok {
			continue
		}
		instant, parsed := parseInstant(raw)
		if !parsed {
			continue
		}
		a.claim(key)
		return instant
	}
	return 0
}

// The instants an int64 of nanoseconds can hold.
var (
	earliestInstant = time.Unix(0, math.MinInt64)
	latestInstant   = time.Unix(0, math.MaxInt64)
)

// parseInstant reads the shapes an SDK sends an instant in: an integer of
// nanoseconds, an RFC 3339 string, and that same string with a layer of JSON
// quoting still around it — which is what the Langfuse SDK 4.7 emits on the
// wire (verified 2026-08-30). One layer is stripped, not all of them: a value
// quoted twice over is a client bug, not a convention.
func parseInstant(raw any) (int64, bool) {
	switch value := raw.(type) {
	case int64:
		return value, true
	case float64:
		// A double large enough to hold nanoseconds has already lost
		// precision, but the alternative is losing the value. What no
		// precision survives is a magnitude int64 cannot hold: `1e30` is
		// as whole as any other double, and converting it is
		// implementation-defined — it saturates on arm64 and wraps on
		// amd64, either way storing an instant no clock produced and
		// subtracting it into a TTFT of some 10^12 milliseconds. Out of
		// range is not a nanosecond count, so it stays unclaimed and
		// falls through to metadata with every other shape this cannot
		// read (found in review of PR #19). The infinities are screened
		// earlier — `attrValue` keeps a non-finite double as its textual
		// form, because JSON has no way to spell one — and NaN would
		// fail the integrality check anyway, every comparison against it
		// being false. The guard covers them regardless: this function
		// reads a value, not a wire format.
		if value != math.Trunc(value) || value >= math.MaxInt64 || value < math.MinInt64 {
			return 0, false
		}
		return int64(value), true
	case string:
		text := strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(text); err == nil && strings.HasPrefix(text, `"`) {
			text = strings.TrimSpace(unquoted)
		}
		if nanoseconds, err := strconv.ParseInt(text, 10, 64); err == nil {
			return nanoseconds, true
		}
		instant, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || instant.Before(earliestInstant) || instant.After(latestInstant) {
			// Outside the years 1678–2262 `UnixNano` is undefined: a
			// date in the year 3000 came back as an arbitrary
			// nanosecond count. Not an instant, so it stays in
			// metadata with every other shape this cannot read
			// (spec 043 #5).
			return 0, false
		}
		return instant.UnixNano(), true
	}
	return 0, false
}

// mapPrompt reads the prompt the client said this observation ran. The name
// and the version are independent: a version that is not an integer stays in
// metadata and the name is still recorded, because "which prompt" is the
// question the filter answers and half an answer beats none (spec 012 #5).
//
// A version has to count from one. `prompt=` reads a version as a run of
// digits (spec 012 #15), so a zero or negative one would be a value the store
// holds and no filter string can ask for — the interface would build a badge
// linking at an empty listing. It stays in metadata like every other shape
// this cannot use (found in review of PR #19).
func mapPrompt(a *attrs) (string, *int64) {
	name, ok := a.firstString(obsPromptNameKeys...)
	if !ok {
		return "", nil
	}
	key, raw, present := a.first(obsPromptVersionKeys...)
	if !present {
		return name, nil
	}
	version, integral := asInteger(raw)
	if !integral || version < 1 {
		return name, nil
	}
	a.claim(key)
	return name, &version
}

// mapModelParameters: an explicit JSON object wins; otherwise every
// `gen_ai.request.*` attribute except the model name is collected.
func mapModelParameters(a *attrs) map[string]any {
	for _, key := range []string{lfObsModelParameters, lfObsModelParametersAlt} {
		if raw, ok := a.lookup(key); ok {
			// Claimed only if it really is an object: an attribute
			// that does not parse stays visible in metadata.
			if obj, valid := parseJSONObject(raw); valid {
				a.claim(key)
				return obj
			}
		}
	}
	out := map[string]any{}
	for k, v := range a.values {
		if !strings.HasPrefix(k, genAIRequestPrefix) || isEmpty(v) {
			continue
		}
		name := k[len(genAIRequestPrefix):]
		if name == "model" { // a model name, not a parameter
			continue
		}
		a.claim(k)
		out[name] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapUsage: an explicit usage_details object wins; otherwise every
// `gen_ai.usage.<key>` is collected verbatim, minus the cost (which is a
// price, not a token count, and feeds the cost chain instead); otherwise the
// bare token keys of spec 030 #1.
//
// The three sources are a chain, not a merge. An exporter that sends both
// `gen_ai.usage.input_tokens` and `input_tokens` is describing one number
// twice, and the standard spelling wins whole — the bare keys then stay
// unclaimed in metadata, where both readings are still visible.
func mapUsage(a *attrs) map[string]any {
	if raw, ok := a.lookup(lfObsUsageDetails); ok {
		if obj, valid := parseJSONObject(raw); valid {
			a.claim(lfObsUsageDetails)
			return obj
		}
	}
	out := map[string]any{}
	for k, v := range a.values {
		if !strings.HasPrefix(k, genAIUsagePrefix) || k == genAIUsageCost {
			continue
		}
		n, ok := asNumber(v)
		if !ok {
			// Not a count: leave it unclaimed so it survives in
			// metadata rather than being coerced into a number.
			continue
		}
		a.claim(k)
		out[k[len(genAIUsagePrefix):]] = jsonNumber(n)
	}
	if len(out) > 0 {
		return out
	}
	// Third source: the bare spellings, walked in the order rules.go lists
	// them so the result never depends on map iteration. The key is kept as
	// sent — `cache_read_tokens` stays `cache_read_tokens` — the way a
	// `gen_ai.usage.*` key keeps its suffix; renaming it to something
	// canonical would be a fourth vocabulary (spec 030 #1).
	for _, key := range bareUsageKeys {
		raw, ok := a.lookup(key)
		if !ok {
			continue
		}
		n, valid := asNumber(raw)
		if !valid {
			// Not a count, and a bare word is the likeliest key to
			// mean something else entirely: leave it in metadata.
			continue
		}
		a.claim(key)
		out[key] = jsonNumber(n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapCost returns the client-provided cost, with a `total` derived from the
// components when none was sent and their sum is finite (spec 002 #14: cost is
// never estimated, only recorded; spec 043 #5). A sum that is not finite
// leaves the components as sent and no `total`, which the store counts as no
// data.
func mapCost(a *attrs) map[string]any {
	var out map[string]any
	if raw, ok := a.lookup(lfObsCostDetails); ok {
		if obj, valid := parseJSONObject(raw); valid {
			a.claim(lfObsCostDetails)
			out = obj
		}
	}
	if out == nil {
		if raw, ok := a.lookup(genAIUsageCost); ok {
			if n, valid := asNumber(raw); valid {
				a.claim(genAIUsageCost)
				out = map[string]any{"total": jsonNumber(n)}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	// A total written as a string that is a number is the number: SQLite's
	// `SUM` always counted `"0.25"`, and the store's counting rule still does
	// (spec 043 #24). A string that is not one — "abc", or ".5", which Go
	// would parse and the rule does not — stays as sent (#24 u).
	if text, ok := out["total"].(string); ok {
		if n, valid := jsonNumberText(text); valid {
			out["total"] = jsonNumber(n)
		}
	}
	if _, ok := out["total"]; !ok {
		// The trace list sums one number per observation; deriving it
		// once here beats teaching every reader the component names.
		var total float64
		var hasComponent bool
		// In key order: with components near the largest double, whether
		// the sum overflows depends on the order it is taken in, and the
		// same span must store the same thing on every delivery.
		keys := make([]string, 0, len(out))
		for key := range out {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if n, ok := asNumber(out[key]); ok {
				total += n
				hasComponent = true
			}
		}
		// Each component is finite already, but two near the largest
		// double sum to an infinity, which the store cannot encode — so
		// the whole batch failed, one bad span taking every good one
		// with it (spec 002 #13). No total is derived then: the
		// components are kept as sent and the cost counts as no data
		// (spec 043 #5).
		if hasComponent && model.Finite(total) {
			out["total"] = jsonNumber(total)
		}
	}
	return out
}

// mapPayload resolves an input/output chain. Beyond the plain keys it
// reassembles the flattened form (`gen_ai.prompt.0.content`) that
// message-per-attribute instrumentations emit.
func mapPayload(a *attrs, keys []string, flatPrefix string) any {
	if key, v, ok := a.first(keys...); ok {
		a.claim(key)
		return looseJSON(v)
	}
	return reassemble(a, flatPrefix)
}

// mapTags parses the trace tags attribute. Arrays pass through; a string is
// tried as a JSON array, then as a comma-separated list, then as a single
// tag — the three shapes SDKs actually send.
func mapTags(a *attrs) []string {
	key, raw, ok := a.first(traceTagsKeys...)
	if !ok {
		return nil
	}
	a.claim(key)
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, asString(item))
		}
		return out
	case string:
		if strings.HasPrefix(strings.TrimSpace(v), "[") {
			var parsed []any
			if err := json.Unmarshal([]byte(v), &parsed); err == nil {
				out := make([]string, 0, len(parsed))
				for _, item := range parsed {
					out = append(out, asString(item))
				}
				return out
			}
		}
		if strings.Contains(v, ",") {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, part := range parts {
				out = append(out, strings.TrimSpace(part))
			}
			return out
		}
		return []string{v}
	}
	return nil
}

// mapMetadata collects metadata prefixes in both shapes SDKs use: a JSON
// object at the bare key, and one attribute per entry underneath it. Prefixes
// come highest-priority first and are read in reverse, so that where two
// dialects name the same entry the higher-priority one is what stays.
func mapMetadata(a *attrs, prefixes ...string) map[string]any {
	out := map[string]any{}
	for i := len(prefixes) - 1; i >= 0; i-- {
		prefix := prefixes[i]
		if raw, ok := a.lookup(prefix); ok {
			if obj, valid := parseJSONObject(raw); valid {
				a.claim(prefix)
				for k, v := range obj {
					out[k] = v
				}
			}
		}
		for k, v := range a.prefixed(prefix) {
			out[k] = looseJSON(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mergeMetadata(parts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, part := range parts {
		for k, v := range part {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// looseJSON parses a string that carries JSON so that a structured payload is
// stored as structure rather than as a quoted blob. A string that is not JSON
// is a legitimate payload and passes through unchanged.
func looseJSON(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return v
	}
	var out any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return v
	}
	return out
}

// reassemble rebuilds a value that an instrumentation flattened into
// attribute names: `gen_ai.prompt.0.content` becomes [{content: …}]. Maps
// whose keys are exactly 0..n-1 become arrays, which is what makes the
// message-list case come out as a list.
func reassemble(a *attrs, prefix string) any {
	nested := map[string]any{}
	found := false
	for k, v := range a.values {
		if !strings.HasPrefix(k, prefix+".") || isEmpty(v) {
			continue
		}
		a.claim(k)
		setNested(nested, strings.Split(k[len(prefix)+1:], "."), looseJSON(v))
		found = true
	}
	if !found {
		return nil
	}
	return arrayify(nested)
}

func setNested(root map[string]any, path []string, value any) {
	current := root
	for _, key := range path[:len(path)-1] {
		next, ok := current[key].(map[string]any)
		if !ok {
			// A leaf already sits here; the first value wins rather
			// than being silently replaced by a container.
			if _, taken := current[key]; taken {
				return
			}
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
}

// arrayify turns {"0": …, "1": …} into a slice, recursively.
func arrayify(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for k, item := range m {
		m[k] = arrayify(item)
	}
	out := make([]any, len(m))
	for i := range out {
		item, ok := m[strconv.Itoa(i)]
		if !ok {
			return m
		}
		out[i] = item
	}
	return out
}

// traceID renders a 16-byte OTLP trace id as 32 lower-case hex characters
// (spec 002 #3). An absent or all-zero id is invalid per the OTLP spec.
func traceID(b []byte) string {
	if len(b) != 16 || allZero(b) {
		return ""
	}
	return hex.EncodeToString(b)
}

// spanID renders an 8-byte OTLP span id as 16 lower-case hex characters.
func spanID(b []byte) string {
	if len(b) != 8 || allZero(b) {
		return ""
	}
	return hex.EncodeToString(b)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// dialectOf labels a span by the richest dialect its attributes speak. The run
// link is not a dialect: `tracepad.run_id` is stamped by any harness, in any
// language, over whatever SDK the application already runs (spec 014 #2), so
// it says nothing about who wrote the span (spec 017 #3).
func dialectOf(values map[string]any) string {
	dialect := DialectOTel
	for k := range values {
		if strings.HasPrefix(k, "langfuse.") {
			return DialectLangfuse
		}
		if strings.HasPrefix(k, "tracepad.") && k != tpRunID && k != tpItemID {
			dialect = DialectTracepad
			continue
		}
		if strings.HasPrefix(k, "gen_ai.") && dialect == DialectOTel {
			dialect = DialectGenAI
		}
	}
	return dialect
}

func maxDialect(a, b string) string {
	rank := map[string]int{DialectOTel: 0, DialectGenAI: 1, DialectTracepad: 2, DialectLangfuse: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// NoteUnreadable records ResourceSpans blocks the decoder had to skip, so
// the exporter hears about them in partial_success.
func (r *Result) NoteUnreadable(n int) {
	if n <= 0 {
		return
	}
	r.Unreadable += n
	note := fmt.Sprintf("unreadable resource spans: %d", n)
	if r.SkipReason == "" {
		r.SkipReason = note
		return
	}
	r.SkipReason += "; " + note
}

// summarize renders skip reasons for the OTLP partial_success message, which
// is the only channel an exporter has for learning what we dropped.
func summarize(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, reasons[k]))
	}
	return "skipped spans (" + strings.Join(parts, ", ") + ")"
}
