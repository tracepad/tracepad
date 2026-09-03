package server

import (
	"net/http"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// Score configs over HTTP (spec 014 #15–#17, Testing — scores): the config
// matrix, the binding by name, and that a rule governs only what comes after
// it.

func (h *harness) putConfig(t *testing.T, name string, body map[string]any) scoreConfigResponse {
	t.Helper()
	rec := h.send(t, "PUT", "/api/v1/score-configs/"+name, body)
	expectStatus(t, rec, http.StatusOK)
	return decodeJSON[scoreConfigResponse](t, rec)
}

// The config's own validation: direction required and forbidden per type,
// bounds numeric only and ordered, categories categorical only.
func TestScoreConfigValidationMatrix(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	cases := []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{"numeric without a direction", map[string]any{"data_type": "numeric"}, `needs a "direction"`},
		{"boolean without a direction", map[string]any{"data_type": "boolean"}, `needs a "direction"`},
		{"categorical with a direction", map[string]any{"data_type": "categorical", "direction": "higher", "categories": []string{"a"}}, `takes no "direction"`},
		{"text with a direction", map[string]any{"data_type": "text", "direction": "none"}, `takes no "direction"`},
		{"an unknown direction", map[string]any{"data_type": "numeric", "direction": "up"}, `"direction" must be`},
		{"an unknown type", map[string]any{"data_type": "vibes"}, `"data_type" must be`},
		{"min above max", map[string]any{"data_type": "numeric", "direction": "higher", "min": 2, "max": 1}, `"min" 2 is above "max" 1`},
		{"bounds on a boolean", map[string]any{"data_type": "boolean", "direction": "higher", "max": 1}, `belong to a numeric config`},
		{"categorical without categories", map[string]any{"data_type": "categorical"}, `needs a non-empty "categories"`},
		{"categorical with an empty category", map[string]any{"data_type": "categorical", "categories": []string{"pass", ""}}, "empty entry at index 1"},
		{"categorical with a repeat", map[string]any{"data_type": "categorical", "categories": []string{"pass", "pass"}}, `lists "pass" twice`},
		{"categories on a numeric", map[string]any{"data_type": "numeric", "direction": "none", "categories": []string{"a"}}, "belongs to a categorical config"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expectError(t, h.send(t, "PUT", "/api/v1/score-configs/x", c.body), http.StatusBadRequest, c.wantErr)
		})
	}

	// And the shapes that are right.
	numeric := h.putConfig(t, "accuracy", map[string]any{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1, "description": "judge"})
	if numeric.Name != "accuracy" || *numeric.Direction != "higher" || *numeric.Min != 0 || *numeric.Max != 1 || numeric.Categories != nil {
		t.Errorf("numeric = %+v", numeric)
	}
	verdict := h.putConfig(t, "verdict", map[string]any{"data_type": "categorical", "categories": []string{"pass", "fail"}})
	if verdict.Direction != nil || len(verdict.Categories) != 2 || verdict.Min != nil {
		t.Errorf("categorical = %+v", verdict)
	}
	h.putConfig(t, "tokens", map[string]any{"data_type": "numeric", "direction": "none"})
	h.putConfig(t, "note", map[string]any{"data_type": "text"})

	list := decodeJSON[scoreConfigListResponse](t, h.get(t, "/api/v1/score-configs"))
	if len(list.Configs) != 4 || list.Configs[0].Name != "accuracy" || list.Configs[3].Name != "verdict" {
		t.Errorf("list = %+v, want four by name", list.Configs)
	}
	got := decodeJSON[scoreConfigResponse](t, h.get(t, "/api/v1/score-configs/accuracy"))
	if *got.Description != "judge" {
		t.Errorf("GET = %+v", got)
	}
	expectError(t, h.get(t, "/api/v1/score-configs/nope"), http.StatusNotFound, "not found")
	expectStatus(t, h.send(t, "DELETE", "/api/v1/score-configs/note", nil), http.StatusOK)
	expectError(t, h.send(t, "DELETE", "/api/v1/score-configs/note", nil), http.StatusNotFound, "not found")
}

// The binding (Decision 15): a score whose name has a config must satisfy it,
// checked inside the write; one bad item fails its batch with a 400 naming it;
// a name without a config is as free as ever.
func TestScoreConfigGovernsWrites(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfig(t, "accuracy", map[string]any{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1})
	h.putConfig(t, "passed", map[string]any{"data_type": "boolean", "direction": "higher"})
	h.putConfig(t, "verdict", map[string]any{"data_type": "categorical", "categories": []string{"pass", "fail"}})
	h.putConfig(t, "note", map[string]any{"data_type": "text"})

	score := func(fields map[string]any) map[string]any {
		fields["trace_id"] = scoreTraceID
		return fields
	}
	cases := []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{"type mismatch", score(map[string]any{"name": "accuracy", "string_value": "good"}),
			`"accuracy" is numeric in its config, got text`},
		{"below min", score(map[string]any{"name": "accuracy", "value": -0.5}), "value -0.5 is below the config's min 0"},
		{"above max", score(map[string]any{"name": "accuracy", "value": 1.5}), "value 1.5 is above the config's max 1"},
		{"category outside the list", score(map[string]any{"name": "verdict", "data_type": "categorical", "string_value": "maybe"}),
			`"maybe" is not among the config's categories (pass, fail)`},
		{"boolean with a numeric config", score(map[string]any{"name": "accuracy", "data_type": "boolean", "value": 1}),
			`"accuracy" is numeric in its config, got boolean`},
		{"numeric with a boolean config", score(map[string]any{"name": "passed", "value": 0.5}),
			`"passed" is boolean in its config, got numeric`},
		{"text with a categorical config", score(map[string]any{"name": "verdict", "string_value": "pass"}),
			`"verdict" is categorical in its config, got text`},
		{"numeric with a text config", score(map[string]any{"name": "note", "value": 1}),
			`"note" is text in its config, got numeric`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			expectError(t, h.send(t, "POST", "/api/v1/scores", c.body), http.StatusBadRequest, c.wantErr)
		})
	}
	if list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores")); len(list.Scores) != 0 {
		t.Fatalf("scores = %d, want none written by refused requests", len(list.Scores))
	}

	// The batch: one violating item, nothing written, the index named.
	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		score(map[string]any{"name": "accuracy", "value": 0.9}),
		score(map[string]any{"name": "helpfulness", "value": 7}),
		score(map[string]any{"name": "verdict", "data_type": "categorical", "string_value": "meh"}),
	})
	expectError(t, rec, http.StatusBadRequest, `score at index 2: "meh" is not among`)
	if list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores")); len(list.Scores) != 0 {
		t.Fatalf("scores = %d, want none: a refused batch writes nothing", len(list.Scores))
	}

	// The same batch, compliant: the configured names pass their checks and
	// the unconfigured one is as free as ever.
	rec = h.send(t, "POST", "/api/v1/scores", []map[string]any{
		score(map[string]any{"name": "accuracy", "value": 0.9}),
		score(map[string]any{"name": "helpfulness", "value": 7}),
		score(map[string]any{"name": "verdict", "data_type": "categorical", "string_value": "pass"}),
		score(map[string]any{"name": "passed", "data_type": "boolean", "value": 1}),
		score(map[string]any{"name": "note", "string_value": "fine"}),
	})
	expectStatus(t, rec, http.StatusCreated)
}

// A config replaced between two writes governs only the second (Decision 15,
// #17): the score admitted under the old rule stands.
func TestScoreConfigReplacementGovernsWhatFollows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfig(t, "accuracy", map[string]any{"data_type": "numeric", "direction": "higher", "min": 0, "max": 10})
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "accuracy", "value": 7,
	}), http.StatusCreated)

	tightened := h.putConfig(t, "accuracy", map[string]any{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1})
	if *tightened.Max != 1 {
		t.Fatalf("replaced = %+v", tightened)
	}
	expectError(t, h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "accuracy", "value": 7,
	}), http.StatusBadRequest, "above the config's max 1")

	list := decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?name=accuracy"))
	if len(list.Scores) != 1 || *list.Scores[0].Value != 7 {
		t.Errorf("scores = %+v, want the score admitted under the old rule still there", list.Scores)
	}

	// Deleting the config frees the name again.
	expectStatus(t, h.send(t, "DELETE", "/api/v1/score-configs/accuracy", nil), http.StatusOK)
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", map[string]any{
		"trace_id": scoreTraceID, "name": "accuracy", "string_value": "anything goes",
	}), http.StatusCreated)
}

// A re-PUT of the same body is a no-op: updated_at does not move (#17).
func TestScoreConfigRePutIsANoOp(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := map[string]any{"data_type": "categorical", "categories": []string{"pass", "fail"}, "description": "judge"}
	first := h.putConfig(t, "verdict", body)
	second := h.putConfig(t, "verdict", body)
	if first.UpdatedAt != second.UpdatedAt || first.CreatedAt != second.CreatedAt {
		t.Errorf("re-PUT moved the timestamps: %+v then %+v", first, second)
	}
	// A config name is a score name: any name a score may carry can have a
	// config, spaces and all.
	expectStatus(t, h.send(t, "PUT", "/api/v1/score-configs/judge%20score", map[string]any{"data_type": "text"}), http.StatusOK)
	expectStatus(t, h.get(t, "/api/v1/score-configs/judge%20score"), http.StatusOK)
}
