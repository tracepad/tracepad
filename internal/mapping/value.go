package mapping

import (
	"encoding/json"
	"math"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
)

// attrs is one span's effective attribute set — resource, scope and span
// attributes flattened into plain Go values — plus a record of which keys a
// mapping rule has already claimed. Everything left unclaimed at the end
// lands in the observation's metadata (spec 002 #11), so "reading" an
// attribute through this type is what marks it consumed.
type attrs struct {
	values   map[string]any
	consumed map[string]bool
}

func newAttrs() *attrs {
	return &attrs{values: map[string]any{}, consumed: map[string]bool{}}
}

// merge folds a KeyValue list in. Later calls win, which is how the
// resource < scope < span precedence of spec 002 ("resource attributes are
// merged at lower priority than the span's own") is expressed.
func (a *attrs) merge(kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		if kv == nil || kv.Key == "" {
			continue
		}
		a.values[kv.Key] = anyValue(kv.Value)
	}
}

// get returns the value at key and marks the key consumed. A key present
// with an empty value counts as absent: priority chains are "first non-empty
// wins", and an SDK that stamps an empty string should not shadow the next
// candidate in the chain.
func (a *attrs) get(key string) (any, bool) {
	a.consumed[key] = true
	v, ok := a.values[key]
	if !ok || isEmpty(v) {
		return nil, false
	}
	return v, true
}

// str is get plus stringification: a non-string value is rendered as JSON so
// that a target column typed TEXT never silently loses a structured value.
func (a *attrs) str(key string) (string, bool) {
	v, ok := a.get(key)
	if !ok {
		return "", false
	}
	return asString(v), true
}

// firstStr walks a priority chain and returns the first non-empty value.
// Every key in the chain is marked consumed, including those after the hit:
// they carry the same meaning as the winner, so repeating them in metadata
// would be noise, not preservation.
func (a *attrs) firstStr(keys ...string) (string, bool) {
	var out string
	var found bool
	for _, k := range keys {
		if s, ok := a.str(k); ok && !found {
			out, found = s, true
		}
	}
	return out, found
}

// prefixed returns every attribute under "<prefix>." with the prefix
// stripped, marking each consumed.
func (a *attrs) prefixed(prefix string) map[string]any {
	out := map[string]any{}
	for k, v := range a.values {
		if len(k) <= len(prefix)+1 || k[:len(prefix)+1] != prefix+"." {
			continue
		}
		a.consumed[k] = true
		out[k[len(prefix)+1:]] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// consume marks a key claimed without reading it — for attributes a rule
// handles by other means (a whole prefix, a span field) but that must not
// reappear in metadata.
func (a *attrs) consume(key string) { a.consumed[key] = true }

// rest returns every attribute no rule claimed (spec 002 #11).
func (a *attrs) rest() map[string]any {
	out := map[string]any{}
	for k, v := range a.values {
		if a.consumed[k] || isEmpty(v) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// has reports presence of a non-empty value without consuming the key —
// used by heuristics (spec 002 #12) that only need to know a value exists
// while some other rule owns it.
func (a *attrs) has(key string) bool {
	v, ok := a.values[key]
	return ok && !isEmpty(v)
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
