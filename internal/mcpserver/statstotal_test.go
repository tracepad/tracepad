package mcpserver_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// `get_stats {"group_by": "total"}` (spec 034 #3): the value passes through
// the same enum as every other grouping, and that enum is the served
// document's — a grouping the API accepts and the tool refuses is a
// capability a model cannot reach.

func TestStatsGroupByEnumIsTheDocuments(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	stats := toolByName(t, tools.Tools, "get_stats")
	in := schemaOf(t, stats.InputSchema)
	out := schemaOf(t, stats.OutputSchema)

	response, err := http.Get(h.url + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var document struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `json:"name"`
					Schema struct {
						Enum []string `json:"enum"`
					} `json:"schema"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, parameter := range document.Paths["/api/v1/stats"].Get.Parameters {
		if parameter.Name == "group_by" {
			documented = parameter.Schema.Enum
		}
	}
	if !slices.Contains(documented, "total") {
		t.Fatalf("openapi.json's group_by enum is %v, want total in it", documented)
	}
	for label, enum := range map[string][]string{
		"input":  in.Properties["group_by"].Enum,
		"output": out.Properties["group_by"].Enum,
	} {
		if !slices.Equal(enum, documented) {
			t.Errorf("the tool's %s group_by enum is %v, openapi.json says %v", label, enum, documented)
		}
	}
}
