package mcpserver_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The `search` tool (spec 011 #9): the trace listing with `q` required and
// `match` on every row, and `list_traces` left exactly as it was.

// schema is as much of a JSON Schema as these assertions read. The client
// receives the schemas as JSON, so they are read as JSON.
type schema struct {
	Type       string             `json:"type"`
	Required   []string           `json:"required"`
	Properties map[string]*schema `json:"properties"`
	Items      *schema            `json:"items"`
}

func schemaOf(t *testing.T, raw any) *schema {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var out schema
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func toolByName(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

func TestSearchToolSchema(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	search := toolByName(t, tools.Tools, "search")
	in := schemaOf(t, search.InputSchema)
	out := schemaOf(t, search.OutputSchema)

	if !slices.Contains(in.Required, "q") {
		t.Errorf("required = %v, want q: a search without text is not a search", in.Required)
	}
	// It takes the listing's filters and paging, so a search can be narrowed
	// without a second tool.
	for _, name := range []string{"environment", "user_id", "from", "to", "status",
		"limit", "cursor", "direction", "count", "fields"} {
		if _, offered := in.Properties[name]; !offered {
			t.Errorf("the search tool does not offer %q, which the endpoint accepts", name)
		}
	}
	// The rows say where they matched.
	match, carried := out.Properties["traces"].Items.Properties["match"]
	if !carried {
		t.Fatal("the search tool's rows do not carry a match")
	}
	for _, name := range []string{"observation_id", "field", "snippet"} {
		if _, named := match.Properties[name]; !named {
			t.Errorf("match = %v, want %q in it", match.Properties, name)
		}
	}

	// `list_traces` does not grow `q` (spec 011 #9): the two questions are
	// different, and a model choosing by description is served by two tools.
	list := toolByName(t, tools.Tools, "list_traces")
	if _, grown := schemaOf(t, list.InputSchema).Properties["q"]; grown {
		t.Errorf("list_traces grew a q; the search tool is the one that has it")
	}
	listRow := schemaOf(t, list.OutputSchema).Properties["traces"].Items
	if _, grown := listRow.Properties["match"]; grown {
		t.Errorf("list_traces advertises a match it can never return")
	}
	// The description is a trigger, not a restatement of the endpoint (#17).
	if !strings.Contains(search.Description, "quotes") {
		t.Errorf("the search tool's description does not say when to reach for it: %q",
			search.Description)
	}
}

func TestSearchToolAnswers(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "search", Arguments: map[string]any{"q": "password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("the search tool refused: %+v", result.Content)
	}
	body, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	answer := struct {
		Traces []struct {
			ID    string `json:"id"`
			Match *struct {
				ObservationID *string `json:"observation_id"`
				Field         string  `json:"field"`
				Snippet       string  `json:"snippet"`
			} `json:"match"`
		} `json:"traces"`
	}{}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Traces) != 1 || answer.Traces[0].ID != traceHex(1) {
		t.Fatalf("traces = %+v, want the one trace whose text says that", answer.Traces)
	}
	match := answer.Traces[0].Match
	if match == nil || match.Field != "input" {
		t.Fatalf("match = %+v, want the field that matched", match)
	}
	if match.ObservationID == nil || *match.ObservationID != spanHex(2) {
		t.Errorf("observation_id = %v, want the observation to call get_observation_io on",
			match.ObservationID)
	}
	if !strings.Contains(match.Snippet, "password") {
		t.Errorf("snippet = %q, want the text around the hit", match.Snippet)
	}

	// The line of text beside the structured result says where the hit was,
	// because the next call is about one observation (#18).
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "input") {
		t.Errorf("content = %+v, want a summary naming where it matched", result.Content)
	}

	// A refusal by the API is a tool error the model can correct itself
	// from, not a protocol error.
	empty, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "search", Arguments: map[string]any{"q": "()"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.IsError {
		t.Errorf("a query with no word in it was accepted: %+v", empty.StructuredContent)
	}
}
