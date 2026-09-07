package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Prompts end to end (spec 003, Testing #2 and #3): the version lifecycle
// through the real handlers, and the concurrency guarantee that makes version
// numbers an audit trail rather than a race.

func chatBody(content string, extra map[string]any) map[string]any {
	body := map[string]any{
		"type":   "chat",
		"prompt": []map[string]any{{"role": "system", "content": content}},
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}

// Create, label, move, remove, fetch by every path: the whole reason prompt
// management exists is that a promotion and a rollback are label moves (#12).
func TestPromptLifecycle(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	first := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		chatBody("You are terse.", map[string]any{
			"config":         map[string]any{"model": "claude", "temperature": 0.2},
			"commit_message": "first cut",
			"labels":         []string{"production"},
		}))
	expectStatus(t, first, http.StatusCreated)
	created := decodeJSON[promptResponse](t, first)
	if created.Version != 1 || created.Type != store.PromptChat {
		t.Fatalf("first version = %+v, want version 1 of a chat prompt", created)
	}
	if len(created.Labels) != 1 || created.Labels[0] != "production" {
		t.Errorf("labels = %v, want the one assigned at creation", created.Labels)
	}

	second := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		chatBody("You are terse and kind.", map[string]any{"commit_message": "tighten tone"}))
	expectStatus(t, second, http.StatusCreated)
	if version := decodeJSON[promptResponse](t, second).Version; version != 2 {
		t.Fatalf("second version = %d, want 2", version)
	}

	// Unqualified GET is the newest version (#13); the label still points
	// at the version it was pinned to.
	latest := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize"))
	if latest.Version != 2 {
		t.Errorf("unqualified GET = version %d, want the latest", latest.Version)
	}
	if labelled := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?label=production")); labelled.Version != 1 {
		t.Errorf("?label=production = version %d, want 1", labelled.Version)
	}
	if pinned := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?version=1")); pinned.CommitMessage != "first cut" {
		t.Errorf("?version=1 = %+v, want the first version", pinned)
	}
	// `latest` is virtual and agrees with the unqualified path (#11).
	if virtual := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?label=latest")); virtual.Version != 2 {
		t.Errorf("?label=latest = version %d, want 2", virtual.Version)
	}
	if cache := h.get(t, "/api/v1/prompts/summarize").Header().Get("Cache-Control"); cache != promptCacheControl {
		t.Errorf("Cache-Control = %q, want %q", cache, promptCacheControl)
	}

	// Promote: move the label instead of writing a new version.
	moved := h.send(t, "PUT", "/api/v1/prompts/summarize/labels/production", map[string]any{"version": 2})
	expectStatus(t, moved, http.StatusOK)
	if label := decodeJSON[labelResponse](t, moved); label.Version != 2 || label.Label != "production" {
		t.Errorf("move = %+v", label)
	}
	if labelled := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?label=production")); labelled.Version != 2 {
		t.Errorf("after the move ?label=production = version %d, want 2", labelled.Version)
	}
	// Roll back the same way.
	expectStatus(t, h.send(t, "PUT", "/api/v1/prompts/summarize/labels/production", map[string]any{"version": 1}), http.StatusOK)
	if labelled := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?label=production")); labelled.Version != 1 {
		t.Errorf("after the rollback ?label=production = version %d, want 1", labelled.Version)
	}

	removed := h.send(t, "DELETE", "/api/v1/prompts/summarize/labels/production", nil)
	expectStatus(t, removed, http.StatusOK)
	if label := decodeJSON[labelResponse](t, removed); label.Version != 1 {
		t.Errorf("delete = %+v, want the version it pointed at", label)
	}
	expectError(t, h.get(t, "/api/v1/prompts/summarize?label=production"), http.StatusNotFound, "production")

	versions := decodeJSON[promptVersionListResponse](t, h.get(t, "/api/v1/prompts/summarize/versions"))
	if len(versions.Versions) != 2 || versions.Versions[0].Version != 2 {
		t.Fatalf("versions = %+v, want both, newest first", versions.Versions)
	}
	// Version lists are for picking and diffing: no bodies (#18).
	if strings.Contains(h.get(t, "/api/v1/prompts/summarize/versions").Body.String(), "terse") {
		t.Error("a version list must not carry prompt bodies")
	}

	prompts := decodeJSON[promptListResponse](t, h.get(t, "/api/v1/prompts"))
	if len(prompts.Prompts) != 1 {
		t.Fatalf("prompts = %+v, want the one name", prompts.Prompts)
	}
	if summary := prompts.Prompts[0]; summary.Name != "summarize" ||
		summary.LatestVersion != 2 || summary.Type != store.PromptChat || len(summary.Labels) != 0 {
		t.Errorf("summary = %+v", summary)
	}
}

// A text prompt is a JSON string, stored and returned verbatim (#15).
func TestPromptTextRoundTrip(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	body := map[string]any{"type": "text", "prompt": "Summarize {{input}} in one line."}
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/one-liner/versions", body), http.StatusCreated)

	got := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/one-liner"))
	if string(got.Prompt) != `"Summarize {{input}} in one line."` {
		t.Errorf("prompt = %s, want it stored verbatim", got.Prompt)
	}
	if got.Type != store.PromptText {
		t.Errorf("type = %q", got.Type)
	}
}

// A name that changes shape between versions breaks every client fetching it
// by label, so the type is part of the name's contract (#10).
func TestPromptTypeIsFixedByTheFirstVersion(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", chatBody("You are terse.", nil)),
		http.StatusCreated)

	// Declared as text against a chat name.
	mismatch := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"type": "text", "prompt": "You are terse."})
	expectError(t, mismatch, http.StatusBadRequest, "is a chat prompt")

	// The same mismatch with the type left out: the body's own shape is
	// what contradicts the name.
	inferred := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"prompt": "You are terse."})
	expectError(t, inferred, http.StatusBadRequest, "is a chat prompt")

	// A stated type that contradicts its own body never reaches the writer.
	selfContradictory := h.send(t, "POST", "/api/v1/prompts/other/versions",
		map[string]any{"type": "chat", "prompt": "not a message array"})
	expectError(t, selfContradictory, http.StatusBadRequest, "must be an array of messages")

	// Omitting the type is fine once the name has a shape.
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"prompt": []map[string]any{{"role": "user", "content": "hi"}}}), http.StatusCreated)
}

// The first version of a name must state its type (#10).
func TestPromptFirstVersionNeedsAType(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"prompt": "You are terse."})
	expectError(t, rec, http.StatusBadRequest, "first version must state")
}

// `latest` is reserved: it always names the highest version, so it can never
// be stored as a label (#11).
func TestPromptLatestIsReserved(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	atCreation := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		chatBody("You are terse.", map[string]any{"labels": []string{"latest"}}))
	expectError(t, atCreation, http.StatusBadRequest, "reserved")

	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", chatBody("You are terse.", nil)),
		http.StatusCreated)
	expectError(t, h.send(t, "PUT", "/api/v1/prompts/summarize/labels/latest", map[string]any{"version": 1}),
		http.StatusBadRequest, "reserved")
	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/summarize/labels/latest", nil),
		http.StatusBadRequest, "reserved")
}

// Every way of asking for something that is not there (edge cases).
func TestPromptNotFound(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", chatBody("You are terse.", nil)),
		http.StatusCreated)

	expectError(t, h.get(t, "/api/v1/prompts/unknown"), http.StatusNotFound, "not found")
	expectError(t, h.get(t, "/api/v1/prompts/unknown/versions"), http.StatusNotFound, "not found")
	expectError(t, h.get(t, "/api/v1/prompts/summarize?version=9"), http.StatusNotFound, "no version 9")
	expectError(t, h.get(t, "/api/v1/prompts/summarize?label=staging"), http.StatusNotFound, "staging")
	expectError(t, h.send(t, "PUT", "/api/v1/prompts/summarize/labels/staging", map[string]any{"version": 9}),
		http.StatusNotFound, "no version 9")
	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/summarize/labels/staging", nil),
		http.StatusNotFound, "no label")
}

// The request surface is as strict as the score surface (#13, #17).
func TestPromptRequestValidation(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", chatBody("You are terse.", nil)),
		http.StatusCreated)

	expectError(t, h.get(t, "/api/v1/prompts/summarize?version=1&label=production"),
		http.StatusBadRequest, "not both")
	expectError(t, h.get(t, "/api/v1/prompts/summarize?version=zero"),
		http.StatusBadRequest, "positive whole number")
	expectError(t, h.get(t, "/api/v1/prompts/-bad"), http.StatusBadRequest, "prompt name")
	expectError(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"prompt": []map[string]any{{"content": "no role"}}}),
		http.StatusBadRequest, `needs a non-empty "role"`)
	expectError(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		map[string]any{"type": "chat", "prompt": []any{}}),
		http.StatusBadRequest, "at least one message")
	expectError(t, h.call(t, "POST", "/api/v1/prompts/summarize/versions",
		[]byte(`{"type":"chat","prompt":[{"role":"user","content":"hi"}],"commit_mesage":"typo"}`)),
		http.StatusBadRequest, `unknown field "commit_mesage"`)
	expectError(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
		chatBody("You are terse.", map[string]any{"config": "not an object"})),
		http.StatusBadRequest, "must be a JSON object")
	expectError(t, h.send(t, "PUT", "/api/v1/prompts/summarize/labels/production", map[string]any{}),
		http.StatusBadRequest, "positive whole number")
	expectError(t, h.call(t, "PUT", "/api/v1/prompts/summarize/labels/production", nil),
		http.StatusBadRequest, "empty")
}

// An empty selector is a client whose template left a variable unset. Reading
// it as "not given" would serve the latest version to a caller that believes
// it asked for the released one (#23).
func TestPromptRefusesValuelessSelectors(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", chatBody("You are terse.", nil)),
		http.StatusCreated)

	expectError(t, h.get(t, "/api/v1/prompts/summarize?label="), http.StatusBadRequest, "without a value")
	expectError(t, h.get(t, "/api/v1/prompts/summarize?version="), http.StatusBadRequest, "without a value")
	expectError(t, h.call(t, "POST", "/api/v1/prompts/summarize/versions?label=production",
		[]byte(`{"prompt":[{"role":"user","content":"hi"}]}`)),
		http.StatusBadRequest, "unknown query parameter")
}

// Versions are append-only, so an empty message cannot be edited away later:
// the client that fetches it would only find out when the model call fails
// (#23).
func TestPromptRefusesEmptyMessageContent(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	expectError(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", map[string]any{
		"type": "chat", "prompt": []map[string]any{{"role": "system", "content": nil}},
	}), http.StatusBadRequest, `needs a "content"`)
	expectError(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", map[string]any{
		"type": "chat", "prompt": []map[string]any{{"role": "system", "content": ""}},
	}), http.StatusBadRequest, `empty "content"`)

	// Structured content is still whatever the client says it is (#15).
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions", map[string]any{
		"type": "chat",
		"prompt": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "hi"},
		}}},
	}), http.StatusCreated)
}

// Version and prompt listings walk by cursor without gaps or repeats (#18).
func TestPromptListPagination(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, name := range []string{"alpha", "beta", "gamma"} {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/"+name+"/versions", chatBody("You are "+name, nil)),
			http.StatusCreated)
	}
	for range 2 {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/alpha/versions", chatBody("again", nil)),
			http.StatusCreated)
	}

	var names []string
	path := "/api/v1/prompts?limit=1"
	for range 5 {
		page := decodeJSON[promptListResponse](t, h.get(t, path))
		for _, summary := range page.Prompts {
			names = append(names, summary.Name)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/prompts?limit=1&cursor=" + *page.NextCursor
	}
	if fmt.Sprint(names) != "[alpha beta gamma]" {
		t.Errorf("prompt walk = %v, want every name once, alphabetically", names)
	}

	var versions []int
	path = "/api/v1/prompts/alpha/versions?limit=2"
	for range 5 {
		page := decodeJSON[promptVersionListResponse](t, h.get(t, path))
		for _, version := range page.Versions {
			versions = append(versions, version.Version)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/prompts/alpha/versions?limit=2&cursor=" + *page.NextCursor
	}
	if fmt.Sprint(versions) != "[3 2 1]" {
		t.Errorf("version walk = %v, want every version once, newest first", versions)
	}
}

// Both listings read their labels in one query and group them in memory, so
// what has to be pinned is that a version never borrows another version's
// label and a name never borrows another name's. The summary's `type` and
// `updated_at` come from the newest version's row for the same reason.
func TestPromptLabelsAreGroupedByVersionAndName(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for range 3 {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
			chatBody("You are terse.", nil)), http.StatusCreated)
	}
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/translate/versions",
		map[string]any{"type": "text", "prompt": "Translate {{input}}."}), http.StatusCreated)

	for _, pin := range []struct {
		name    string
		label   string
		version int
	}{
		{"summarize", "production", 1},
		{"summarize", "staging", 3},
		{"summarize", "canary", 3},
		{"translate", "production", 1},
	} {
		expectStatus(t, h.send(t, "PUT",
			fmt.Sprintf("/api/v1/prompts/%s/labels/%s", pin.name, pin.label),
			map[string]any{"version": pin.version}), http.StatusOK)
	}

	versions := decodeJSON[promptVersionListResponse](t, h.get(t, "/api/v1/prompts/summarize/versions"))
	labelsByVersion := map[int]string{}
	for _, version := range versions.Versions {
		labelsByVersion[version.Version] = fmt.Sprint(version.Labels)
	}
	if labelsByVersion[3] != "[canary staging]" || labelsByVersion[2] != "[]" || labelsByVersion[1] != "[production]" {
		t.Errorf("labels by version = %v", labelsByVersion)
	}
	// The name's whole map comes back beside the page, so "where is
	// production" is answered without paging to the version it is on (#12).
	if fmt.Sprint(versions.Labels) != "map[canary:3 production:1 staging:3]" {
		t.Errorf("the name's labels = %v", versions.Labels)
	}
	page := decodeJSON[promptVersionListResponse](t, h.get(t, "/api/v1/prompts/summarize/versions?limit=1"))
	if len(page.Versions) != 1 || fmt.Sprint(page.Labels) != "map[canary:3 production:1 staging:3]" {
		t.Errorf("on a one-row page the labels = %v, want the name's whole map", page.Labels)
	}
	if bare := decodeJSON[promptVersionListResponse](t,
		h.get(t, "/api/v1/prompts/translate/versions")); len(bare.Labels) != 1 {
		t.Errorf("the other name's labels = %v", bare.Labels)
	}

	third := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?version=3"))
	if fmt.Sprint(third.Labels) != "[canary staging]" {
		t.Errorf("labels of version 3 = %v", third.Labels)
	}

	prompts := decodeJSON[promptListResponse](t, h.get(t, "/api/v1/prompts")).Prompts
	if len(prompts) != 2 {
		t.Fatalf("prompts = %+v, want both names", prompts)
	}
	summarize, translate := prompts[0], prompts[1]
	if fmt.Sprint(summarize.Labels) != "map[canary:3 production:1 staging:3]" {
		t.Errorf("summarize labels = %v", summarize.Labels)
	}
	if fmt.Sprint(translate.Labels) != "map[production:1]" {
		t.Errorf("translate labels = %v", translate.Labels)
	}
	if summarize.LatestVersion != 3 || summarize.Type != store.PromptChat {
		t.Errorf("summarize summary = %+v", summarize)
	}
	if translate.LatestVersion != 1 || translate.Type != store.PromptText {
		t.Errorf("translate summary = %+v", translate)
	}
	// updated_at describes the newest version, not whichever row the
	// grouping happened to visit first.
	if summarize.UpdatedAt != third.CreatedAt {
		t.Errorf("updated_at = %q, want the newest version's created_at %q",
			summarize.UpdatedAt, third.CreatedAt)
	}
}

// The #9/#10 guarantee: parallel creates for one name are serialized by the
// writer, so the versions they get are exactly 1..N — no gaps, no duplicates.
func TestPromptConcurrentVersionsAreGapless(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	const parallel = 24
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		assigned []int
	)
	for i := range parallel {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := h.send(t, "POST", "/api/v1/prompts/summarize/versions",
				chatBody(fmt.Sprintf("attempt %d", i), map[string]any{"type": "chat"}))
			if rec.Code != http.StatusCreated {
				t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			assigned = append(assigned, decodeJSON[promptResponse](t, rec).Version)
		}(i)
	}
	wg.Wait()

	sort.Ints(assigned)
	if len(assigned) != parallel {
		t.Fatalf("versions assigned = %d, want %d", len(assigned), parallel)
	}
	for i, version := range assigned {
		if version != i+1 {
			t.Fatalf("versions = %v, want exactly 1..%d", assigned, parallel)
		}
	}

	list := decodeJSON[promptVersionListResponse](t, h.get(t, "/api/v1/prompts/summarize/versions?limit=500"))
	if len(list.Versions) != parallel {
		t.Fatalf("stored versions = %d, want %d", len(list.Versions), parallel)
	}
}

// Parallel moves of one label end on a single winner: uniqueness per
// (project, name, label) is what makes "which version is production" a
// single-row answer (#12).
func TestPromptConcurrentLabelMovesHaveOneWinner(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	const versions = 8
	for i := range versions {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
			chatBody(fmt.Sprintf("version %d", i), map[string]any{"type": "chat"})), http.StatusCreated)
	}

	var wg sync.WaitGroup
	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := h.send(t, "PUT", "/api/v1/prompts/summarize/labels/production",
				map[string]any{"version": i + 1})
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
			}
		}(i)
	}
	wg.Wait()

	winner := decodeJSON[promptResponse](t, h.get(t, "/api/v1/prompts/summarize?label=production"))
	if winner.Version < 1 || winner.Version > versions {
		t.Fatalf("label points at version %d, outside 1..%d", winner.Version, versions)
	}
	summary := decodeJSON[promptListResponse](t, h.get(t, "/api/v1/prompts")).Prompts[0]
	if len(summary.Labels) != 1 || summary.Labels["production"] != winner.Version {
		t.Fatalf("labels = %v, want exactly one row pointing at version %d", summary.Labels, winner.Version)
	}
}

// promptDeletion is the shape of the dry run and of what it becomes
// (spec 021 #7).
type promptDeletion struct {
	DryRun      bool   `json:"dry_run"`
	Name        string `json:"name"`
	WouldDelete struct {
		Versions int `json:"versions"`
		Labels   int `json:"labels"`
	} `json:"would_delete"`
	Deleted bool   `json:"deleted"`
	Confirm string `json:"confirm"`
	Note    string `json:"note"`
}

// Deleting a name is spec 005 #8's ceremony over a prompt's whole history
// (spec 021 #7): a dry run that changes nothing, a wrong echo that changes
// nothing either, and one transaction that takes the versions and the labels
// together.
func TestPromptDelete(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for range 3 {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
			chatBody("You are terse.", nil)), http.StatusCreated)
	}
	expectStatus(t, h.send(t, "PUT", "/api/v1/prompts/summarize/labels/production",
		map[string]any{"version": 2}), http.StatusOK)
	// A second name, to pin that the delete takes one and only one.
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/translate/versions",
		map[string]any{"type": "text", "prompt": "Translate {{input}}."}), http.StatusCreated)
	expectStatus(t, h.send(t, "PUT", "/api/v1/prompts/translate/labels/production",
		map[string]any{"version": 1}), http.StatusOK)

	// A trace that ran the prompt. Its columns are what the client said it
	// ran (spec 012), not a reference into the prompt table.
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "answer", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms,
			PromptName: "summarize", PromptVersion: version(2)})

	dry := decodeJSON[promptDeletion](t, h.send(t, "DELETE", "/api/v1/prompts/summarize", nil))
	if !dry.DryRun || dry.Name != "summarize" || dry.Deleted {
		t.Fatalf("dry run = %+v", dry)
	}
	if dry.WouldDelete.Versions != 3 || dry.WouldDelete.Labels != 1 {
		t.Errorf("would delete = %+v, want three versions and one label", dry.WouldDelete)
	}
	if dry.Confirm != "summarize" || !strings.Contains(dry.Note, "traces") {
		t.Errorf("dry run = %+v, want the echo to ask for and a word about the traces", dry)
	}
	// It changed nothing.
	if versions := decodeJSON[promptVersionListResponse](t,
		h.get(t, "/api/v1/prompts/summarize/versions")); len(versions.Versions) != 3 {
		t.Fatalf("after the dry run there are %d versions, want all three", len(versions.Versions))
	}

	// The echo is checked inside the write transaction, and a wrong one
	// changes nothing either.
	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/summarize?confirm=summarise", nil),
		http.StatusBadRequest, `confirm must be the prompt's name`)
	if versions := decodeJSON[promptVersionListResponse](t,
		h.get(t, "/api/v1/prompts/summarize/versions")); len(versions.Versions) != 3 {
		t.Fatalf("after a wrong echo there are %d versions, want all three", len(versions.Versions))
	}

	gone := decodeJSON[promptDeletion](t, h.send(t, "DELETE", "/api/v1/prompts/summarize?confirm=summarize", nil))
	if gone.DryRun || !gone.Deleted || gone.WouldDelete.Versions != 3 || gone.WouldDelete.Labels != 1 {
		t.Fatalf("deletion = %+v", gone)
	}
	expectError(t, h.get(t, "/api/v1/prompts/summarize"), http.StatusNotFound, "not found")
	expectError(t, h.get(t, "/api/v1/prompts/summarize/versions"), http.StatusNotFound, "not found")
	// The label went with the name rather than outliving it as a row
	// pointing at a version that is not there.
	expectError(t, h.get(t, "/api/v1/prompts/summarize?label=production"), http.StatusNotFound, `no label "production"`)
	names := decodeJSON[promptListResponse](t, h.get(t, "/api/v1/prompts")).Prompts
	if len(names) != 1 || names[0].Name != "translate" || names[0].Labels["production"] != 1 {
		t.Errorf("prompts = %+v, want the other name with its label untouched", names)
	}

	// The trace still records what it ran, and the filter still answers.
	trace := decodeJSON[struct {
		Observations []struct {
			Prompt *struct {
				Name    string `json:"name"`
				Version *int   `json:"version"`
			} `json:"prompt"`
		} `json:"observations"`
	}](t, h.get(t, "/api/v1/traces/"+traceHex(1)))
	if len(trace.Observations) != 1 || trace.Observations[0].Prompt == nil ||
		trace.Observations[0].Prompt.Name != "summarize" {
		t.Errorf("observation after the delete = %+v, want its prompt columns intact", trace.Observations)
	}
	if rows := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}](t, h.get(t, "/api/v1/traces?prompt=summarize")); len(rows.Traces) != 1 {
		t.Errorf("?prompt=summarize = %+v after the delete, want the trace that ran it", rows.Traces)
	}

	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/summarize", nil), http.StatusNotFound, "not found")
	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/unknown?confirm=unknown", nil),
		http.StatusNotFound, "not found")
	expectError(t, h.send(t, "DELETE", "/api/v1/prompts/-bad", nil), http.StatusBadRequest, "prompt name")
}

// Both prompt listings page in both directions (spec 021 #11), which is what
// lets the interface's « ‹ › » cost what one page costs here too.
func TestPromptListingsPageBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, name := range []string{"alpha", "beta", "gamma"} {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/"+name+"/versions",
			chatBody("You are "+name, nil)), http.StatusCreated)
	}
	for range 2 {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/alpha/versions",
			chatBody("again", nil)), http.StatusCreated)
	}

	// With no cursor, `prev` is the far end: the last name alphabetically,
	// and the oldest version of a name.
	end := decodeJSON[promptListResponse](t, h.get(t, "/api/v1/prompts?limit=1&direction=prev"))
	if len(end.Prompts) != 1 || end.Prompts[0].Name != "gamma" {
		t.Fatalf("the far end of the prompt listing = %+v, want gamma", end.Prompts)
	}
	if end.PrevCursor == nil || end.NextCursor != nil {
		t.Errorf("far-end cursors = %v / %v, want a page before it and none after", end.PrevCursor, end.NextCursor)
	}
	back := decodeJSON[promptListResponse](t,
		h.get(t, "/api/v1/prompts?limit=1&direction=prev&cursor="+*end.PrevCursor))
	if len(back.Prompts) != 1 || back.Prompts[0].Name != "beta" {
		t.Errorf("one page back from gamma = %+v, want beta", back.Prompts)
	}

	oldest := decodeJSON[promptVersionListResponse](t,
		h.get(t, "/api/v1/prompts/alpha/versions?limit=2&direction=prev"))
	if len(oldest.Versions) != 2 || oldest.Versions[0].Version != 2 || oldest.Versions[1].Version != 1 {
		t.Fatalf("the oldest page of versions = %+v, want [2 1] — newest first within the page", oldest.Versions)
	}
	if oldest.PrevCursor == nil {
		t.Fatalf("the oldest page names no page before it")
	}
	newer := decodeJSON[promptVersionListResponse](t,
		h.get(t, "/api/v1/prompts/alpha/versions?limit=2&direction=prev&cursor="+*oldest.PrevCursor))
	if len(newer.Versions) != 1 || newer.Versions[0].Version != 3 {
		t.Errorf("one page towards the newest = %+v, want [3]", newer.Versions)
	}

	// The forward walk still says what it said, and the two agree about the
	// cursors between them.
	first := decodeJSON[promptVersionListResponse](t, h.get(t, "/api/v1/prompts/alpha/versions?limit=2"))
	if first.NextCursor == nil || first.PrevCursor != nil {
		t.Errorf("first page cursors = %v / %v", first.PrevCursor, first.NextCursor)
	}
	expectError(t, h.get(t, "/api/v1/prompts?direction=sideways"), http.StatusBadRequest, "direction must be")
	expectError(t, h.get(t, "/api/v1/prompts/alpha/versions?direction=sideways"),
		http.StatusBadRequest, "direction must be")
}

// The credential story is the one ingest uses (#2).
func TestPromptRequiresCredentials(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	strip := func(r *http.Request) { r.Header.Del("Authorization") }
	expectError(t, h.call(t, "GET", "/api/v1/prompts", nil, strip), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.call(t, "POST", "/api/v1/prompts/summarize/versions", []byte(`{}`), strip),
		http.StatusUnauthorized, "unauthorized")
}
