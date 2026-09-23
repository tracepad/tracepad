package store

import "testing"

// sameJSON and canonicalNumber in isolation (spec 014 #32): what counts as
// the same JSON value, and what does not.

func TestSameJSON(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{`"Ω"`, `"\u03a9"`, true},
		{`"\u00e9"`, `"é"`, true},
		{`"<&>"`, `"\u003c\u0026\u003e"`, true},
		{`"\/"`, `"/"`, true},
		{`[1,2]`, `[2,1]`, false},
		{`{"a":1}`, `{"a":1,"b":null}`, false},
		{`{"a":null}`, `{}`, false},
		{`1`, `"1"`, false},
		{`true`, `1`, false},
		{`null`, `false`, false},
		{`{"a":[{"x":1.0}]}`, `{"a":[{"x":1}]}`, true},
		{`"a"`, `"A"`, false},
		{`not json`, `"not json"`, false},
		// U+FFFD is what the decoder writes for a lone surrogate and an
		// invalid byte alike, so a string holding it is compared as bytes.
		{`"\ud800"`, `"\udc00"`, false},
		{`"\ud800"`, `"\ufffd"`, false},
		{"\"\xff\"", "\"\xfe\"", false},
		{`{"\ud800":1}`, `{"\udc00":1}`, false},
		// A repeated key could hide an edit to the one a decoder drops.
		{`{"a":1,"a":2}`, `{"a":2}`, false},
		// Anything after the value is not ignored.
		{`[1] x`, `[1]`, false},
		{`[1][2]`, `[1]`, false},
	}
	for _, c := range cases {
		if got := sameJSON([]byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("sameJSON(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := sameJSON([]byte(c.b), []byte(c.a)); got != c.want {
			t.Errorf("sameJSON(%s, %s) = %v, want %v", c.b, c.a, got, c.want)
		}
	}
	if !sameJSON(nil, nil) {
		t.Error("two absent bodies differ")
	}
	if sameJSON(nil, []byte(`null`)) || sameJSON([]byte(`{}`), nil) {
		t.Error("an absent body equals a present one")
	}
	// Byte-equal still settles it, whatever the body holds.
	if !sameJSON([]byte(`{"a":1,"a":2}`), []byte(`{"a":1,"a":2}`)) || !sameJSON([]byte(`"\ud800"`), []byte(`"\ud800"`)) {
		t.Error("byte-equal bodies differ")
	}
}

func TestCanonicalNumber(t *testing.T) {
	equal := [][]string{
		{"1", "1.0", "1.000", "1e0", "1E+0", "10e-1", "0.1e1", "100e-2"},
		{"0", "-0", "0.0", "-0.0", "0e10", "0E-5"},
		{"-12.5", "-125e-1", "-1.25E1", "-0.125e2"},
		{"1200", "1.2e3", "12e2", "1200.00"},
		{"12345678901234567890123", "1.2345678901234567890123e22"},
		{"0.001", "1e-3", "10E-4", "1e-000000000000003"},
		{"10", "1e+01", "1e0000000000000001", "1E+0000000000000000000001"},
	}
	for _, group := range equal {
		want := canonicalNumber(group[0])
		for _, spelling := range group[1:] {
			if got := canonicalNumber(spelling); got != want {
				t.Errorf("canonicalNumber(%s) = %s, want %s like %s", spelling, got, want, group[0])
			}
		}
	}
	different := [][2]string{
		{"1", "-1"},
		{"1", "10"},
		{"1.5", "15"},
		{"12345678901234567890123", "12345678901234567890124"},
		{"1e-3", "1e3"},
	}
	for _, pair := range different {
		if canonicalNumber(pair[0]) == canonicalNumber(pair[1]) {
			t.Errorf("canonicalNumber says %s == %s", pair[0], pair[1])
		}
	}
	// An exponent with too many significant digits for an int is compared
	// as written, not parsed into an arithmetic of its own.
	huge := "1e1234567890123456789"
	if got := canonicalNumber(huge); got != huge {
		t.Errorf("canonicalNumber(%s) = %s, want it as written", huge, got)
	}
}
