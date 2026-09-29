package mcpserver_test

import (
	"slices"
	"testing"
)

// Tokens where people look (spec 049 #11): every tool that returns a row or a
// bucket carrying `tokens` declares it with its five classes, `list_traces`
// offers `min_tokens`, and `list_users` sorts by tokens. A model reading only
// the schemas has to be able to find all three; `get_stats` returned the
// object for a whole spec without declaring it.
func TestTokensAreDeclaredWhereTheyAreReturned(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	output := func(name string) *schema {
		return schemaOf(t, toolByName(t, tools.Tools, name).OutputSchema)
	}
	for where, row := range map[string]*schema{
		"list_traces row":   output("list_traces").Properties["traces"].Items,
		"search row":        output("search").Properties["traces"].Items,
		"get_trace":         output("get_trace"),
		"get_last_trace":    output("get_last_trace"),
		"list_sessions row": output("list_sessions").Properties["sessions"].Items,
		"get_session":       output("get_session"),
		"get_session trace": output("get_session").Properties["traces"].Items,
		"list_users row":    output("list_users").Properties["users"].Items,
		"get_user":          output("get_user"),
		"get_stats bucket":  output("get_stats").Properties["buckets"].Items,
	} {
		tokens, declared := row.Properties["tokens"]
		if !declared {
			t.Errorf("%s does not declare tokens", where)
			continue
		}
		for _, class := range []string{"input", "output", "cache_read", "reasoning", "cache_write"} {
			if tokens.Properties[class] == nil || tokens.Properties[class].Type != "integer" {
				t.Errorf("%s: tokens.%s is not declared as an integer", where, class)
			}
		}
	}

	for _, name := range []string{"list_traces", "search", "get_last_trace"} {
		in := schemaOf(t, toolByName(t, tools.Tools, name).InputSchema)
		if field := in.Properties["min_tokens"]; field == nil || field.Type != "integer" {
			t.Errorf("%s does not offer min_tokens as an integer", name)
		}
	}
	sorts := schemaOf(t, toolByName(t, tools.Tools, "list_users").InputSchema).Properties["sort"].Enum
	if !slices.Contains(sorts, "tokens") {
		t.Errorf("list_users sorts by %v, not by tokens", sorts)
	}
}
