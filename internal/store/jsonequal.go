package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// sameJSON reports whether two stored bodies say the same thing as JSON
// values (spec 014 #6, #32): object keys in any order, a string spelled with
// `\uXXXX` escapes or as raw UTF-8, a number written `1`, `1.0` or `1e0`.
//
// Byte-equal bodies are settled without decoding; only a body whose bytes
// differ from its stored row pays for parsing both sides. A nil body is "none
// was sent" and equals only another nil. Where the decoded value cannot be
// trusted to stand for the text, the bytes decide, which the fast path
// already did: a body that does not parse, carries anything after its value,
// repeats a key (last-wins would hide an edit to the shadowed one), or is not
// lossless (see lossless).
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

// errUntrusted is a body whose decoded value may not say what its text says.
var errUntrusted = errors.New("decoded value does not stand for the text")

// decodeJSONValue parses exactly one value with its numbers kept as written,
// so no precision is lost on the way to the comparison.
func decodeJSONValue(raw []byte) (any, error) {
	if !lossless(raw) {
		return nil, errUntrusted
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeToken(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errUntrusted
	}
	return value, nil
}

func decodeToken(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		if token == '[' {
			array := []any{}
			for decoder.More() {
				element, err := decodeToken(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, element)
			}
			_, err := decoder.Token()
			return array, err
		}
		object := map[string]any{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, _ := key.(string)
			if _, repeated := object[name]; repeated {
				return nil, errUntrusted
			}
			if object[name], err = decodeToken(decoder); err != nil {
				return nil, err
			}
		}
		_, err := decoder.Token()
		return object, err
	default:
		// string, json.Number, bool, nil.
		return token, nil
	}
}

// lossless reports whether decoding raw keeps every string distinct from
// every other: its bytes are valid UTF-8 and every `\uD800`–`\uDFFF` escape
// is half of a pair. The decoder writes U+FFFD for an invalid byte and for a
// lone surrogate alike, so two different strings would otherwise read as one;
// a U+FFFD that was sent as such is an ordinary character. Every backslash in
// a valid body opens an escape inside a string, so the scan needs no string
// state of its own.
func lossless(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		code, ok := escapedUnit(raw, i+1)
		if !ok {
			return false
		}
		i += 4
		switch {
		case code >= 0xDC00 && code <= 0xDFFF:
			return false
		case code >= 0xD800 && code <= 0xDBFF:
			if i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, ok := escapedUnit(raw, i+3)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

// escapedUnit reads the four hex digits of a `\u` escape starting at at.
func escapedUnit(raw []byte, at int) (uint64, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	code, err := strconv.ParseUint(string(raw[at:at+4]), 16, 16)
	return code, err == nil
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
// digits could turn into a cost. Zero is "0" whatever its sign or exponent.
// A number whose exponent has more than 15 significant digits is returned as
// written, lower-cased — equal only to its own spelling, which is the old
// byte comparison.
func canonicalNumber(number string) string {
	text := number
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")

	mantissa, written, scientific := strings.Cut(strings.ToLower(text), "e")
	if strings.Trim(mantissa, "0.") == "" {
		return "0"
	}
	exponent := 0
	if scientific {
		negativeExponent := strings.HasPrefix(written, "-")
		written = strings.TrimLeft(strings.TrimLeft(written, "+-"), "0")
		if len(written) > 15 {
			return strings.ToLower(number)
		}
		if written != "" {
			parsed, err := strconv.Atoi(written)
			if err != nil {
				return strings.ToLower(number)
			}
			exponent = parsed
		}
		if negativeExponent {
			exponent = -exponent
		}
	}
	integer, fraction, _ := strings.Cut(mantissa, ".")
	digits := integer + fraction
	exponent -= len(fraction)

	digits = strings.TrimLeft(digits, "0")
	trimmed := strings.TrimRight(digits, "0")
	exponent += len(digits) - len(trimmed)

	sign := ""
	if negative {
		sign = "-"
	}
	return sign + trimmed + "e" + strconv.Itoa(exponent)
}
