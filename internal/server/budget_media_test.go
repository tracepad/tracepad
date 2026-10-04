package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// A payload cut by the budget that holds media says how many references it
// holds (spec 004 #39): the image is not lost because its reference sits past
// the preview. One found upgrading a live install.
func TestAMarkerSaysHowMuchMediaThePayloadHolds(t *testing.T) {
	sha := func(n int) string { return fmt.Sprintf("%064x", n) }
	ref := func(n int) map[string]any {
		return map[string]any{"tracepad_media": sha(n), "mime_type": "image/png", "size": float64(1000 * n)}
	}
	text := map[string]any{"type": "text", "text": strings.Repeat("a long prompt ", 2000)}

	marker := func(t *testing.T, share int, value any) truncation {
		t.Helper()
		got, ok := payloadBudget{share: share, affordable: true}.render(value, "t", "o").(truncation)
		if !ok {
			t.Fatalf("render did not cut a payload of %d bytes to a share of %d", len(mustJSON(t, value)), share)
		}
		if size := markerSize(got); size > share {
			t.Errorf("the marker is %d bytes, over its share of %d", size, share)
		}
		return got
	}

	t.Run("a reference past the cut is counted, and the preview stays", func(t *testing.T) {
		got := marker(t, 2048, []any{text, ref(1)})
		if got.MediaCount != 1 || got.Preview == "" {
			t.Errorf("media_count = %d, preview %d bytes, want 1 and a preview", got.MediaCount, len(got.Preview))
		}
	})

	t.Run("a body sent twice is one, a placeholder is a reference", func(t *testing.T) {
		placeholder := map[string]any{"tracepad_media": sha(2), "mime_type": "image/png", "size": 1000.0, "stored": false}
		got := marker(t, 2048, []any{text, ref(1), ref(1), placeholder})
		if got.MediaCount != 2 {
			t.Errorf("media_count = %d, want 2", got.MediaCount)
		}
	})

	t.Run("nested references, whatever else they carry", func(t *testing.T) {
		withMore := ref(3)
		withMore["zz_after"] = "a key past tracepad_media"
		got := marker(t, 2048, map[string]any{"messages": []any{text, map[string]any{"image": withMore}, ref(4)}})
		if got.MediaCount != 2 {
			t.Errorf("media_count = %d, want 2", got.MediaCount)
		}
	})

	t.Run("no media, no field", func(t *testing.T) {
		got := marker(t, 2048, []any{text})
		if hasMediaField(t, got) {
			t.Errorf("a payload with no media has media_count: %s", mustJSON(t, got))
		}
		// And a hash quoted in text is text.
		quoting := map[string]any{"text": "the file is " + sha(1) + strings.Repeat(" padding", 2000)}
		if got := marker(t, 2048, []any{quoting}); hasMediaField(t, got) {
			t.Errorf("a quoted hash was counted: %s", mustJSON(t, got))
		}
	})

	t.Run("the count never costs the preview its minimum", func(t *testing.T) {
		many := []any{text}
		for n := 1; n <= 500; n++ {
			many = append(many, ref(n))
		}
		for share := 800; share <= 4000; share += 400 {
			with := marker(t, share, many)
			without := marker(t, share, []any{text})
			if with.MediaCount != 500 {
				t.Errorf("share %d: media_count = %d, want 500", share, with.MediaCount)
			}
			// The preview gives up the bytes of the field and no more.
			if lost := len(without.Preview) - len(with.Preview); lost < 0 || lost > len(`,"media_count":500`)+8 {
				t.Errorf("share %d: the count took %d bytes of the preview", share, lost)
			}
		}
	})

	t.Run("a share too small for the count carries none", func(t *testing.T) {
		value := []any{text, ref(1)}
		traceID, observationID := strings.Repeat("f", 32), strings.Repeat("f", 16)
		bare := markerSize(truncation{Truncated: true, Size: len(mustJSON(t, value)), TraceID: traceID,
			ObservationID: observationID, Full: ioPath(traceID, observationID)})
		got, ok := payloadBudget{share: bare + 5, affordable: true}.render(value, traceID, observationID).(truncation)
		if !ok || hasMediaField(t, got) || markerSize(got) > bare+5 {
			t.Errorf("a marker with no room for the count: %s", mustJSON(t, got))
		}
	})

	t.Run("it is the rule extraction wrote them by", func(t *testing.T) {
		if got := mapping.CountMediaReferences([]any{ref(1), map[string]any{"x": ref(2)}, "text"}); got != 2 {
			t.Errorf("CountMediaReferences = %d, want 2", got)
		}
	})
}

// hasMediaField says whether the marker, as written, carries media_count.
func hasMediaField(t *testing.T, marker truncation) bool {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(t, marker), &fields); err != nil {
		t.Fatal(err)
	}
	_, ok := fields["media_count"]
	return ok
}

// Through the read API: the stored input is a long prompt and then an image,
// and `?expand=io` cuts it. The answer says the payload holds one.
func TestExpandIOSaysWhatMediaTheCutPayloadHolds(t *testing.T) {
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
	if marker := body.Observations[0].Input; !marker.Truncated || marker.MediaCount != 1 {
		t.Errorf("marker = %+v, want it to say the payload holds one media reference", marker)
	}
}
