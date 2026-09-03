package mapping_test

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// The run link (spec 014 #2, ingest contract): two attributes claimed into two
// trace columns, only when they have the shape of an id, and the item only
// beside its run.

const (
	runID  = "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7"
	itemID = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

func TestRunLinkClaimsBothAttributes(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith("tracepad.run_id", runID, "tracepad.item_id", itemID))

	trace := result.Traces[0]
	if trace.RunID != runID || trace.ItemID != itemID {
		t.Errorf("link = run %q item %q, want both columns set", trace.RunID, trace.ItemID)
	}
	metadata := result.Observations[0].Metadata
	for _, key := range []string{"tracepad.run_id", "tracepad.item_id"} {
		if _, kept := metadata[key]; kept {
			t.Errorf("metadata[%q] is set, want a claimed attribute out of metadata (spec 012 #7)", key)
		}
	}
}

// A value that is not a 32-hex id claims nothing: it stays in metadata where
// a harness that stamped a run name rather than its id will find it.
func TestRunLinkRefusesWhatIsNotAnID(t *testing.T) {
	cases := []struct {
		name string
		run  string
		item string
	}{
		{"a name rather than an id", "nightly-2026-09-03", itemID},
		{"upper-case hex", strings.ToUpper(runID), itemID},
		{"too short", runID[:31], itemID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := mapping.Map(otlptest.SpanWith("tracepad.run_id", c.run, "tracepad.item_id", c.item))
			trace := result.Traces[0]
			if trace.RunID != "" || trace.ItemID != "" {
				t.Errorf("link = run %q item %q, want neither column set", trace.RunID, trace.ItemID)
			}
			metadata := result.Observations[0].Metadata
			if metadata["tracepad.run_id"] != c.run {
				t.Errorf("metadata = %v, want the unusable run id preserved", metadata)
			}
			// The item was well-formed, but an item without a run
			// names nothing: it stays visible too.
			if metadata["tracepad.item_id"] != c.item {
				t.Errorf("metadata = %v, want the item id preserved beside its unusable run", metadata)
			}
		})
	}
}

// An item without a run on the same span claims nothing and sets nothing.
func TestItemWithoutRunClaimsNothing(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith("tracepad.item_id", itemID))
	if trace := result.Traces[0]; trace.RunID != "" || trace.ItemID != "" {
		t.Errorf("link = run %q item %q, want nothing", trace.RunID, trace.ItemID)
	}
	if got := result.Observations[0].Metadata["tracepad.item_id"]; got != itemID {
		t.Errorf("metadata = %v, want the item id preserved", result.Observations[0].Metadata)
	}
}

// A malformed item beside a good run: the run is claimed, the item stays.
func TestMalformedItemBesideAGoodRun(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith("tracepad.run_id", runID, "tracepad.item_id", "case-17"))
	trace := result.Traces[0]
	if trace.RunID != runID || trace.ItemID != "" {
		t.Errorf("link = run %q item %q, want the run alone", trace.RunID, trace.ItemID)
	}
	if got := result.Observations[0].Metadata["tracepad.item_id"]; got != "case-17" {
		t.Errorf("metadata = %v, want the unusable item preserved", result.Observations[0].Metadata)
	}
}

// The ranked chain keeps the root's value over a child's absence (spec 012
// #11): three spans, the link on the root only.
func TestRunLinkSurvivesTheTraceMerge(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{{"tracepad.run_id", runID, "tracepad.item_id", itemID}, nil, nil},
	}))
	trace := result.Traces[0]
	if trace.RunID != runID || trace.ItemID != itemID {
		t.Errorf("link = run %q item %q, want the root's values kept through the merge", trace.RunID, trace.ItemID)
	}

	// And a link on a child span alone still links the trace: any span
	// may carry it, the root is only the convention.
	result = mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{nil, {"tracepad.run_id", runID, "tracepad.item_id", itemID}},
	}))
	if trace := result.Traces[0]; trace.RunID != runID || trace.ItemID != itemID {
		t.Errorf("link from a child = run %q item %q, want it kept", trace.RunID, trace.ItemID)
	}
}

// TestRunAndItemOnDifferentSpans: the contract binds the item to a run on the
// *trace*, not on the span it was stamped on (spec 014, ingest contract). A
// harness that puts the run on the root span and the item on the span that
// answered the case is the shape the docs' recipe grows into, and reading the
// item only beside its own run would file that trace under the run with no
// item — a hole part 2's run view would report as a missing case.
func TestRunAndItemOnDifferentSpans(t *testing.T) {
	result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
		ScopeName: "langfuse-sdk", ScopeVersion: "4.7.0",
		Spans: [][]string{{"tracepad.run_id", runID}, {"tracepad.item_id", itemID}},
	}))
	if trace := result.Traces[0]; trace.RunID != runID || trace.ItemID != itemID {
		t.Errorf("link = run %q item %q, want both: the rule is trace-level", trace.RunID, trace.ItemID)
	}
}

// The Langfuse experiment namespace is not mapped: one namespace for the link
// (spec 014, ingest contract).
func TestLangfuseExperimentStaysInMetadata(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith("langfuse.experiment.run_id", runID))
	if trace := result.Traces[0]; trace.RunID != "" {
		t.Errorf("run_id = %q, want no claim from the langfuse.experiment namespace", trace.RunID)
	}
	if got := result.Observations[0].Metadata["langfuse.experiment.run_id"]; got != runID {
		t.Errorf("metadata = %v, want the attribute preserved", result.Observations[0].Metadata)
	}
}
