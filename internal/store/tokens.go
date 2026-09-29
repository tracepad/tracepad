package store

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// The counting rule of the five token classes (spec 031 #1, spec 049 #1, #12),
// written once, in Go, and handed to SQL as a function.
//
// It used to be spelled out in SQL: per key a `json_type` and two
// `json_extract`s, for every key of every class. At five classes and twenty
// keys the expression was several kilobytes, which SQLite parsed again for
// every statement that carried it — once per trace on every ingest batch —
// and evaluated as some thirty JSON lookups per observation. Measured, that
// put the ingest batch 8–11 % and the whole-hour roll 42 % over what they
// cost before (spec 049 #17). As a function the rule is one call per class,
// the `usage` text is decoded once per row, and the Go and SQL readings of
// the rule cannot drift apart, because there is only one.

// tokenFunction is the rule's name in SQL: `tracepad_token(usage, class)` is
// the count of class `class` — its index in `tokenClasses` — in one
// observation's `usage` text, or NULL when it carries none. Migration 0033
// calls it, so it is registered for as long as that migration can run: for
// good.
const tokenFunction = "tracepad_token"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(tokenFunction, 2, tokenSQL)
}

// tokenSQL is the function's body. A `usage` that is NULL, not text, or not a
// JSON object carries no count; so does a class index out of range, which
// only a typo in this package could produce.
func tokenSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var usage string
	switch v := args[0].(type) {
	case string:
		usage = v
	case []byte:
		usage = string(v)
	default:
		return nil, nil
	}
	class, ok := args[1].(int64)
	if !ok || class < 0 || class >= int64(len(tokenClasses)) {
		return nil, nil
	}
	tokens := lastUsage.tokens(usage)
	if count := *tokens.fields()[class]; count != nil {
		return *count, nil
	}
	return nil, nil
}

// usageMemo remembers the last `usage` text decoded and what it counted. A row
// asks for its five classes one after another, so the four after the first are
// a string comparison rather than a decode. The mutex is uncontended in the
// ordinary case — one writer — and a read that interleaves with it costs a
// decode, never a wrong answer: the text is the key.
type usageMemo struct {
	mu     sync.Mutex
	text   string
	counts Tokens
}

var lastUsage usageMemo

func (m *usageMemo) tokens(text string) Tokens {
	m.mu.Lock()
	defer m.mu.Unlock()
	if text == m.text && text != "" {
		return m.counts
	}
	var usage map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&usage); err != nil {
		usage = nil
	}
	m.text, m.counts = text, UsageTokens(usage)
	return m.counts
}

// UsageTokens is the counting rule over one `usage` object: for each class the
// first key of its list that is *present* decides (spec 031 #1) — a JSON null
// included — and it counts only as a number within 0..10^9 (spec 043 #4),
// truncated toward zero. A key that is there but holds no count — a string, an
// object, a number outside the range — leaves its class nil rather than
// reading the next spelling: falling through would make a collision on the
// first spelling silently disappear, and coercing "lots" to zero would be a
// claim nobody made.
//
// A number arrives as a float64 from a plain decode (the CLI's tree) or as a
// json.Number from the SQL function's, which keeps 1e300 from rounding into
// range and a large integer exact.
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

// tokenCount reads one value as a count within the domain.
func tokenCount(value any) (int64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		n = f
	default:
		return 0, false
	}
	if !(n >= 0 && n <= 1e9) {
		return 0, false
	}
	return int64(n), true
}

// tokenExprs is the five counts of one observation's `usage` as SQL
// expressions, in the order of `tokenClasses`, given the column expression
// `usage` holds it under.
func tokenExprs(usage string) [len(tokenClasses)]string {
	var out [len(tokenClasses)]string
	for i := range out {
		out[i] = tokenFunction + "(" + usage + ", " + string(rune('0'+i)) + ")"
	}
	return out
}
