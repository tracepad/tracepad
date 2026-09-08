package mcpserver_test

import (
	"encoding/json"
	"testing"
)

// `get_score_trends` (spec 025 #8): one tool, one endpoint, and a schema that
// says what comes back — a model reads the schema before it calls, so a field
// the endpoint sends and the schema omits is a field it will never ask about.

func TestScoreTrendToolIsItsEndpoint(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		arguments map[string]any
		path      string
	}{
		{map[string]any{}, "/api/v1/stats/scores"},
		{map[string]any{"group_by": "model"}, "/api/v1/stats/scores?group_by=model"},
		{map[string]any{"name": "hallucination"}, "/api/v1/stats/scores?name=hallucination"},
		{map[string]any{"environment": "production", "group_by": "release"},
			"/api/v1/stats/scores?environment=production&group_by=release"},
		{map[string]any{"limit": 5}, "/api/v1/stats/scores?limit=5"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			structured := h.callRaw(t, "get_score_trends", tc.arguments)
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

func TestScoreTrendToolDeclaresWhatItReturns(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tool := toolByName(t, tools.Tools, "get_score_trends")

	in := schemaOf(t, tool.InputSchema)
	for _, field := range []string{"group_by", "name", "from", "to", "environment", "limit"} {
		if _, offered := in.Properties[field]; !offered {
			t.Errorf("the tool does not offer %q, which the endpoint accepts", field)
		}
	}
	// `user_id` is `/stats`' filter and not this endpoint's: offering it
	// would turn a tool call into a 400 for a parameter that does not exist.
	if _, offered := in.Properties["user_id"]; offered {
		t.Error("the tool offers user_id, which this endpoint refuses")
	}

	out := schemaOf(t, tool.OutputSchema)
	if _, declared := out.Properties["targets"]; !declared {
		t.Error("the tool does not declare targets, which is how it says what was counted")
	}
	// A model that cannot see `omitted` reads a truncated list as the whole
	// list (spec 025 #24).
	if _, declared := out.Properties["omitted"]; !declared {
		t.Error("the tool does not declare omitted, which is how it says the list is not all of it")
	}
	bucket := out.Properties["series"].Items.Properties["buckets"].Items
	for _, field := range []string{"key", "count", "mean", "min", "max", "rate", "categories"} {
		if _, declared := bucket.Properties[field]; !declared {
			t.Errorf("the bucket does not declare %q", field)
		}
	}
}
