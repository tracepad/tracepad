package mcpserver_test

import (
	"encoding/json"
	"testing"
)

// `get_facets` (spec 027 #5): what a filter can be set to, which is the
// question a model asks before it guesses at a value.

func TestFacetsToolIsItsEndpoint(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		arguments map[string]any
		path      string
	}{
		// A pinned range, because the default is "now" and two calls a
		// moment apart would honestly differ.
		{map[string]any{"from": "2026-08-01T00:00:00Z", "to": "2026-09-01T00:00:00Z"},
			"/api/v1/facets?from=2026-08-01T00%3A00%3A00Z&to=2026-09-01T00%3A00%3A00Z"},
		{map[string]any{"from": "2026-08-01T00:00:00Z", "to": "2026-08-15T00:00:00Z"},
			"/api/v1/facets?from=2026-08-01T00%3A00%3A00Z&to=2026-08-15T00%3A00%3A00Z"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			structured := h.callRaw(t, "get_facets", tc.arguments)
			var fromTool, fromEndpoint any
			if err := json.Unmarshal(structured, &fromTool); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(h.get(t, tc.path), &fromEndpoint); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(fromTool)
			b, _ := json.Marshal(fromEndpoint)
			if string(a) != string(b) {
				t.Errorf("the tool and the endpoint disagree:\n%s\n%s", a, b)
			}
		})
	}
}

func TestFacetsToolDeclaresWhatItReturns(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := toolByName(t, tools.Tools, "get_facets")

	in := schemaOf(t, tool.InputSchema)
	for _, field := range []string{"from", "to"} {
		if _, offered := in.Properties[field]; !offered {
			t.Errorf("the tool does not offer %q, which the endpoint accepts", field)
		}
	}
	// The range is the only thing it takes (#4): offering `environment`
	// would promise filter-aware counts the endpoint does not compute.
	if _, offered := in.Properties["environment"]; offered {
		t.Error("the tool offers environment, which this endpoint refuses")
	}

	out := schemaOf(t, tool.OutputSchema)
	for _, column := range []string{"environment", "release", "name"} {
		list, declared := out.Properties[column]
		if !declared {
			t.Fatalf("the tool does not declare %q", column)
		}
		for _, field := range []string{"value", "count"} {
			if _, held := list.Items.Properties[field]; !held {
				t.Errorf("a %s value does not declare %q", column, field)
			}
		}
	}
	// A model that cannot see `omitted` reads a capped list as the whole
	// list (spec 027 #2).
	if _, declared := out.Properties["omitted"]; !declared {
		t.Error("the tool does not declare omitted, which is how it says the list is not all of it")
	}
}

// The list form travels through the existing tools untouched, which is what
// makes `get_facets` worth calling: a value it returns can be pasted straight
// into `list_traces`.
func TestTheListFormTravelsThroughTheTraceTools(t *testing.T) {
	h := newHarness(t)

	structured := h.callRaw(t, "list_traces", map[string]any{"environment": "production,staging"})
	var fromTool, fromEndpoint any
	if err := json.Unmarshal(structured, &fromTool); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(h.get(t, "/api/v1/traces?environment=production%2Cstaging"), &fromEndpoint); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(fromTool)
	b, _ := json.Marshal(fromEndpoint)
	if string(a) != string(b) {
		t.Errorf("the tool and the endpoint disagree:\n%s\n%s", a, b)
	}
}
