package mcpserver_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mcpserver"
)

// The one line of text beside a structured result is prose a client shows,
// and some show it on a terminal: a value from a trace goes into it escaped,
// the way it does on the CLI (spec 004 #35).
func TestSummariesEscapeWhatATraceCarries(t *testing.T) {
	const attack = "x\x1b]52;c;ZWNobyBwd25lZA==\x07\u009b2J‮\nforged line"
	value, err := json.Marshal(attack)
	if err != nil {
		t.Fatal(err)
	}
	v := string(value)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"trace list", `{"traces":[{"id":` + v + `,"timestamp":` + v + `}]}`},
		{"search", `{"traces":[{"id":` + v + `,"match":{"field":` + v + `,"snippet":` + v + `}}]}`},
		{"trace", `{"id":` + v + `,"name":` + v + `,"environment":` + v + `}`},
		{"io", `{"observation_id":` + v + `}`},
		{"session list", `{"sessions":[{"id":` + v + `,"last_seen":` + v + `}]}`},
		{"session", `{"id":` + v + `}`},
		{"user list", `{"users":[{"user_id":` + v + `}]}`},
		{"user", `{"user_id":` + v + `,"last_seen":` + v + `}`},
		{"prompt", `{"name":` + v + `,"type":` + v + `,"labels":[` + v + `]}`},
		{"scores", `{"scores":[{"name":` + v + `}]}`},
		{"stats", `{"group_by":` + v + `,"unit":` + v + `}`},
		{"score trends", `{"group_by":` + v + `,"targets":` + v + `,"series":[{"name":` + v + `}]}`},
		{"runs", `{"runs":[{"id":` + v + `,"status":` + v + `}]}`},
		{"run", `{"status":` + v + `,"dataset":` + v + `}`},
	} {
		summarize, ok := mcpserver.Summaries[tc.name]
		if !ok {
			t.Fatalf("no summary called %q", tc.name)
		}
		got := summarize(json.RawMessage(tc.body))
		if !strings.Contains(got, `\x1b]52;c;`) {
			t.Errorf("%s: the value is missing or not escaped: %q", tc.name, got)
		}
		for _, raw := range []string{"\x1b", "\x07", "\u009b", "‮", "\n"} {
			if strings.Contains(got, raw) {
				t.Errorf("%s: raw %q in %q", tc.name, raw, got)
			}
		}
	}
}
