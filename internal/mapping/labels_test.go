package mapping_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// The labels a listing shows are cut at 1,000 characters, at a character
// boundary, and a trace keeps its first 50 distinct tags in order (spec 043
// #14). Everything else stays as sent.
func TestLabelsAreBounded(t *testing.T) {
	long := strings.Repeat("é", 19_999) + "x" // 20,000 characters, 39,999 bytes
	var tags []string
	for i := range 200 {
		tags = append(tags, fmt.Sprintf("tag-%d", i%70))
	}
	tagJSON, err := json.Marshal(tags)
	if err != nil {
		t.Fatal(err)
	}
	span := otlptest.ProbeSpan(
		"langfuse.trace.name", long,
		"langfuse.user.id", long,
		"langfuse.session.id", long,
		"langfuse.environment", long,
		"langfuse.release", long,
		"langfuse.version", long,
		"langfuse.observation.model.name", long,
		"langfuse.trace.tags", string(tagJSON),
	)
	span.Name = long
	result := mapping.Map(otlptest.Export(span))
	if len(result.Traces) != 1 || len(result.Observations) != 1 {
		t.Fatalf("mapped %d traces, %d observations", len(result.Traces), len(result.Observations))
	}
	trace, obs := result.Traces[0], result.Observations[0]
	want := strings.Repeat("é", 1000)
	for name, got := range map[string]string{
		"name": trace.Name, "user_id": trace.UserID, "session_id": trace.SessionID,
		"environment": trace.Environment, "release": trace.Release, "version": trace.Version,
		"model": obs.Model, "observation name": obs.Name,
	} {
		if got != want {
			t.Errorf("%s is %d characters (valid UTF-8: %t), want the first 1000",
				name, utf8.RuneCountInString(got), utf8.ValidString(got))
		}
	}
	var wantTags []string
	for i := range 50 {
		wantTags = append(wantTags, fmt.Sprintf("tag-%d", i))
	}
	if !slices.Equal(trace.Tags, wantTags) {
		t.Errorf("tags = %v, want the first 50 distinct in order", trace.Tags)
	}

	// A label within the bound is untouched; repeated tags count once.
	short := mapping.Map(otlptest.SpanWith("langfuse.trace.name", "checkout", "langfuse.trace.tags", `["a","b","a"]`))
	if got := short.Traces[0]; got.Name != "checkout" || !slices.Equal(got.Tags, []string{"a", "b"}) {
		t.Errorf("short trace = %q %v, want the name as sent and the tags deduplicated", got.Name, got.Tags)
	}
}

// Two tags that differ only past the cut are one tag, as they are stored.
func TestTagsDeduplicateAfterTheCut(t *testing.T) {
	prefix := strings.Repeat("t", 1000)
	tags, err := json.Marshal([]string{prefix + "a", prefix + "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	got := mapping.Map(otlptest.SpanWith("langfuse.trace.tags", string(tags))).Traces[0].Tags
	if !slices.Equal(got, []string{prefix, "c"}) {
		t.Errorf("tags = %d of them, want the cut prefix once and c", len(got))
	}
}

func TestCountSpans(t *testing.T) {
	if got := mapping.CountSpans(otlptest.Bulk(1, 7, 3)); got != 21 {
		t.Errorf("CountSpans = %d, want 21", got)
	}
	if got := mapping.CountSpans(nil); got != 0 {
		t.Errorf("CountSpans(nil) = %d", got)
	}
}
