package store

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// sameJSON reports whether two stored bodies say the same thing as JSON
// values (spec 014 #6, #32): object keys in any order, a string spelled with
// `\uXXXX` escapes or as raw UTF-8, a number written `1`, `1.0` or `1e0`.
// Duplicate keys resolve the way a decoder resolves them, last one wins.
//
// Byte-equal bodies — the harness re-declaring its cases with the same
// client — are settled without decoding; only a body whose bytes differ pays
// for parsing both sides. A nil body is "none was sent" and equals only
// another nil. A body that does not parse is compared by its bytes alone,
// which the fast path already did.
func sameJSON(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	left, err := decodeJSONValue(a)
	if err != nil {
		return false
	}
	right, err := decodeJSONValue(b)
	if err != nil {
		return false
	}
	return equalJSONValue(left, right)
}

// decodeJSONValue parses one value with its numbers kept as written, so no
// precision is lost on the way to the comparison.
func decodeJSONValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func equalJSONValue(a, b any) bool {
	switch a := a.(type) {
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, left := range a {
			right, ok := b[key]
			if !ok || !equalJSONValue(left, right) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !equalJSONValue(a[i], b[i]) {
				return false
			}
		}
		return true
	case json.Number:
		b, ok := b.(json.Number)
		return ok && canonicalNumber(string(a)) == canonicalNumber(string(b))
	default:
		// string, bool and nil compare as Go values: the decoder has
		// already undone every escape a string was sent with.
		return a == b
	}
}

// canonicalNumber rewrites a JSON number as its significant digits and a
// power of ten, `[-]digits e exp` with no leading or trailing zeros, so two
// spellings of one decimal value come out the same. It works on the text:
// no float rounding, and no big-number arithmetic an exponent of a million
// digits could turn into a cost. Zero is "0" whatever its sign. A number whose
// exponent does not fit an int is returned as written — equal only to
// itself, which is the old byte comparison.
func canonicalNumber(number string) string {
	text := number
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")

	exponent := 0
	if at := strings.IndexAny(text, "eE"); at >= 0 {
		written := strings.TrimPrefix(text[at+1:], "+")
		if len(written) > 15 {
			return number
		}
		parsed, err := strconv.Atoi(written)
		if err != nil {
			return number
		}
		exponent = parsed
		text = text[:at]
	}
	integer, fraction, _ := strings.Cut(text, ".")
	digits := integer + fraction
	exponent -= len(fraction)

	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0"
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += len(digits) - len(trimmed)

	sign := ""
	if negative {
		sign = "-"
	}
	return sign + trimmed + "e" + strconv.Itoa(exponent)
}
