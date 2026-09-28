package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// A JSON body is one value and white space, whatever the value: a closing
// bracket after it is not white space, and `decoder.More()` — which answers
// "is there another element of the array or object being read" — said it was
// the end of one nobody had opened, so `{"input":1}]` and `[{"input":1}] ]`
// passed (spec 043 #36).
func TestDecodeStrictWantsOneValueAndWhiteSpace(t *testing.T) {
	shapes := map[string]string{
		"an object": `{"input":1}`,
		"an array":  `[{"input":1}]`,
	}
	tails := map[string]bool{ // the tail, and whether the body is accepted
		"":              true,
		" ":             true,
		"\n":            true,
		" \r\n\t ":      true,
		"]":             false,
		"}":             false,
		" ]":            false,
		"\n}":           false,
		"]]":            false,
		"0":             false,
		"1 ":            false,
		`"x"`:           false,
		"null":          false,
		"true":          false,
		"{}":            false,
		"[]":            false,
		` {"input":1}`:  false,
		",":             false,
		",{}":           false,
		"\n\n]\n":       false,
		"\xc2\xa0":      false, // a no-break space is not white space to JSON
		"\x00":          false,
		"/* c */":       false,
		"\xef\xbb\xbf":  false,
		"{\"input\":1":  false,
		"\n{\"input\":": false,
	}
	for shape, value := range shapes {
		for tail, accepted := range tails {
			name := fmt.Sprintf("%s then %q", shape, tail)
			body := []byte(value + tail)
			var err error
			if shape == "an object" {
				var one itemRequest
				err = decodeStrict(body, &one)
			} else {
				var many []*itemRequest
				err = decodeStrict(body, &many)
			}
			switch {
			case accepted && err != nil:
				t.Errorf("%s: refused: %v", name, err)
			case !accepted && !errors.Is(err, errTrailingValue):
				t.Errorf("%s: err = %v, want %q", name, err, errTrailingValue)
			}
		}
	}
}

// The same on every route that reads a JSON body, by one of them, and on the
// two that read a batch at each side of the cap — 10,000 and a stray bracket
// was 201, 10,001 and a stray bracket 400 (spec 043 #36).
func TestJSONWritesWantOneValueAndWhiteSpace(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for tail, want := range map[string]int{"": http.StatusOK, "\n": http.StatusOK, "]": http.StatusBadRequest, "}": http.StatusBadRequest, " 0": http.StatusBadRequest} {
		rec := h.call(t, "PUT", "/api/v1/datasets/strict", []byte(`{"description":"d"}`+tail))
		if rec.Code != want {
			t.Errorf("PUT dataset then %q = %d %s, want %d", tail, rec.Code, rec.Body, want)
		}
	}

	for _, route := range batchRoutes() {
		for _, rows := range []int{1, maxItemsPerWrite, maxItemsPerWrite + 1} {
			encoded, err := json.Marshal(route.batch(rows))
			if err != nil {
				t.Fatal(err)
			}
			one := string(encoded)
			for tail, want := range map[string]string{"\n": "", "]": errTrailingValue.Error(), "}": errTrailingValue.Error()} {
				rec := h.call(t, "POST", route.path, []byte(one+tail))
				switch {
				case want == "" && rows <= maxItemsPerWrite && rec.Code != http.StatusCreated:
					t.Errorf("%s: %d rows then %q = %d %s", route.kind, rows, tail, rec.Code, rec.Body)
				case want == "" && rows > maxItemsPerWrite && rec.Code != http.StatusRequestEntityTooLarge:
					t.Errorf("%s: %d rows then %q = %d, want the cap", route.kind, rows, tail, rec.Code)
				case want != "" && (rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want)):
					t.Errorf("%s: %d rows then %q = %d %s, want 400 %q", route.kind, rows, tail, rec.Code, rec.Body, want)
				}
			}
		}
	}
}
