package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// A payload cut by the budget that holds media must say so (spec 004 #39): the
// reference after the cut is not lost, and the marker names it. One found
// upgrading a live install: the image was in the stored input, past the
// preview, and read as dropped.
func TestAMarkerNamesTheMediaTheCutLeavesOut(t *testing.T) {
	sha := func(n int) string { return fmt.Sprintf("%064x", n) }
	ref := func(n int) map[string]any {
		return map[string]any{"tracepad_media": sha(n), "mime_type": "image/png", "size": float64(1000 * n)}
	}
	text := map[string]any{"type": "text", "text": strings.Repeat("a long prompt ", 2000)}
	budget := payloadBudget{share: 2048, affordable: true}

	marker := func(t *testing.T, value any) truncation {
		t.Helper()
		got, ok := budget.render(value, "t", "o").(truncation)
		if !ok {
			t.Fatalf("render did not cut a payload of %d bytes to a share of %d", len(mustJSON(t, value)), budget.share)
		}
		if size := markerSize(got); size > budget.share {
			t.Errorf("the marker is %d bytes, over its share of %d", size, budget.share)
		}
		return got
	}

	t.Run("a reference after the cut", func(t *testing.T) {
		got := marker(t, []any{text, ref(1)})
		if got.MediaCount != 1 || len(got.Media) != 1 || got.Media[0]["tracepad_media"] != sha(1) {
			t.Errorf("media = %d %v, want the one reference past the preview", got.MediaCount, got.Media)
		}
		if got.Media[0]["mime_type"] != "image/png" || fmt.Sprint(got.Media[0]["size"]) != "1000" {
			t.Errorf("the reference was not carried as stored: %v", got.Media[0])
		}
		if got.Preview == "" {
			t.Error("naming the media took the preview away")
		}
	})

	t.Run("a reference the preview holds is not repeated", func(t *testing.T) {
		got := marker(t, []any{ref(1), text})
		if !strings.Contains(got.Preview, sha(1)) {
			t.Fatalf("the preview does not hold the reference it was meant to: %.200s", got.Preview)
		}
		if got.MediaCount != 0 || got.Media != nil {
			t.Errorf("media = %d %v, want none: the preview shows it", got.MediaCount, got.Media)
		}
		if hasMediaFields(t, got) {
			t.Errorf("the marker mentions media nobody left out: %s", mustJSON(t, got))
		}
	})

	t.Run("no media, no fields", func(t *testing.T) {
		got := marker(t, []any{text})
		if hasMediaFields(t, got) {
			t.Errorf("a payload with no media has media fields: %.300s", mustJSON(t, got))
		}
	})

	t.Run("a body sent twice is one reference, a placeholder another", func(t *testing.T) {
		placeholder := map[string]any{"tracepad_media": sha(1), "mime_type": "image/png", "size": 1000.0, "stored": false}
		got := marker(t, []any{text, ref(1), ref(1), placeholder})
		if got.MediaCount != 2 {
			t.Errorf("media_count = %d, want 2", got.MediaCount)
		}
	})

	t.Run("a list is bounded and the count still says how many", func(t *testing.T) {
		many := []any{text}
		for n := 1; n <= 40; n++ {
			many = append(many, ref(n))
		}
		got := marker(t, many)
		if got.MediaCount != 40 {
			t.Errorf("media_count = %d, want all 40", got.MediaCount)
		}
		if len(got.Media) == 0 || len(got.Media) > maxListedMedia {
			t.Errorf("listed %d references, want between 1 and %d", len(got.Media), maxListedMedia)
		}
	})

	// Review of #202: references the preview holds are not listed, and the ones
	// past it are, whatever the list's own room costs the preview.
	t.Run("what the preview holds is not listed and what it does not is", func(t *testing.T) {
		many := []any{}
		for n := 1; n <= 40; n++ {
			many = append(many, ref(n))
		}
		many = append(many, text)
		big := payloadBudget{share: 3000, affordable: true}
		got, _ := big.render(many, "t", "o").(truncation)
		var inPreview int
		for n := 1; n <= 40; n++ {
			if strings.Contains(got.Preview, sha(n)) {
				inPreview++
			}
		}
		if inPreview == 0 || inPreview == 40 {
			t.Fatalf("%d of 40 references are in the preview, want some and not all", inPreview)
		}
		if got.MediaCount != 40-inPreview || len(got.Media) != min(got.MediaCount, maxListedMedia) {
			t.Errorf("media_count = %d, listed %d, want the %d the preview does not hold, the first %d listed",
				got.MediaCount, len(got.Media), 40-inPreview, maxListedMedia)
		}
		for _, listed := range got.Media {
			if strings.Contains(got.Preview, listed["tracepad_media"].(string)) {
				t.Errorf("%v is listed and in the preview", listed["tracepad_media"])
			}
		}
		if size := markerSize(got); size > big.share {
			t.Errorf("the marker is %d bytes, over its share of %d", size, big.share)
		}
	})

	// Over every size of payload and of share, with text and references
	// alternating and a reference that carries a key after `tracepad_media`:
	// the list starts with the first reference the preview does not hold whole,
	// keeps document order, is bounded, the count is every one past the
	// preview, the preview keeps its minimum, and the marker stays in its share.
	t.Run("the list is what the preview does not hold, at every size", func(t *testing.T) {
		line := func(n int) any {
			return map[string]any{"type": "text", "text": fmt.Sprintf("turn %d: ", n) + strings.Repeat("words ", 40)}
		}
		for refs := 5; refs <= 40; refs += 5 {
			for share := 1500; share <= 4500; share += 250 {
				many := []any{}
				for n := 1; n <= refs; n++ {
					r := ref(n)
					if n%3 == 0 {
						r["zz_after"] = "a key past tracepad_media"
					}
					many = append(many, line(n), r)
				}
				got, ok := payloadBudget{share: share, affordable: true}.render(many, "t", "o").(truncation)
				if !ok {
					continue // it fits whole: no marker, nothing to say
				}
				var beyond []int
				for n := 1; n <= refs; n++ {
					if !strings.Contains(got.Preview, sha(n)+`","zz_after"`) && !strings.Contains(got.Preview, sha(n)+`"}`) {
						beyond = append(beyond, n)
					}
				}
				name := fmt.Sprintf("refs=%d share=%d", refs, share)
				if got.MediaCount != len(beyond) {
					t.Errorf("%s: media_count = %d, want the %d past the preview", name, got.MediaCount, len(beyond))
				}
				if want := min(len(beyond), maxListedMedia); len(got.Media) > want || (share >= 3000 && len(got.Media) != want) {
					t.Errorf("%s: listed %d, want %d", name, len(got.Media), want)
				}
				for i, listed := range got.Media {
					if listed["tracepad_media"] != sha(beyond[i]) {
						t.Errorf("%s: entry %d is %v, want the %dth reference in document order, starting with the first unshown",
							name, i, listed["tracepad_media"], beyond[i])
						break
					}
				}
				// The room kept is minPreview of *encoded* text, in which each
				// quote of the prefix costs two bytes.
				if len(got.Preview) < minPreview*3/4 {
					t.Errorf("%s: the preview is %d bytes, the list took its minimum", name, len(got.Preview))
				}
				if size := markerSize(got); size > share {
					t.Errorf("%s: the marker is %d bytes", name, size)
				}
			}
		}
	})

	// A hash a prompt quotes is text, not the reference: the reference after
	// the cut is still listed.
	t.Run("a hash quoted in the text does not make the reference seen", func(t *testing.T) {
		quoting := map[string]any{"type": "text", "text": "the file is " + sha(1) + ". " + strings.Repeat("padding ", 2000)}
		got := marker(t, []any{quoting, ref(1)})
		if got.MediaCount != 1 || len(got.Media) != 1 {
			t.Errorf("media = %d %v, want the reference past the preview despite the quoted hash", got.MediaCount, got.Media)
		}
	})

	t.Run("things that are not references are not counted", func(t *testing.T) {
		got := marker(t, []any{text,
			map[string]any{"tracepad_media": "not-a-hash", "mime_type": "image/png"},
			map[string]any{"tracepad_media": sha(1)},
			map[string]any{"note": sha(2)}})
		if got.MediaCount != 0 {
			t.Errorf("media_count = %d, want 0 for a hash with no type, a type with no hash and a bare string", got.MediaCount)
		}
	})
}

// hasMediaFields says whether the marker, as written, carries either field.
func hasMediaFields(t *testing.T, marker truncation) bool {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, marker), &fields); err != nil {
		t.Fatal(err)
	}
	_, count := fields["media_count"]
	_, list := fields["media"]
	return count || list
}

// Through the read API: the stored input is a long prompt and then an image,
// and `?expand=io` cuts it. The answer says the image is past the preview.
func TestExpandIOSaysWhatMediaTheCutLeftOut(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	sha := strings.Repeat("ab", 32)
	input := []any{
		map[string]any{"type": "text", "text": strings.Repeat("a long prompt ", 20_000)},
		map[string]any{"tracepad_media": sha, "mime_type": "image/png", "size": 48210},
	}
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms, Input: input})

	rec := h.get(t, "/api/v1/traces/"+traceHex(1)+"?expand=io")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Observations []struct {
			Input truncation `json:"input"`
		} `json:"observations"`
	}](t, rec)
	marker := body.Observations[0].Input
	if !marker.Truncated || marker.MediaCount != 1 || len(marker.Media) != 1 || marker.Media[0]["tracepad_media"] != sha {
		t.Errorf("marker = %+v, want it to name the image the preview does not reach", marker)
	}
	if fmt.Sprint(marker.Media[0]["size"]) != "48210" || marker.Media[0]["mime_type"] != "image/png" {
		t.Errorf("the reference was not carried as it is stored: %v", marker.Media[0])
	}
}
