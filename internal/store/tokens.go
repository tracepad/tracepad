package store

import (
	"database/sql/driver"
	"encoding/json"

	"modernc.org/sqlite"
)

// The counting rule of the five token classes (spec 031 #1, spec 049 #1, #12),
// written once, in Go.
//
// Every reader decodes an observation's `usage` text once and takes all five
// classes from that one decode: the trace's columns in `refreshAggregates`,
// both rollups, the live statistics. The rule used to be spelled out in SQL —
// per key a `json_type` and two `json_extract`s — and at five classes and
// twenty keys that expression was several kilobytes that SQLite parsed again
// for every statement carrying it, once per trace on every ingest batch, and
// some thirty JSON lookups per observation it read; measured, the budgets of
// spec 049 #8 were missed by far (#17). Read as text and decoded here, a row
// costs one decode and no statement carries the rule.

// UsageTokens is the counting rule over one `usage` object: for each class the
// first key of its list that is *present* decides (spec 031 #1) — a JSON null
// included — and it counts only as a number within 0..10^9 (spec 043 #4),
// truncated toward zero. A key that is there but holds no count — a string, an
// object, a number outside the range — leaves its class nil rather than
// reading the next spelling: falling through would make a collision on the
// first spelling silently disappear, and coercing "lots" to zero would be a
// claim nobody made.
func UsageTokens(usage map[string]any) Tokens {
	var t Tokens
	fields := t.fields()
	for i, keys := range tokenClasses {
		for _, key := range keys {
			value, present := usage[key]
			if !present {
				continue
			}
			if count, ok := tokenCount(value); ok {
				*fields[i] = &count
			}
			break
		}
	}
	return t
}

// tokenCount reads one decoded JSON value as a count within the domain. A
// number decodes as a float64, which holds every count in range exactly and
// puts every number outside it outside it still.
func tokenCount(value any) (int64, bool) {
	n, ok := value.(float64)
	if !ok || !(n >= 0 && n <= 1e9) {
		return 0, false
	}
	return int64(n), true
}

// usageTokens is the rule over one stored `usage` text: all five classes from
// one decode. NULL, empty or not a JSON object carries no count.
func usageTokens(text []byte) Tokens {
	if len(text) == 0 {
		return Tokens{}
	}
	var usage map[string]any
	if err := json.Unmarshal(text, &usage); err != nil {
		return Tokens{}
	}
	return UsageTokens(usage)
}

// tokenFunction is the rule as an SQL function, `tracepad_token(usage,
// class)`: the count of class `class` — its index in `tokenClasses` — in one
// `usage` text, or NULL. Migration 0033's backfill calls it, once per class
// per observation at the upgrade, and nothing on a hot path does: a scalar
// function answers one class, so it decodes the text once per call. It stays
// registered for as long as that migration can run: for good.
const tokenFunction = "tracepad_token"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(tokenFunction, 2, tokenSQL)
}

func tokenSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var text []byte
	switch v := args[0].(type) {
	case string:
		text = []byte(v)
	case []byte:
		text = v
	default:
		return nil, nil
	}
	class, ok := args[1].(int64)
	if !ok || class < 0 || class >= int64(len(tokenClasses)) {
		return nil, nil
	}
	tokens := usageTokens(text)
	if count := *tokens.fields()[class]; count != nil {
		return *count, nil
	}
	return nil, nil
}
