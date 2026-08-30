package mapping

import (
	"encoding/json"
	"math"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// origin is the level an attribute arrived at. The mapping chains never see
// it — `deployment.environment` on the Resource sets the environment exactly
// as it would on the span — but what no rule claims keeps it, as the prefix
// it lands in metadata under (spec 012 #7).
type origin int

// The three levels of an OTLP export, in the order they are merged.
const (
	originResource origin = iota
	originScope
	originSpan
)

// levels lists them in merge order, which is also the order rest() writes
// them: the span level goes last, so an attribute literally named
// `resource.x` on the span beats a resource attribute named `x`.
var levels = []origin{originResource, originScope, originSpan}

// prefix is what an unclaimed attribute of this origin is filed under in
// metadata. Span attributes keep their bare names — that is what every
// metadata key looked like before spec 012, and the span is the level a
// reader means when they say "this attribute".
//
// The spelling is ours, not the reference platform's `resourceAttributes.`:
// this is a metadata contract of our own (spec 012 #7).
func (o origin) prefix() string {
	switch o {
	case originResource:
		return "resource."
	case originScope:
		return "scope."
	}
	return ""
}

// attrs is one span's effective attribute set — resource, scope and span
// attributes flattened into plain Go values — plus a record of which keys a
// mapping rule has already claimed. Everything left unclaimed at the end
// lands in the observation's metadata (spec 002 #11), so "reading" an
// attribute through this type is what marks it consumed.
//
// The flattening is kept beside the three levels it came from rather than
// instead of them: merged, a resource `service.name` and a span attribute of
// the same name are indistinguishable and whichever came last is the only one
// left, which breaks the promise that nothing is lost (spec 012 #7). Chains
// read the merged view; what falls through falls through per level.
type attrs struct {
	values   map[string]any
	byLevel  map[origin]map[string]any
	consumed map[string]bool
}

func newAttrs() *attrs {
	return &attrs{
		values: map[string]any{},
		byLevel: map[origin]map[string]any{
			originResource: {}, originScope: {}, originSpan: {},
		},
		consumed: map[string]bool{},
	}
}

// merge folds a KeyValue list in at one level. Later calls win in the merged
// view, which is how the resource < scope < span precedence of spec 002
// ("resource attributes are merged at lower priority than the span's own") is
// expressed.
func (a *attrs) merge(level origin, kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		if kv == nil || kv.Key == "" {
			continue
		}
		value := anyValue(kv.Value)
		a.values[kv.Key] = value
		a.byLevel[level][kv.Key] = value
	}
}

// Reading and claiming are deliberately separate operations. A key is
// claimed only once a rule has actually *used* its value, never merely
// because a rule looked at it — otherwise an attribute a rule inspected and
// then rejected (an unknown level spelling, a cost_details that is not JSON,
// the loser of a priority chain) would be consumed without being stored, and
// would appear nowhere at all. Spec 002 #11 promises the opposite.

// keyLevel binds an attribute name to the one level it means something at.
// Most names mean the same thing wherever they arrive, and a rule reading the
// merged view is right not to care — but `service.version` is not one of
// them. On the Resource it is the version of the service that produced the
// trace, which is what the release chain falls back to (spec 012 #4, "then
// the resource's `service.version`"); on a span it describes whatever that
// span talked to, and reading it there is wrong twice over, because claiming
// it would take the Resource's own value out of metadata as well (found in
// review of PR #19).
var keyLevel = map[string]origin{"service.version": originResource}

// lookup returns the value at key without claiming it. A key present with an
// empty value counts as absent: priority chains are "first non-empty wins",
// and an SDK that stamps an empty string should not shadow the next
// candidate.
func (a *attrs) lookup(key string) (any, bool) {
	v, ok := a.values[key]
	if level, bound := keyLevel[key]; bound {
		v, ok = a.byLevel[level][key]
	}
	if !ok || isEmpty(v) {
		return nil, false
	}
	return v, true
}

// first walks a priority chain and returns the winning key and its value,
// claiming nothing. Losers stay unclaimed on purpose: `gen_ai.response.model`
// is not the same fact as `gen_ai.request.model`, so the one that did not win
// still belongs in metadata.
func (a *attrs) first(keys ...string) (string, any, bool) {
	for _, k := range keys {
		if v, ok := a.lookup(k); ok {
			return k, v, true
		}
	}
	return "", nil, false
}

// firstString resolves a chain into a string column, claiming the winner.
// A non-string value is rendered as JSON so a TEXT column never silently
// loses a structured value.
func (a *attrs) firstString(keys ...string) (string, bool) {
	key, v, ok := a.first(keys...)
	if !ok {
		return "", false
	}
	a.claim(key)
	return asString(v), true
}

// firstRanked is firstString for a trace-level chain: it also reports how far
// down the chain the winner was found, which is what keeps the chain's
// priority alive when the spans of one export disagree (spec 012 #11).
func (a *attrs) firstRanked(keys ...string) rankedValue {
	for rank, key := range keys {
		if v, ok := a.lookup(key); ok {
			a.claim(key)
			return rankedValue{value: asString(v), rank: rank}
		}
	}
	return rankedValue{}
}

// prefixed returns every attribute under "<prefix>." with the prefix
// stripped, claiming each — they are all carried into the result.
func (a *attrs) prefixed(prefix string) map[string]any {
	out := map[string]any{}
	for k, v := range a.values {
		if len(k) <= len(prefix)+1 || k[:len(prefix)+1] != prefix+"." {
			continue
		}
		a.claim(k)
		out[k[len(prefix)+1:]] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// claim marks a key as carried into the result, so it does not reappear in
// metadata. It claims the key at every level: a rule reads the merged view,
// so it cannot say which level answered it, and a rule that wants a resource
// key should not have to know it is one (spec 012 #7). Where a key *is* bound
// to a level, claimedAt narrows that back down.
func (a *attrs) claim(key string) { a.consumed[key] = true }

// claimedAt reports whether this level's value is the one a rule took. For an
// ordinary key that is every level, since the rule read the merged view. For
// a key bound to a level (see keyLevel), only that level could have answered,
// so the same name at another level is a different fact and keeps its place
// in metadata — which is the whole promise of spec 012 #7.
func (a *attrs) claimedAt(level origin, key string) bool {
	if !a.consumed[key] {
		return false
	}
	bound, isBound := keyLevel[key]
	return !isBound || bound == level
}

// rest returns every attribute no rule claimed, each under the key its origin
// gives it (spec 002 #11, spec 012 #7). The same name at two levels yields two
// entries, which is the point: `resource.service.name` and `service.name` are
// different facts and used to be one.
func (a *attrs) rest() map[string]any {
	out := map[string]any{}
	for _, level := range levels {
		prefix := level.prefix()
		for k, v := range a.byLevel[level] {
			if a.claimedAt(level, k) || isEmpty(v) {
				continue
			}
			out[prefix+k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anyValue converts an OTLP AnyValue into a plain Go value. Kvlists become
// maps and arrays become slices rather than being stringified, so a
// structured attribute survives into metadata as structure.
func anyValue(v *commonpb.AnyValue) any {
	if v == nil {
		return nil
	}
	switch value := v.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return value.StringValue
	case *commonpb.AnyValue_BoolValue:
		return value.BoolValue
	case *commonpb.AnyValue_IntValue:
		return value.IntValue
	case *commonpb.AnyValue_DoubleValue:
		// NaN and the infinities are not representable in JSON and would
		// fail every downstream encoder; keep them as their textual form.
		if math.IsNaN(value.DoubleValue) || math.IsInf(value.DoubleValue, 0) {
			return strconv.FormatFloat(value.DoubleValue, 'g', -1, 64)
		}
		return value.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return string(value.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		if value.ArrayValue == nil {
			return nil
		}
		out := make([]any, 0, len(value.ArrayValue.Values))
		for _, item := range value.ArrayValue.Values {
			out = append(out, anyValue(item))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		if value.KvlistValue == nil {
			return nil
		}
		out := make(map[string]any, len(value.KvlistValue.Values))
		for _, kv := range value.KvlistValue.Values {
			if kv != nil {
				out[kv.Key] = anyValue(kv.Value)
			}
		}
		return out
	}
	return nil
}

// isEmpty reports whether a value carries no information. Zero numbers and
// false are information; empty strings and empty containers are not.
func isEmpty(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

// asString renders an attribute value for a TEXT column: strings pass
// through, everything else becomes JSON.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// asNumber coerces an attribute value to a float. SDKs disagree on whether
// token counts travel as ints, doubles or decimal strings, so all three are
// accepted.
func asNumber(v any) (float64, bool) {
	switch value := v.(type) {
	case int64:
		return float64(value), true
	case float64:
		return value, true
	case string:
		n, err := strconv.ParseFloat(value, 64)
		return n, err == nil
	}
	return 0, false
}

// asInteger coerces an attribute value to a whole number for an INTEGER
// column. It accepts the three shapes asNumber does and refuses anything with
// a fractional part: a prompt version of 7.5 is not a version, and coercing
// it would file the observation under a prompt release that never existed
// (spec 012 #5).
func asInteger(v any) (int64, bool) {
	n, ok := asNumber(v)
	if !ok || n != math.Trunc(n) || math.Abs(n) >= 1<<53 {
		return 0, false
	}
	return int64(n), true
}

// jsonNumber returns a number as an int64 when it is integral, so that token
// counts render as 42 rather than 42.0 in stored JSON.
func jsonNumber(f float64) any {
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f)
	}
	return f
}

// parseJSONObject parses an attribute that is documented to carry a JSON
// object. A value that is already a map (a kvlist attribute) is taken as is;
// anything that does not parse into an object yields false, and the caller
// falls through to the next rule in the chain.
func parseJSONObject(v any) (map[string]any, bool) {
	switch value := v.(type) {
	case map[string]any:
		return value, len(value) > 0
	case string:
		var out map[string]any
		if err := json.Unmarshal([]byte(value), &out); err != nil || len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return nil, false
}
