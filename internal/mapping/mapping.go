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
	"sort"
	"strconv"
	"strings"

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
	DialectGenAI    = "genai"
	DialectOTel     = "otel"
)

// spanCtx is a span together with the attribute scopes above it.
type spanCtx struct {
	span     *tracepb.Span
	resource []*commonpb.KeyValue
	scope    []*commonpb.KeyValue
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
		res.Traces = append(res.Traces, traces[id].finish())
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
					span:     span,
					resource: rs.GetResource().GetAttributes(),
					scope:    ss.GetScope().GetAttributes(),
				})
			}
		}
	}
	return out
}

// traceFields are the trace-level values one span contributed.
type traceFields struct {
	name        string
	rootName    string
	userID      string
	sessionID   string
	environment string
	tags        []string
	metadata    map[string]any
	dialect     string
}

// traceAccumulator merges per-span contributions field-wise: a non-empty
// value beats an empty one and a later non-empty value overwrites an earlier
// one (spec 002 #6).
type traceAccumulator struct {
	trace    *model.Trace
	rootName string
}

func (a *traceAccumulator) apply(tf *traceFields) {
	setIf(&a.trace.Name, tf.name)
	setIf(&a.rootName, tf.rootName)
	setIf(&a.trace.UserID, tf.userID)
	setIf(&a.trace.SessionID, tf.sessionID)
	setIf(&a.trace.Environment, tf.environment)
	if len(tf.tags) > 0 {
		a.trace.Tags = tf.tags
	}
	if len(tf.metadata) > 0 {
		a.trace.Metadata = tf.metadata
	}
}

func (a *traceAccumulator) finish() *model.Trace {
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
	a.merge(sc.resource)
	a.merge(sc.scope)
	a.merge(span.GetAttributes())

	tf := &traceFields{dialect: dialectOf(a.values)}
	tf.name, _ = a.firstString(traceNameKeys...)
	tf.userID, _ = a.firstString(traceUserKeys...)
	tf.sessionID, _ = a.firstString(traceSessionKeys...)
	tf.environment, _ = a.firstString(traceEnvironmentKeys...)
	tf.tags = mapTags(a)
	tf.metadata = mapMetadata(a, lfTraceMetadata)
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

	obs.Level = mapLevel(a, span.GetStatus())
	obs.StatusMessage = mapStatusMessage(a, span.GetStatus())
	obs.Model, _ = a.firstString(obsModelKeys...)
	obs.ModelParameters = mapModelParameters(a)
	obs.Usage = mapUsage(a)
	obs.CostDetails = mapCost(a)
	obs.Input = mapPayload(a, obsInputKeys, genAIPromptPrefix)
	obs.Output = mapPayload(a, obsOutputKeys, genAICompletion)

	typeMetadata := map[string]any{}
	obs.Type = mapType(a, obs, parents[id], typeMetadata)
	obs.Metadata = mergeMetadata(mapMetadata(a, lfObsMetadata), typeMetadata, a.rest())

	return obs, tf, ""
}

// mapLevel: an explicit level wins; otherwise an ERROR span status raises
// the level, and everything else is DEFAULT. A spelling the alias table does
// not know is left unclaimed, so it surfaces in metadata rather than being
// swallowed by the fallback.
func mapLevel(a *attrs, status *tracepb.Status) string {
	if key, raw, ok := a.first(obsLevelKeys...); ok {
		if level, known := levelAliases[strings.ToUpper(strings.TrimSpace(asString(raw)))]; known {
			a.claim(key)
			return level
		}
	}
	if status.GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		return model.LevelError
	}
	return model.LevelDefault
}

func mapStatusMessage(a *attrs, status *tracepb.Status) string {
	if msg, ok := a.firstString(obsStatusMessageKeys...); ok {
		return msg
	}
	return status.GetMessage()
}

// mapType implements spec 002 #12. An explicit Langfuse type wins; a model
// attribute in any dialect means a generation; a zero-duration childless span
// is an event; everything else is a span. A Langfuse type richer than our
// three collapses onto the nearest one and keeps its original spelling in
// metadata, so nothing is lost.
func mapType(a *attrs, obs *model.Observation, hasChildren bool, meta map[string]any) string {
	if raw, ok := a.lookup(lfObsType); ok {
		spelling := strings.ToLower(strings.TrimSpace(asString(raw)))
		mapped, known := observationTypeAliases[spelling]
		a.claim(lfObsType)
		if known {
			if mapped != spelling {
				meta[lfObsType] = raw
			}
			return mapped
		}
		// An unknown spelling is preserved and left to the heuristics.
		meta[lfObsType] = raw
	}
	if obs.Model != "" {
		return model.TypeGeneration
	}
	if obs.StartTime == obs.EndTime && !hasChildren {
		return model.TypeEvent
	}
	return model.TypeSpan
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
// price, not a token count, and feeds the cost chain instead).
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
	if len(out) == 0 {
		return nil
	}
	return out
}

// mapCost returns the client-provided cost, normalized so that a `total` is
// always present (spec 002 #14: cost is never estimated, only recorded).
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
	if _, ok := out["total"]; !ok {
		// The trace list sums one number per observation; deriving it
		// once here beats teaching every reader the component names.
		var total float64
		var hasComponent bool
		for _, v := range out {
			if n, ok := asNumber(v); ok {
				total += n
				hasComponent = true
			}
		}
		if hasComponent {
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

// mapMetadata collects a metadata prefix in both shapes SDKs use: a JSON
// object at the bare key, and one attribute per entry underneath it.
func mapMetadata(a *attrs, prefix string) map[string]any {
	out := map[string]any{}
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

// dialectOf labels a span by the richest dialect its attributes speak.
func dialectOf(values map[string]any) string {
	dialect := DialectOTel
	for k := range values {
		if strings.HasPrefix(k, "langfuse.") {
			return DialectLangfuse
		}
		if strings.HasPrefix(k, "gen_ai.") {
			dialect = DialectGenAI
		}
	}
	return dialect
}

func maxDialect(a, b string) string {
	rank := map[string]int{DialectOTel: 0, DialectGenAI: 1, DialectLangfuse: 2}
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
