package mcpserver_test

import (
	"slices"
	"strings"
	"testing"
)

// The MCP half of spec 012: the three tools that take the listing's filters
// take the four new ones, the rows and the tree carry the new fields, and
// `get_stats` can group by release.

func TestWireFiltersReachEveryListingTool(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// The three tools that stand in front of the trace listing and its
	// shortcut. A filter the endpoint accepts and a tool does not offer is a
	// capability a model cannot reach (MCP contract).
	for _, name := range []string{"list_traces", "search", "get_last_trace"} {
		t.Run(name, func(t *testing.T) {
			in := schemaOf(t, toolByName(t, tools.Tools, name).InputSchema)
			for _, filter := range []string{"release", "version", "type", "prompt"} {
				if _, offered := in.Properties[filter]; !offered {
					t.Errorf("%s does not offer %q, which the endpoint accepts", name, filter)
				}
			}
			// The type filter is a closed set, so a model cannot spend a
			// call discovering that "tool call" is not a value.
			kinds := in.Properties["type"]
			if kinds == nil || len(kinds.Enum) != 10 {
				t.Errorf("the type filter is not the ten-value vocabulary: %v", kinds)
			}
			// Descriptions are triggers, not restatements (spec 004 #17).
			for _, filter := range []string{"release", "type", "prompt"} {
				if !strings.Contains(in.Properties[filter].Description, "user") {
					t.Errorf("%s's %s description does not say when to reach for it: %q",
						name, filter, in.Properties[filter].Description)
				}
			}
		})
	}
}

func TestWireFieldsAreDeclared(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	row := schemaOf(t, toolByName(t, tools.Tools, "list_traces").OutputSchema).
		Properties["traces"].Items
	for _, field := range []string{"release", "version", "ttft_ms"} {
		if _, declared := row.Properties[field]; !declared {
			t.Errorf("the listing row does not declare %q", field)
		}
	}

	trace := schemaOf(t, toolByName(t, tools.Tools, "get_trace").OutputSchema)
	node := trace.Defs["observation"]
	if node == nil {
		t.Fatal("the trace schema has no observation definition")
	}
	for _, field := range []string{
		"completion_start_time", "ttft_ms", "prompt", "input_bytes", "output_bytes",
	} {
		if _, declared := node.Properties[field]; !declared {
			t.Errorf("the observation does not declare %q", field)
		}
	}
	if kinds := node.Properties["type"]; kinds == nil || len(kinds.Enum) != 10 {
		t.Errorf("the observation's type is not the ten-value vocabulary: %v", kinds)
	}
	if prompt := node.Properties["prompt"]; prompt == nil ||
		prompt.Properties["name"] == nil || prompt.Properties["version"] == nil {
		t.Errorf("the prompt link does not carry a name and a version")
	}
}

func TestStatsGroupsByRelease(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats := toolByName(t, tools.Tools, "get_stats")
	in := schemaOf(t, stats.InputSchema)
	out := schemaOf(t, stats.OutputSchema)

	for label, enum := range map[string][]string{
		"input":  in.Properties["group_by"].Enum,
		"output": out.Properties["group_by"].Enum,
	} {
		for _, value := range []string{"release", "total"} {
			if !slices.Contains(enum, value) {
				t.Errorf("the %s group_by enum has no %s: %v", label, value, enum)
			}
		}
	}
	// The description says when to reach for each, which is the only thing
	// a model has to go on when choosing between six groupings.
	for _, value := range []string{"release", "total"} {
		if !strings.Contains(stats.Description, value) {
			t.Errorf("the tool's description does not mention the %s grouping: %q", value, stats.Description)
		}
	}
}
