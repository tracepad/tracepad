package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The eval commands (spec 014, Testing — CLI): golden output for every new
// command in both modes, the two file shapes `push` takes, and the
// confirmation a dataset deletion needs off a terminal.

func cliItemID(n int) string { return fmt.Sprintf("%032x", 0xd000+n) }
func cliRunID(n int) string  { return fmt.Sprintf("%032x", 0xe000+n) }

// pushCases writes a cases file and pushes it, which is how every test here
// gets a dataset.
func (h *harness) pushCases(t *testing.T, name, contents, extension string) result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cases"+extension)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return h.run(t.Context(), true, "datasets", "push", name, "--file", path)
}

const jsonlCases = `{"id": "0000000000000000000000000000d001", "input": {"q": "one"}, "expected_output": {"a": "1"}}
{"id": "0000000000000000000000000000d002", "input": {"q": "two"}}
`

// The whole loop through the command line: push, create, stamp, score, finish,
// show, compare.
func TestEvalLoopOnTheCommandLine(t *testing.T) {
	h := newHarness(t)

	push := h.pushCases(t, "golden", jsonlCases, ".jsonl")
	if push.code != ExitOK || !strings.Contains(push.stdout, "version 1: 2 items changed") {
		t.Fatalf("push = %+v", push)
	}
	// The same file again changes nothing, and says so rather than
	// reporting a version it did not move.
	again := h.pushCases(t, "golden", jsonlCases, ".jsonl")
	if !strings.Contains(again.stdout, "unchanged at version 1") {
		t.Errorf("re-push = %q", again.stdout)
	}

	created := h.run(t.Context(), false, "runs", "create", "golden",
		"--id", cliRunID(1), "--name", "prompt v7")
	var run struct {
		ID             string `json:"id"`
		DatasetVersion int    `json:"dataset_version"`
		Status         string `json:"status"`
	}
	if err := json.Unmarshal([]byte(created.stdout), &run); err != nil {
		t.Fatalf("create --json = %q: %v", created.stdout, err)
	}
	if run.ID != cliRunID(1) || run.DatasetVersion != 1 || run.Status != "running" {
		t.Errorf("created = %+v, want the whole run object a script reads", run)
	}

	// Two attempts arrive as traces stamped with the run and the item.
	h.evalTrace(t, cliRunID(1), cliItemID(1), fmt.Sprintf("%032x", 0xa001))
	h.evalTrace(t, cliRunID(1), cliItemID(2), fmt.Sprintf("%032x", 0xa002))
	h.scoreTrace(t, fmt.Sprintf("%032x", 0xa001), "accuracy", 1)
	h.scoreTrace(t, fmt.Sprintf("%032x", 0xa002), "accuracy", 0)

	finished := h.run(t.Context(), true, "runs", "finish", cliRunID(1))
	if !strings.Contains(finished.stdout, "is finished") {
		t.Errorf("finish = %q", finished.stdout)
	}

	shown := h.run(t.Context(), true, "runs", "show", cliRunID(1))
	for _, want := range []string{
		"golden at version 1, finished",
		"items:  2 of 2 covered, 0 missing, 0 unknown traces",
		"traces: 2, 0 failed, up to 1 attempts per item",
		"accuracy",
	} {
		if !strings.Contains(shown.stdout, want) {
			t.Errorf("show is missing %q:\n%s", want, shown.stdout)
		}
	}

	items := h.run(t.Context(), true, "runs", "show", cliRunID(1), "--items")
	if !strings.Contains(items.stdout, cliItemID(1)) || !strings.Contains(items.stdout, "accuracy=1") {
		t.Errorf("show --items = %q", items.stdout)
	}
}

// scoreTrace writes one score against a trace. Straight through the writer
// rather than the command line: posting scores is not what these tests are
// about, and `tracepad` has no command that writes one.
func (h *harness) scoreTrace(t *testing.T, traceID, name string, value float64) {
	t.Helper()
	score := &store.Score{
		ID: traceID, TraceID: traceID, Name: name, DataType: store.ScoreNumeric,
		Value: &value, Timestamp: seedBase, CreatedAt: seedBase,
	}
	if err := h.writer.Submit(t.Context(),
		&store.ScoreWrite{ProjectID: h.projectID(t), Scores: []*store.Score{score}}); err != nil {
		t.Fatal(err)
	}
}

// evalTrace seeds one attempt of a run.
func (h *harness) evalTrace(t *testing.T, runID, itemID, traceID string) {
	t.Helper()
	h.seed(t, &model.Trace{ID: traceID, Name: "case", RunID: runID, ItemID: itemID},
		&model.Observation{TraceID: traceID, ID: traceID[:16], Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault, Model: "claude-sonnet-5",
			StartTime: seedBase, EndTime: seedBase + 100_000_000,
			Output: map[string]any{"answer": "because"}})
}

// `push` takes a JSON array as well as JSONL: one is edited by hand, the other
// generated, and both are the same batch on the wire.
func TestDatasetsPushTakesBothFileShapes(t *testing.T) {
	h := newHarness(t)
	array := `[{"id": "0000000000000000000000000000d001", "input": 1},
	           {"id": "0000000000000000000000000000d002", "input": 2}]`
	if got := h.pushCases(t, "golden", array, ".json"); got.code != ExitOK ||
		!strings.Contains(got.stdout, "version 1: 2 items changed") {
		t.Fatalf("array push = %+v", got)
	}
	if got := h.pushCases(t, "golden", "", ".jsonl"); got.code != ExitFailure ||
		!strings.Contains(got.stderr, "no cases in it") {
		t.Errorf("empty file = %+v, want a refusal rather than an empty batch", got)
	}
	if got := h.run(t.Context(), true, "datasets", "push", "golden"); got.code != ExitUsage {
		t.Errorf("push without --file = %+v, want a usage error", got)
	}
}

// casesFile writes n cases as a JSONL file; a case with no input is the one
// the server refuses, and `ids` names the cases that carry an id.
func casesFile(t *testing.T, n int, noInput map[int]bool, ids map[int]string) string {
	t.Helper()
	var lines []string
	for i := range n {
		fields := []string{fmt.Sprintf(`"input": %d`, i)}
		if noInput[i] {
			fields = nil
		}
		if id, ok := ids[i]; ok {
			fields = append(fields, fmt.Sprintf(`"id": %q`, id))
		}
		lines = append(lines, "{"+strings.Join(fields, ", ")+"}")
	}
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// smallWrites makes a write carry three cases, so that how a file is split
// takes a handful of cases to test rather than tens of thousands.
func smallWrites(t *testing.T) {
	t.Helper()
	was := itemsPerWrite
	itemsPerWrite = 3
	t.Cleanup(func() { itemsPerWrite = was })
}

// A file longer than a request takes goes as consecutive writes (spec 014
// #34); the answer is the last version and the sum of the changes, in the one
// shape a single write has.
func TestDatasetsPushSendsALongFileInSeveralWrites(t *testing.T) {
	smallWrites(t)
	h := newHarness(t)
	path := casesFile(t, 7, nil, nil)

	got := h.run(t.Context(), true, "datasets", "push", "golden", "--file", path)
	if got.code != ExitOK || !strings.Contains(got.stdout, "version 3: 7 items changed, 7 in the batch, sent as 3 writes") {
		t.Fatalf("push = %+v", got)
	}
	// The cases carry no ids, so the second push adds them all again.
	asJSON := h.run(t.Context(), false, "datasets", "push", "golden", "--file", path)
	written, err := decode[itemsWritten](json.RawMessage(asJSON.stdout))
	if err != nil || len(written.IDs) != 7 || written.Version != 6 || written.Changed != 7 {
		t.Fatalf("json push = %d ids, version %d, changed %d, err %v", len(written.IDs), written.Version, written.Changed, err)
	}
	// A file one write carries is answered as it always was.
	if got := h.run(t.Context(), true, "datasets", "push", "small", "--file", casesFile(t, 2, nil, nil)); !strings.Contains(got.stdout, "version 1: 2 items changed, 2 in the batch\n") {
		t.Errorf("one write = %+v, want no mention of writes", got)
	}
}

// One request refuses an id given twice; split, the second would quietly
// become an edit of the first — so the file is refused before anything is
// sent, and the description with it.
func TestDatasetsPushRefusesARepeatedIDBeforeAnythingIsSent(t *testing.T) {
	smallWrites(t)
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	path := casesFile(t, 5, nil, map[int]string{1: cliItemID(9), 4: cliItemID(9)})

	got := h.run(t.Context(), true, "datasets", "push", "golden", "--file", path, "--description", "changed")
	if got.code != ExitFailure || !strings.Contains(got.stderr, "the case at index 4 repeats id "+cliItemID(9)+" of the case at index 1") {
		t.Fatalf("repeated id = %+v", got)
	}
	shown, err := decode[struct {
		Version int `json:"version"`
	}](json.RawMessage(h.run(t.Context(), false, "datasets", "show", "golden").stdout))
	if err != nil || shown.Version != 1 {
		t.Errorf("version = %d after a refused file, want 1 (err %v)", shown.Version, err)
	}
	if got := h.run(t.Context(), false, "datasets", "ls"); strings.Contains(got.stdout, `"changed"`) {
		t.Errorf("a refused file changed the description: %s", got.stdout)
	}
}

// The server counts its indexes from the first case of the write it was sent,
// and names none for a write of one case: the error says which cases of the
// file the write was, and what is written before it.
func TestDatasetsPushPlacesAFailedWriteInTheFile(t *testing.T) {
	smallWrites(t)
	h := newHarness(t)
	for name, c := range map[string]struct {
		badCase int
		want    []string
	}{
		"in the first write": {1, []string{"item at index 1", "in the write of cases 0–2 of the file, the first of 3 writes; the server refused it, so nothing of the file is written"}},
		"in the second":      {4, []string{"item at index 1", "in the write of cases 3–5 of the file; an index in this message counts from case 3", "the first 3 cases are written, at version 1", "adds the ones without an id a second time"}},
		"alone in the last":  {6, []string{"in the write of case 6 of the file", "the first 6 cases are written, at version 2"}},
	} {
		t.Run(name, func(t *testing.T) {
			path := casesFile(t, 7, map[int]bool{c.badCase: true}, nil)
			got := h.run(t.Context(), true, "datasets", "push", "fail-"+strings.ReplaceAll(name, " ", "-"), "--file", path)
			if got.code != ExitFailure {
				t.Fatalf("push = %+v", got)
			}
			for _, want := range c.want {
				if !strings.Contains(got.stderr, want) {
					t.Errorf("stderr = %q, want it to say %q", got.stderr, want)
				}
			}
		})
	}
	// A file one write carries is refused as the server says it, and no more.
	got := h.run(t.Context(), true, "datasets", "push", "fail-small", "--file", casesFile(t, 2, map[int]bool{1: true}, nil))
	if got.code != ExitFailure || strings.Contains(got.stderr, "of the file") {
		t.Errorf("one write = %+v, want the server's own error", got)
	}
}

// What the error says of the write that failed depends on what came back: a
// 4xx is the server refusing it whole, and a connection that dropped or timed
// out is a write that may have been committed — and the message that says
// nothing is written must not be said of that.
func TestPushFailureSaysWhatItKnowsOfTheFailingWrite(t *testing.T) {
	refused := &client.Error{Status: 400, Message: "item at index 1: \"input\" is required"}
	dropped := errors.New("Post: connection reset by peer")
	unavailable := &client.Error{Status: 503, Message: "storage is temporarily unavailable; retry shortly"}
	const unknown = "whether this write itself landed is not known"
	const nothing = "nothing of the file is written"
	for name, c := range map[string]struct {
		err        error
		start, end int
		want, not  string
	}{
		"refused, the first write":     {refused, 0, 3, nothing, unknown},
		"dropped, the first write":     {dropped, 0, 3, unknown, nothing},
		"unavailable, the first write": {unavailable, 0, 3, unknown, nothing},
		"refused, a later write":       {refused, 3, 6, "the first 3 cases are written, at version 2", unknown},
		"dropped, a later write":       {dropped, 3, 6, unknown, nothing},
		"a later write of one case":    {dropped, 6, 7, "in the write of case 6 of the file", nothing},
	} {
		got := pushFailure(c.err, 3, c.start, c.end, 2).Error()
		if !strings.Contains(got, c.want) || strings.Contains(got, c.not) || !strings.HasPrefix(got, c.err.Error()) {
			t.Errorf("%s: %q, want it to say %q and not %q", name, got, c.want, c.not)
		}
	}
	if got := pushFailure(refused, 1, 0, 2, 0); got != error(refused) {
		t.Errorf("a file one write carried: %v, want the server's own error", got)
	}
}

// The id is read as the server reads it: a key named twice with different case
// is the last, a null is no id, and `ID` is the id.
func TestRefuseRepeatedIDsReadsTheIDAsTheServerDoes(t *testing.T) {
	cases := func(lines ...string) []json.RawMessage {
		var items []json.RawMessage
		for _, line := range lines {
			items = append(items, json.RawMessage(line))
		}
		return items
	}
	for name, c := range map[string]struct {
		items []json.RawMessage
		want  string
	}{
		"a null after the id is none":  {cases(`{"id":"a","input":1}`, `{"id":"a","ID":null,"input":2}`), ""},
		"the last key of a name wins":  {cases(`{"id":"a","input":1}`, `{"ID":"a","id":"b","input":2}`), ""},
		"the id in another case":       {cases(`{"id":"a","input":1}`, `{"ID":"a","input":2}`), "index 1 repeats id a of the case at index 0"},
		"an empty id and a missing id": {cases(`{"id":"","input":1}`, `{"id":"","input":2}`, `{"input":3}`), ""},
	} {
		err := refuseRepeatedIDs(c.items)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// The cap the command splits at is the cap the server takes: one case over it
// is two writes, against a real server, on the real number (spec 014 #34).
func TestDatasetsPushSplitsAtTheNumberTheServerTakes(t *testing.T) {
	h := newHarness(t)
	path := casesFile(t, itemsPerWrite+1, nil, nil)

	got := h.run(t.Context(), true, "datasets", "push", "golden", "--file", path)
	if got.code != ExitOK || !strings.Contains(got.stdout, "version 2: 10001 items changed, 10001 in the batch, sent as 2 writes") {
		t.Fatalf("push = %+v", got)
	}
}

// `datasets show --json` is the dataset's export, so it walks every page: a
// file that looks complete and is not would be worse than no export at all.
func TestDatasetsShowJSONWalksEveryPage(t *testing.T) {
	h := newHarness(t)
	var lines []string
	for i := 1; i <= 3; i++ {
		lines = append(lines, fmt.Sprintf(`{"id": %q, "input": %d}`, cliItemID(i), i))
	}
	h.pushCases(t, "golden", strings.Join(lines, "\n"), ".jsonl")

	got := h.run(t.Context(), false, "datasets", "show", "golden", "--limit", "1")
	if got.code != ExitOK {
		t.Fatalf("show = %+v", got)
	}
	export, err := decode[struct {
		Dataset string `json:"dataset"`
		Version int    `json:"version"`
		Items   []struct {
			ID string `json:"id"`
		} `json:"items"`
	}](json.RawMessage(got.stdout))
	if err != nil {
		t.Fatal(err)
	}
	if export.Dataset != "golden" || export.Version != 1 || len(export.Items) != 3 {
		t.Fatalf("export = %+v, want all three items at one version", export)
	}
	for i, item := range export.Items {
		if item.ID != cliItemID(i+1) {
			t.Errorf("item %d = %s, want %s in first-appearance order", i, item.ID, cliItemID(i+1))
		}
	}
}

// A dataset deletion is the destructive one: the preview names what goes, and
// off a terminal it needs --yes.
func TestDatasetsRemoveNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	h.run(t.Context(), false, "runs", "create", "golden", "--id", cliRunID(1))
	h.evalTrace(t, cliRunID(1), cliItemID(1), fmt.Sprintf("%032x", 0xa001))

	refused := h.run(t.Context(), false, "datasets", "rm", "golden")
	if refused.code != ExitFailure {
		t.Fatalf("rm without --yes = %+v, want a refusal", refused)
	}
	for _, want := range []string{"items          2", "runs           1", "pinned traces  1"} {
		if !strings.Contains(refused.stderr, want) {
			t.Errorf("the preview is missing %q:\n%s", want, refused.stderr)
		}
	}
	if got := h.run(t.Context(), true, "datasets", "ls"); !strings.Contains(got.stdout, "golden") {
		t.Errorf("the dataset went without a confirmation")
	}

	done := h.run(t.Context(), true, "datasets", "rm", "golden", "--yes")
	if done.code != ExitOK || !strings.Contains(done.stdout, "1 trace released") {
		t.Errorf("rm --yes = %+v", done)
	}
	if got := h.run(t.Context(), true, "datasets", "ls"); !strings.Contains(got.stdout, "no datasets") {
		t.Errorf("the dataset survived its deletion: %q", got.stdout)
	}
}

// Removing an item archives it, and the version says so.
func TestDatasetsRemoveItem(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	got := h.run(t.Context(), true, "datasets", "rm-item", "golden", cliItemID(1))
	if got.code != ExitOK || !strings.Contains(got.stdout, "archived "+cliItemID(1)+" at version 2") {
		t.Errorf("rm-item = %+v", got)
	}
}

// The comparison renders the header, the score line and the items that moved;
// --all keeps the ones that did not.
func TestRunsCompareRendersWhatMoved(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	h.pushConfig(t, "accuracy", `{"data_type": "numeric", "direction": "higher"}`)

	for i, values := range [][2]float64{{0.5, 1}, {1, 1}} {
		runID := cliRunID(i + 1)
		h.run(t.Context(), false, "runs", "create", "golden", "--id", runID)
		for item, value := range values {
			traceID := fmt.Sprintf("%032x", 0xa000+(i+1)*16+item)
			h.evalTrace(t, runID, cliItemID(item+1), traceID)
			h.scoreTrace(t, traceID, "accuracy", value)
		}
	}

	got := h.run(t.Context(), true, "runs", "compare", cliRunID(1), cliRunID(2))
	if got.code != ExitOK {
		t.Fatalf("compare = %+v", got)
	}
	for _, want := range []string{
		"golden",
		"1 improved, 0 regressed, 1 same",
		cliItemID(1),
		"improved",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("compare is missing %q:\n%s", want, got.stdout)
		}
	}
	// The item that did not move is out of the way unless asked for.
	if strings.Contains(got.stdout, cliItemID(2)) {
		t.Errorf("an unchanged item was listed without --all:\n%s", got.stdout)
	}
	all := h.run(t.Context(), true, "runs", "compare", cliRunID(1), cliRunID(2), "--all")
	if !strings.Contains(all.stdout, cliItemID(2)) {
		t.Errorf("--all left the unchanged item out:\n%s", all.stdout)
	}

	// A run against itself is refused by the server, and the exit code says
	// "the server said no" rather than "you typed it wrong".
	same := h.run(t.Context(), true, "runs", "compare", cliRunID(1), cliRunID(1))
	if same.code != ExitFailure || !strings.Contains(same.stderr, "with itself") {
		t.Errorf("compare with itself = %+v", same)
	}
}

// pushConfig declares one score config through the command line.
func (h *harness) pushConfig(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := h.run(t.Context(), true, "score-configs", "push", name, "--file", path); got.code != ExitOK {
		t.Fatalf("score-configs push = %+v", got)
	}
}

// The config commands: push, list, show, remove.
func TestScoreConfigCommands(t *testing.T) {
	h := newHarness(t)
	h.pushConfig(t, "accuracy", `{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1}`)
	h.pushConfig(t, "verdict", `{"data_type": "categorical", "categories": ["pass", "fail"]}`)

	listed := h.run(t.Context(), true, "score-configs", "ls")
	for _, want := range []string{"accuracy", "numeric", "higher", "0..1", "pass, fail"} {
		if !strings.Contains(listed.stdout, want) {
			t.Errorf("ls is missing %q:\n%s", want, listed.stdout)
		}
	}
	shown := h.run(t.Context(), true, "score-configs", "show", "accuracy")
	if !strings.Contains(shown.stdout, "accuracy: numeric higher") {
		t.Errorf("show = %q", shown.stdout)
	}
	removed := h.run(t.Context(), true, "score-configs", "rm", "verdict")
	if removed.code != ExitOK || !strings.Contains(removed.stdout, "the scores it admitted stay") {
		t.Errorf("rm = %+v", removed)
	}
	if got := h.run(t.Context(), true, "score-configs", "ls"); strings.Contains(got.stdout, "verdict") {
		t.Errorf("the config survived its removal:\n%s", got.stdout)
	}
}

// Every new command answers in JSON off a terminal, byte for byte what the
// endpoint said (#12).
func TestEvalCommandsAreJSONOffATerminal(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	h.run(t.Context(), false, "runs", "create", "golden", "--id", cliRunID(1))

	for _, args := range [][]string{
		{"datasets", "ls"},
		{"runs", "ls", "golden"},
		{"runs", "ls"},
		{"runs", "show", cliRunID(1)},
		{"runs", "show", cliRunID(1), "--items"},
		{"score-configs", "ls"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got := h.run(t.Context(), false, args...)
			if got.code != ExitOK {
				t.Fatalf("%v = %+v", args, got)
			}
			var any map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got.stdout), &any); err != nil {
				t.Errorf("%v printed something that is not a JSON object: %q", args, got.stdout)
			}
		})
	}
}

// `--version 0` is a version like any other — the dataset before its first
// item — and reading it as "not passed" answers a wider question than was
// asked: the caller gets today's cases where they asked for none (spec 003
// #23, found in review of PR #31).
func TestDatasetsShowTakesVersionZero(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")

	got := h.run(t.Context(), false, "datasets", "show", "golden", "--version", "0")
	if got.code != ExitOK {
		t.Fatalf("show = %+v", got)
	}
	export, err := decode[struct {
		Version int `json:"version"`
		Items   []struct {
			ID string `json:"id"`
		} `json:"items"`
	}](json.RawMessage(got.stdout))
	if err != nil {
		t.Fatal(err)
	}
	if export.Version != 0 || len(export.Items) != 0 {
		t.Errorf("version 0 = %+v, want the dataset before its first item", export)
	}
}

// A table cell cuts on a character, not on a byte, and unwraps a payload the
// server already cut rather than printing the marker's bookkeeping (found in
// review of PR #31).
func TestCompactJSONCutsCleanly(t *testing.T) {
	wide := `{"a":"` + strings.Repeat("日本語", 40) + `"}`
	got := compactJSON(json.RawMessage(wide))
	if !utf8.ValidString(got) {
		t.Errorf("compactJSON split a character: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != 48 {
		t.Errorf("width = %d characters, want 48", n)
	}

	marker, err := json.Marshal(map[string]any{
		"truncated": true, "size": 20000, "preview": `{"answer":"because 0a1b`,
		"trace_id": strings.Repeat("a", 32), "observation_id": strings.Repeat("a", 16),
		"full": "/api/v1/observations/" + strings.Repeat("a", 16) + "/io?trace_id=" + strings.Repeat("a", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	cell := compactJSON(marker)
	if !strings.Contains(cell, `because 0a1b`) || !strings.Contains(cell, "19.5 KiB truncated") {
		t.Errorf("cell = %q, want the preview and the real size", cell)
	}
	if strings.Contains(cell, "truncated\":") || strings.Contains(cell, "observation_id") {
		t.Errorf("cell = %q, want the marker read rather than printed", cell)
	}
}

// An unknown subcommand is a usage error, not a request.
func TestEvalSubcommandsAreStrict(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"datasets", "nope"},
		{"runs", "nope"},
		{"score-configs", "nope"},
	} {
		got := h.run(t.Context(), true, args...)
		if got.code != ExitUsage {
			t.Errorf("%v = %+v, want a usage error", args, got)
		}
	}
}

// `runs ls` with no dataset lists the whole project's runs (spec 016 #2): the
// table gains the dataset column the rows now differ in, and two datasets is
// what makes that visible.
func TestRunsListWithoutADatasetSpansTheProject(t *testing.T) {
	h := newHarness(t)
	h.pushCases(t, "golden", jsonlCases, ".jsonl")
	h.pushCases(t, "silver", jsonlCases, ".jsonl")
	h.run(t.Context(), false, "runs", "create", "golden", "--id", cliRunID(1))
	h.run(t.Context(), false, "runs", "create", "silver", "--id", cliRunID(2))

	got := h.run(t.Context(), true, "runs", "ls")
	if got.code != ExitOK {
		t.Fatalf("runs ls = %+v", got)
	}
	for _, want := range []string{"DATASET", "golden", "silver", cliRunID(1), cliRunID(2)} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("runs ls is missing %q:\n%s", want, got.stdout)
		}
	}
	// Newest first across datasets: the silver run was created second.
	if strings.Index(got.stdout, cliRunID(2)) > strings.Index(got.stdout, cliRunID(1)) {
		t.Errorf("runs ls is not newest first:\n%s", got.stdout)
	}

	// Under one dataset the column would say the same word down the table.
	one := h.run(t.Context(), true, "runs", "ls", "golden")
	if strings.Contains(one.stdout, "DATASET") || strings.Contains(one.stdout, cliRunID(2)) {
		t.Errorf("runs ls golden = %q, want only golden's runs and no dataset column", one.stdout)
	}

	if got := h.run(t.Context(), true, "runs", "ls", "golden", "silver"); got.code != ExitUsage {
		t.Errorf("two datasets = %+v, want a usage error", got)
	}
}
