package mcpserver_test

import (
	"encoding/json"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// The two user tools and `get_stats`'s new filter (spec 023 #7). Each maps
// 1:1 onto one endpoint, which is the whole claim: a tool's structured result
// is the endpoint's own bytes (spec 004 #16).

// seedHour is the hour the fixture's traces fall in; the listing answers from
// the rollup, so a pass has to have taken it.
func rollTheFixture(t *testing.T, h *harness) {
	t.Helper()
	project, err := h.store.ProjectByName("test")
	if err != nil || project == nil {
		t.Fatalf("project = %v, err = %v", project, err)
	}
	hour := seedBase / 1e9 / 3600 * 3600
	if err := h.writer.Submit(t.Context(), store.RollHour(project.ID, hour, seedBase)); err != nil {
		t.Fatal(err)
	}
}

func TestUserToolsAreTheirEndpoints(t *testing.T) {
	h := newHarness(t)
	rollTheFixture(t, h)

	for _, tc := range []struct {
		tool      string
		arguments map[string]any
		path      string
	}{
		{"list_users", map[string]any{}, "/api/v1/users"},
		{"list_users", map[string]any{"sort": "cost"}, "/api/v1/users?sort=cost"},
		{"get_user", map[string]any{"user_id": "u1"}, "/api/v1/users/u1"},
		{"get_stats", map[string]any{"group_by": "hour", "user_id": "u1"},
			"/api/v1/stats?group_by=hour&user_id=u1"},
	} {
		t.Run(tc.tool+" "+tc.path, func(t *testing.T) {
			structured := h.callRaw(t, tc.tool, tc.arguments)
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

// TestUserToolsDeclareWhatTheyReturn: the schemas are the contract a model
// reads before it calls, so a field the endpoint sends and the schema omits is
// a field the model will not ask about.
func TestUserToolsDeclareWhatTheyReturn(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	row := schemaOf(t, toolByName(t, tools.Tools, "list_users").OutputSchema).
		Properties["users"].Items
	for _, field := range []string{"user_id", "traces", "error_count", "total_cost",
		"sessions", "first_seen", "last_seen"} {
		if _, declared := row.Properties[field]; !declared {
			t.Errorf("list_users' row does not declare %q", field)
		}
	}

	user := schemaOf(t, toolByName(t, tools.Tools, "get_user").OutputSchema)
	if _, declared := user.Properties["latency_ms"]; !declared {
		t.Error("get_user does not declare latency_ms, which only it returns")
	}

	stats := schemaOf(t, toolByName(t, tools.Tools, "get_stats").InputSchema)
	if _, offered := stats.Properties["user_id"]; !offered {
		t.Error("get_stats does not offer user_id, which the endpoint accepts")
	}
	bucket := schemaOf(t, toolByName(t, tools.Tools, "get_stats").OutputSchema).
		Properties["buckets"].Items
	if _, declared := bucket.Properties["sessions"]; !declared {
		t.Error("get_stats' bucket does not declare sessions")
	}
}
