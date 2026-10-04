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
		if got.Media[0]["mime_type"] != "image/png" || got.Media[0]["size"] != float64(1000) {
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
	if marker.Media[0]["size"] != float64(48210) || marker.Media[0]["mime_type"] != "image/png" {
		t.Errorf("the reference was not carried as it is stored: %v", marker.Media[0])
	}
}
