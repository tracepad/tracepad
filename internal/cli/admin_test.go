package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The administrative commands (spec 005). What is worth testing here is not
// the rendering but the ceremony: a destructive command must show what it
// would do and refuse to do it until it is told to, in both the interactive
// and the scripted shape.

const testAdminToken = "tp-admin-secret"

func newAdminCLI(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithToken(t, testAdminToken)
}

// asAdmin points the next commands at the cross-project token, which is sent
// in the same header as a key.
func (h *harness) asAdmin() { h.env["TRACEPAD_API_KEY"] = testAdminToken }

func TestProjectsListAndShow(t *testing.T) {
	h := newAdminCLI(t)

	out := h.run(t.Context(), true, "projects", "ls")
	if out.code != ExitOK {
		t.Fatalf("projects ls exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "test") || !strings.Contains(out.stdout, "forever") {
		t.Errorf("projects ls printed:\n%s\nwant the project and its window", out.stdout)
	}

	// With no --project, one reachable project answers the question itself,
	// and says what the key it was asked with may do (spec 045 #12, #15).
	out = h.run(t.Context(), true, "projects", "show")
	if out.code != ExitOK {
		t.Fatalf("projects show exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "kept forever") {
		t.Errorf("projects show printed:\n%s\nwant the retention policy spelled out", out.stdout)
	}
	if !strings.Contains(out.stdout, "key         tp-pk-test: ingest, read, write") {
		t.Errorf("projects show printed:\n%s\nwant the key's scopes", out.stdout)
	}

	// Piped, it is the API's own bytes (spec 004 #1).
	out = h.run(t.Context(), false, "projects", "ls")
	if !json.Valid([]byte(out.stdout)) {
		t.Errorf("piped output is not JSON:\n%s", out.stdout)
	}
}

// TestProjectsShowNamesTheProjectOnce refuses the form that names it twice.
//
// The positional and --project are two spellings of one argument, and given
// both the positional silently won. That is the shape addWalk refuses (spec
// 003 #23): a command that cannot mean what it says answers with an error
// rather than by picking. Including when the two agree — the ambiguity is in
// the form, and a command that is right by accident when they match teaches a
// rule that breaks the first time they do not.
func TestProjectsShowNamesTheProjectOnce(t *testing.T) {
	h := newAdminCLI(t)
	id := h.projectID(t)

	// The empty value is in the list because `--project "$PROJ"` with nothing
	// in PROJ is how the double naming actually arrives, and a guard that read
	// it as "no flag" would let the positional win after all — the shape of
	// spec 003 #23 (found in review of this PR).
	for _, flagValue := range []string{id, "some-other-project", ""} {
		out := h.run(t.Context(), true, "projects", "show", id, "--project", flagValue)
		if out.code != ExitUsage {
			t.Fatalf("projects show <id> --project %q exited %d, want %d: %s",
				flagValue, out.code, ExitUsage, out.stderr)
		}
		if !strings.Contains(out.stderr, "not both") {
			t.Errorf("stderr = %q, want it to say the two forms are one argument", out.stderr)
		}
	}

	// And on its own that expansion asked about whichever project the key
	// reaches rather than about the one the caller meant to name.
	out := h.run(t.Context(), true, "projects", "show", "--project", "")
	if out.code != ExitUsage {
		t.Fatalf("projects show --project \"\" exited %d, want %d: %s",
			out.code, ExitUsage, out.stderr)
	}
	if !strings.Contains(out.stderr, "--project") {
		t.Errorf("stderr = %q, want it to name the flag that came without an id", out.stderr)
	}

	// The positional spelling of the same expansion, which is this command's
	// alone: an argument that is there and names nothing (found in review of
	// PR #26). It cannot be answered by "the project this key reaches" —
	// that is what passing no argument means, and the two have to stay
	// different questions.
	out = h.run(t.Context(), true, "projects", "show", "")
	if out.code != ExitUsage {
		t.Fatalf("projects show \"\" exited %d, want %d: %s",
			out.code, ExitUsage, out.stderr)
	}
	if !strings.Contains(out.stderr, "empty") {
		t.Errorf("stderr = %q, want it to say the id it was given is empty", out.stderr)
	}

	// Each on its own still works, which is what makes the refusal about the
	// combination and not about either spelling.
	for _, args := range [][]string{{"projects", "show", id},
		{"projects", "show", "--project", id}} {
		out := h.run(t.Context(), true, args...)
		if out.code != ExitOK {
			t.Fatalf("%v exited %d: %s", args, out.code, out.stderr)
		}
	}
}

// TestEmptyProjectFlagIsRefusedEverywhere: `--project ""` is what an unset
// shell variable expands to, and every command that takes the flag used to
// read it as "no --project" and fall back to the one project the key reaches.
// That answers a wider question than was asked — spec 003 #23's case, named
// there in as many words — so it is a usage error on all of them.
//
// The destructive ones are in the list on purpose: the refusal has to come
// before the request, or a `retention set --days 1 --project "$UNSET" --yes`
// shortens a window on whichever project the key happens to reach.
func TestEmptyProjectFlagIsRefusedEverywhere(t *testing.T) {
	h := newAdminCLI(t)

	for _, args := range [][]string{
		{"projects", "show"},
		{"keys", "ls"},
		{"keys", "create", "--scope", "read"},
		{"keys", "rm", "tp-pk-test"},
		{"retention", "show"},
		{"retention", "set", "--days", "30", "--yes"},
		{"users", "rm-data", "user-4711", "--yes"},
	} {
		name := strings.Join(args[:2], " ")
		t.Run(name, func(t *testing.T) {
			out := h.run(t.Context(), true, append(args, "--project", "")...)
			if out.code != ExitUsage {
				t.Fatalf("%v --project \"\" exited %d, want %d: %s",
					args, out.code, ExitUsage, out.stderr)
			}
			if !strings.Contains(out.stderr, "--project") {
				t.Errorf("stderr = %q, want it to name the flag that came without an id",
					out.stderr)
			}
		})
	}

	// Nothing was destroyed on the way: the refusal happened before the
	// requests those two commands would otherwise have made.
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.RetentionDays != nil {
		t.Errorf("retention = %v, want the window untouched", project.RetentionDays)
	}
	keys, err := h.store.ProjectKeys(t.Context(), h.projectID(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("keys = %+v, want the one seeded pair still there", keys)
	}
}

// TestRetentionSetIsADryRunUntilConfirmed is the CLI half of Decision 8. A
// window that shrinks is previewed and not applied; --yes is what applies it,
// and it still sends back the confirm value the server named.
func TestRetentionSetIsADryRunUntilConfirmed(t *testing.T) {
	h := newAdminCLI(t)
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	// Non-interactive and unconfirmed: nothing happens, and the reason is
	// on stderr where a script's operator will find it.
	out := h.run(t.Context(), false, "retention", "set", "--days", "30")
	if out.code != ExitFailure {
		t.Fatalf("unconfirmed retention set exited %d, want 1: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "--yes") {
		t.Errorf("stderr = %q, want it to name the flag that goes ahead", out.stderr)
	}
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.RetentionDays != nil {
		t.Fatalf("the window moved to %d without a confirmation", *project.RetentionDays)
	}

	out = h.run(t.Context(), false, "retention", "set", "--days", "30", "--yes")
	if out.code != ExitOK {
		t.Fatalf("retention set --yes exited %d: %s", out.code, out.stderr)
	}
	project, _ := h.store.ProjectByName(t.Context(), "test")
	if project.RetentionDays == nil || *project.RetentionDays != 30 {
		t.Fatalf("retention = %v, want 30", project.RetentionDays)
	}
	// The preview is still shown, on stderr: --yes replaces the typing,
	// not the account of what is about to happen.
	if !strings.Contains(out.stderr, "traces") {
		t.Errorf("stderr = %q, want the preview of what the shorter window costs", out.stderr)
	}

	// Growing it back needs no ceremony at all: nothing is destroyed.
	out = h.run(t.Context(), true, "retention", "set", "--forever")
	if out.code != ExitOK {
		t.Fatalf("retention set --forever exited %d: %s", out.code, out.stderr)
	}
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.RetentionDays != nil {
		t.Errorf("retention = %v, want forever", project.RetentionDays)
	}
	if strings.Contains(out.stderr, "would") {
		t.Errorf("a window that grows was previewed: %q", out.stderr)
	}

	// The media setting alone (spec 041 #6) needs no confirmation and
	// changes no window: the footer says when it applies instead.
	out = h.run(t.Context(), true, "retention", "set", "--media", "placeholder")
	if out.code != ExitOK {
		t.Fatalf("retention set --media exited %d: %s", out.code, out.stderr)
	}
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.Media != "placeholder" {
		t.Errorf("media = %q, want placeholder", project.Media)
	}
	if strings.Contains(out.stdout, "sweep") || !strings.Contains(out.stdout, "from the next export") {
		t.Errorf("stdout = %q, want the media footer and no window's", out.stdout)
	}
}

// TestInteractiveConfirmationTypesTheName: on a terminal the ceremony is
// typing the name back, which is what makes a confirmed request
// self-describing (#8). Anything else aborts and changes nothing.
func TestInteractiveConfirmationTypesTheName(t *testing.T) {
	h := newAdminCLI(t)

	h.stdin = "nope\n"
	out := h.run(t.Context(), true, "retention", "set", "--days", "30")
	if out.code != ExitFailure {
		t.Fatalf("a wrong confirmation exited %d, want 1", out.code)
	}
	if !strings.Contains(out.stderr, "nothing was done") {
		t.Errorf("stderr = %q, want it to say nothing happened", out.stderr)
	}
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.RetentionDays != nil {
		t.Fatalf("a wrong confirmation changed the window anyway")
	}

	h.stdin = "test\n"
	out = h.run(t.Context(), true, "retention", "set", "--days", "30")
	if out.code != ExitOK {
		t.Fatalf("a correct confirmation exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, `type "test" to confirm`) {
		t.Errorf("stderr = %q, want the prompt to name what to type", out.stderr)
	}
	project, _ := h.store.ProjectByName(t.Context(), "test")
	if project.RetentionDays == nil || *project.RetentionDays != 30 {
		t.Fatalf("retention = %v, want 30", project.RetentionDays)
	}
}

// TestKeyRotationThroughTheCLI: several pairs, so create → move → revoke never
// leaves a gap, and revoking the last one asks first. The commands take the
// admin token: no project key manages keys (spec 045 #4).
func TestKeyRotationThroughTheCLI(t *testing.T) {
	h := newAdminCLI(t)
	h.asAdmin()

	out := h.run(t.Context(), false, "keys", "create", "--scope", "ingest,read,write", "--name", "checkout api")
	if out.code != ExitOK {
		t.Fatalf("keys create exited %d: %s", out.code, out.stderr)
	}
	minted := struct {
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
		Name      string `json:"name"`
	}{}
	if err := json.Unmarshal([]byte(out.stdout), &minted); err != nil {
		t.Fatalf("keys create did not answer with JSON: %v (%s)", err, out.stdout)
	}
	if minted.SecretKey == "" || minted.Name != "checkout api" {
		t.Fatalf("keys create = %s, want a whole, named pair", out.stdout)
	}

	// Who made each key and whether it is in use, in columns (spec 045 #15).
	out = h.run(t.Context(), true, "keys", "ls")
	for _, want := range []string{
		"PUBLIC KEY", "NAME", "SCOPES", "CREATED BY", "LAST USED",
		"tp-pk-test", "server", minted.PublicKey, "checkout api", "admin token", "ingest,read,write",
	} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("keys ls printed:\n%s\nwant %q", out.stdout, want)
		}
	}
	// Every command so far went with the token, so no key has
	// authenticated anything yet.
	if !strings.Contains(out.stdout, "never") {
		t.Errorf("keys ls printed:\n%s\nwant a key never used", out.stdout)
	}

	// Revoking one of two is not destructive enough to ask about.
	out = h.run(t.Context(), true, "keys", "rm", "tp-pk-test")
	if out.code != ExitOK {
		t.Fatalf("keys rm exited %d: %s", out.code, out.stderr)
	}

	// The last one is.
	out = h.run(t.Context(), false, "keys", "rm", minted.PublicKey)
	if out.code != ExitFailure {
		t.Fatalf("revoking the last key exited %d, want a refusal", out.code)
	}
	if !strings.Contains(out.stderr, "last key") {
		t.Errorf("stderr = %q, want it to say this is the last key", out.stderr)
	}

	out = h.run(t.Context(), false, "keys", "rm", minted.PublicKey, "--yes")
	if out.code != ExitOK {
		t.Fatalf("keys rm --yes exited %d: %s", out.code, out.stderr)
	}
	keys, err := h.store.ProjectKeys(t.Context(), h.projectID(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("keys = %+v, want none left", keys)
	}
}

// TestKeysCreateAsksWhatTheKeyMayDo: without --scope the mint is a usage
// error naming the three, before any request; with it the words are sent as
// given, and what is printed follows what the key may do (spec 045 #14, #15).
func TestKeysCreateAsksWhatTheKeyMayDo(t *testing.T) {
	h := newAdminCLI(t)
	h.asAdmin()
	for _, args := range [][]string{{"keys", "create"}, {"keys", "create", "--scope", ""},
		{"keys", "create", "--scope", " , "}, {"keys", "create", "--name", "no scope"}} {
		out := h.run(t.Context(), true, args...)
		if out.code != ExitUsage {
			t.Errorf("%q exited %d, want %d: %s", args, out.code, ExitUsage, out.stderr)
		}
		for _, word := range []string{"--scope", "ingest", "read", "write"} {
			if !strings.Contains(out.stderr, word) {
				t.Errorf("%q: stderr = %q, want it to name %s", args, out.stderr, word)
			}
		}
	}
	if keys, _ := h.store.ProjectKeys(t.Context(), h.projectID(t)); len(keys) != 1 {
		t.Fatalf("keys = %+v, want only the harness's: a usage error mints nothing", keys)
	}

	out := h.run(t.Context(), false, "keys", "create", "--scope", "read, ingest")
	var minted struct {
		PublicKey string   `json:"public_key"`
		Scopes    []string `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &minted); err != nil || out.code != ExitOK {
		t.Fatalf("keys create --scope read,ingest = %d %q %v", out.code, out.stdout, err)
	}
	if strings.Join(minted.Scopes, ",") != "ingest,read" {
		t.Errorf("scopes = %v, want both, in the server's order", minted.Scopes)
	}

	// Repeated, the flag adds rather than replaces: a second --scope that
	// silently dropped the first would mint a key without ingest.
	out = h.run(t.Context(), false, "keys", "create", "--scope", "ingest", "--scope", "write")
	if err := json.Unmarshal([]byte(out.stdout), &minted); err != nil || out.code != ExitOK {
		t.Fatalf("keys create --scope ingest --scope write = %d %q %v", out.code, out.stdout, err)
	}
	if strings.Join(minted.Scopes, ",") != "ingest,write" {
		t.Errorf("a repeated --scope minted %v, want both", minted.Scopes)
	}

	// An ingest key is offered to an exporter; a read key is not.
	out = h.run(t.Context(), true, "keys", "create", "--scope", "ingest")
	if !strings.Contains(out.stdout, "TRACEPAD_API_KEY=") || !strings.Contains(out.stdout, "LANGFUSE_SECRET_KEY=") {
		t.Errorf("an ingest key printed:\n%s\nwant the package and Langfuse lines", out.stdout)
	}
	out = h.run(t.Context(), true, "keys", "create", "--scope", "read")
	if !strings.Contains(out.stdout, "TRACEPAD_API_KEY=") || strings.Contains(out.stdout, "LANGFUSE") ||
		!strings.Contains(out.stdout, "scopes: read") || !strings.Contains(out.stdout, "public key: tp-pk-") {
		t.Errorf("a read key printed:\n%s\nwant its public key, its scope and TRACEPAD_API_KEY alone", out.stdout)
	}

	// A word the server does not know is the server's 400, verbatim.
	out = h.run(t.Context(), true, "keys", "create", "--scope", "admin")
	if out.code != ExitFailure || !strings.Contains(out.stderr, `"scopes" must list`) {
		t.Errorf("--scope admin = %d %q, want the server's refusal", out.code, out.stderr)
	}
}

// TestKeysNeedMoreThanAKey: every `keys` command run with a project key prints
// the server's refusal and exits 1, as every refusal does (spec 045 #4).
func TestKeysNeedMoreThanAKey(t *testing.T) {
	h := newAdminCLI(t)
	for _, args := range [][]string{
		{"keys", "ls"},
		{"keys", "create", "--scope", "ingest", "--name", "mine"},
		{"keys", "rm", "tp-pk-test", "--yes"},
	} {
		out := h.run(t.Context(), false, args...)
		if out.code != ExitFailure {
			t.Errorf("%v with a project key exited %d, want %d", args, out.code, ExitFailure)
		}
		if !strings.Contains(out.stderr, "a project key cannot list, mint or revoke keys") {
			t.Errorf("%v: stderr = %q, want the server's refusal", args, out.stderr)
		}
	}
	if keys, _ := h.store.ProjectKeys(t.Context(), h.projectID(t)); len(keys) != 1 {
		t.Errorf("keys = %+v, want the one there was", keys)
	}
}

// TestProjectDeleteNeedsTheAdminToken is Decision 11 as an operator meets it:
// the project's own key administers everything except the one act with a blast
// radius.
func TestProjectDeleteNeedsTheAdminToken(t *testing.T) {
	h := newAdminCLI(t)
	id := h.projectID(t)

	out := h.run(t.Context(), false, "projects", "rm", id, "--yes")
	if out.code != ExitFailure {
		t.Fatalf("deleting with a project key exited %d, want a refusal", out.code)
	}
	if !strings.Contains(out.stderr, "admin token") {
		t.Errorf("stderr = %q, want it to name the credential that is missing", out.stderr)
	}

	h.asAdmin()
	out = h.run(t.Context(), false, "projects", "rm", id, "--yes")
	if out.code != ExitOK {
		t.Fatalf("projects rm --yes exited %d: %s", out.code, out.stderr)
	}
	project, _ := h.store.ProjectByName(t.Context(), "test")
	if !project.Deleted() {
		t.Fatalf("the project was not deleted")
	}

	// The preview reached the operator before it happened.
	if !strings.Contains(out.stderr, "traces") {
		t.Errorf("stderr = %q, want the preview of what deleting it costs", out.stderr)
	}

	// A deleted project is out of the listing until it is asked for.
	out = h.run(t.Context(), true, "projects", "ls")
	if strings.Contains(out.stdout, id) {
		t.Errorf("projects ls shows the deleted project:\n%s", out.stdout)
	}
	out = h.run(t.Context(), true, "projects", "ls", "--deleted")
	if !strings.Contains(out.stdout, "deleted, purged") {
		t.Errorf("projects ls --deleted printed:\n%s\nwant the purge date", out.stdout)
	}

	// Undoing it is an owner's, not a key's (spec 028 #3). Spec 005 #10
	// gave the project's own key the exception because a token-less
	// deployment had no other credential; with accounts there is always an
	// owner, and a key in an application's config is exactly what must not
	// be able to move a project in or out of existence.
	h.env["TRACEPAD_API_KEY"] = testKey
	out = h.run(t.Context(), false, "projects", "restore", id)
	if out.code != ExitFailure {
		t.Fatalf("restoring with a project key exited %d, want a refusal", out.code)
	}

	h.asAdmin()
	out = h.run(t.Context(), true, "projects", "restore", id)
	if out.code != ExitOK {
		t.Fatalf("projects restore exited %d: %s", out.code, out.stderr)
	}
	if project, _ := h.store.ProjectByName(t.Context(), "test"); project.Deleted() {
		t.Errorf("the project is still deleted after a restore")
	}
}

// TestUsersRemoveData is the erasure command: the echo is the user id, and the
// output says what happened to the raw archive rather than leaving the
// operator to assume.
func TestUsersRemoveData(t *testing.T) {
	h := newAdminCLI(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "u1"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	h.seed(t, &model.Trace{ID: traceHex(2), UserID: "u2"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	// A run holding one of the user's traces: erasure outranks its pin, and
	// the preview has to name the run that will lose it (spec 014 #14).
	if err := h.writer.Submit(t.Context(), &store.DatasetItemsWrite{
		ProjectID: h.projectID(t), Dataset: "golden", Now: 1,
		Items: []*store.DatasetItemInput{{ID: strings.Repeat("d", 32), Input: []byte(`{}`)},
			// Cut from the user's trace: the erasure takes it, and the
			// preview names the dataset (spec 044 #9).
			{ID: strings.Repeat("c", 32), Input: []byte(`{}`), SourceTraceID: traceHex(1)}},
	}); err != nil {
		t.Fatal(err)
	}
	runID := strings.Repeat("e", 32)
	if err := h.writer.Submit(t.Context(), &store.RunCreate{
		ProjectID: h.projectID(t), Dataset: "golden", ID: runID, Now: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.seed(t, &model.Trace{ID: traceHex(3), UserID: "u1", RunID: runID},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(3), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	h.stdin = "u1\n"
	out := h.run(t.Context(), true, "users", "rm-data", "u1")
	if out.code != ExitOK {
		t.Fatalf("users rm-data exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, `type "u1" to confirm`) {
		t.Errorf("stderr = %q, want the user id as the echo, not the project name", out.stderr)
	}
	if !strings.Contains(out.stderr, runID) || !strings.Contains(out.stderr, "golden") {
		t.Errorf("stderr = %q, want the run that loses a trace named before the echo", out.stderr)
	}
	if !strings.Contains(out.stderr, "golden loses 1 items") {
		t.Errorf("stderr = %q, want the dataset that loses an item named before the echo", out.stderr)
	}
	if !strings.Contains(out.stdout, "removed 0 spans from 0 raw batches, 0 deleted") {
		t.Errorf("stdout = %q, want what the raw archive lost stated", out.stdout)
	}
	if !regexp.MustCompile(`dataset_items\s+1`).MatchString(out.stdout) {
		t.Errorf("stdout = %q, want the dataset item counted", out.stdout)
	}
	// And when the bytes the rows left in the file are overwritten (spec 044
	// #11).
	if !strings.Contains(out.stdout, "freed bytes are overwritten by the next sweep, expected by") {
		t.Errorf("stdout = %q, want the compaction's date", out.stdout)
	}

	counts, err := h.store.TableCounts(t.Context(), h.projectID(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range counts {
		if count.Table == "traces" && count.Rows != 1 {
			t.Errorf("traces = %d, want the other user's one left", count.Rows)
		}
	}
}

// TestAdminCommandUsageErrors: a mistyped command is exit 2, not a request.
func TestAdminCommandUsageErrors(t *testing.T) {
	h := newAdminCLI(t)

	for _, args := range [][]string{
		{"projects", "wat"},
		{"keys", "wat"},
		{"retention", "wat"},
		{"users", "wat"},
		{"retention", "set"},
		{"retention", "set", "--days", "30", "--forever"},
		// A window big enough to overflow the cutoff is a typo, not a
		// policy: "forever" is --forever.
		{"retention", "set", "--days", "999999"},
		{"retention", "set", "--raw-days", "999999"},
		{"projects", "rm"},
	} {
		out := h.run(t.Context(), true, args...)
		if out.code != ExitUsage {
			t.Errorf("%v exited %d, want 2 (usage): %s", args, out.code, out.stderr)
		}
	}
}

// A confirmed erasure that outlasts the client's wait is still running on the
// server (spec 035 #14): the command says so and how to see what is left,
// rather than that the server could not be reached. One that never reached
// the server says that, and nothing about an erasure running; a refusal the
// server did send stays that refusal.
func TestAnUnansweredErasureSaysItIsRunning(t *testing.T) {
	ask := func(t *testing.T, baseURL string) error {
		t.Helper()
		api := &client.Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 200 * time.Millisecond}}
		_, err := confirmErasure(t.Context(), "tracepad users rm-data u1", func(ctx context.Context) (json.RawMessage, error) {
			return api.Send(ctx, http.MethodDelete, "/data", url.Values{"confirm": {"u1"}}, nil)
		})
		return err
	}
	running := "runs to the end"

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	err := ask(t, slow.URL)
	if err == nil || !strings.Contains(err.Error(), running) || !strings.Contains(err.Error(), "users rm-data u1") {
		t.Errorf("an erasure that outlasted the wait: %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := ask(t, gone.URL); err == nil || strings.Contains(err.Error(), running) ||
		!strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("a server that was never reached: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"raw batch 3 was rewritten since it was read"}`))
	}))
	defer refusing.Close()
	var refusal *client.Error
	if err := ask(t, refusing.URL); !errors.As(err, &refusal) || refusal.Status != http.StatusConflict ||
		strings.Contains(err.Error(), running) {
		t.Errorf("a refusal became %v", err)
	}

	// A proxy in front of the server that stopped waiting too: the request
	// reached it, and the server behind it runs the erasure on.
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		if err := ask(t, proxy.URL); err == nil || !strings.Contains(err.Error(), running) {
			t.Errorf("a proxy's %d after the request was sent: %v", status, err)
		}
		proxy.Close()
	}
	// The server's own 503 comes before anything is erased.
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"writes are not available"}`))
	}))
	defer unavailable.Close()
	if err := ask(t, unavailable.URL); !errors.As(err, &refusal) || strings.Contains(err.Error(), running) {
		t.Errorf("the server's 503 became %v", err)
	}
}

// A server that predates the raw scrub answers no raw counts, and the command
// says so rather than printing zeros, which would read as an archive checked
// and found clean.
func TestAnErasureAnswerWithoutRawCountsSaysSo(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("confirm") == "" {
			_, _ = w.Write([]byte(`{"dry_run":true,"would_delete":{"traces":1},"confirm":"u1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"dry_run":false,"deleted":{"traces":1,"observations":2},"user_id":"u1"}`))
	}))
	defer old.Close()
	h := newAdminCLI(t)
	out := h.run(t.Context(), true, "users", "rm-data", "--url", old.URL, "--project", "p", "--yes", "u1")
	if out.code != ExitOK {
		t.Fatalf("users rm-data exited %d: %s", out.code, out.stderr)
	}
	if strings.Contains(out.stdout, "removed 0 spans") ||
		!strings.Contains(out.stdout, "the server reported nothing about its raw archive") {
		t.Errorf("stdout = %q", out.stdout)
	}
}

// The command the unanswered erasure suggests asks the same server about the
// same project and user: the flags the operator gave are repeated, the key
// never is, and a word a shell would split or expand is quoted.
func TestTheRetryHintRepeatsTheProjectAndQuotes(t *testing.T) {
	fs := flag.NewFlagSet("users rm-data", flag.ContinueOnError)
	fs.String("url", "", "")
	fs.String("key", "", "")
	fs.String("project", "", "")
	fs.Bool("yes", false, "")
	if err := fs.Parse([]string{"--project", "shop eu", "--key", "tp-secret", "--url", "https://t.example:4318", "--yes"}); err != nil {
		t.Fatal(err)
	}
	got := previewAgain(fs, "o'brien $HOME")
	want := `tracepad users rm-data --project 'shop eu' --url https://t.example:4318 'o'\''brien $HOME'`
	if got != want {
		t.Errorf("hint = %s\nwant   %s", got, want)
	}
	if bare := previewAgain(flag.NewFlagSet("x", flag.ContinueOnError), "user-4711"); bare != "tracepad users rm-data user-4711" {
		t.Errorf("hint without flags = %s", bare)
	}
	// An id that starts with a dash would be read as a flag.
	if dashed := previewAgain(flag.NewFlagSet("x", flag.ContinueOnError), "-alice"); dashed != "tracepad users rm-data -- -alice" {
		t.Errorf("hint for a dashed id = %s", dashed)
	}
}
