package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
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

// A confirmed erasure whose answer is lost after the request was written may
// have been accepted (spec 047 #18): the command says where to look, rather
// than that the server could not be reached. One that never reached the
// server says that, and nothing about an erasure; a refusal the server did
// send stays that refusal.
func TestALostErasureAnswerPointsAtTheListing(t *testing.T) {
	ask := func(t *testing.T, baseURL string) error {
		t.Helper()
		api := &client.Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 200 * time.Millisecond}}
		_, err := confirmErasure(t.Context(), "tracepad users erasures", func(ctx context.Context) (json.RawMessage, error) {
			return api.Send(ctx, http.MethodDelete, "/data", url.Values{"confirm": {"u1"}}, nil)
		})
		return err
	}
	accepted := "may have accepted the erasure"

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	err := ask(t, slow.URL)
	if err == nil || !strings.Contains(err.Error(), accepted) || !strings.Contains(err.Error(), "tracepad users erasures") {
		t.Errorf("an answer that never came: %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	if err := ask(t, gone.URL); err == nil || strings.Contains(err.Error(), accepted) ||
		!strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("a server that was never reached: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"confirm must be the user id being erased"}`))
	}))
	defer refusing.Close()
	var refusal *client.Error
	if err := ask(t, refusing.URL); !errors.As(err, &refusal) || refusal.Status != http.StatusBadRequest ||
		strings.Contains(err.Error(), accepted) {
		t.Errorf("a refusal became %v", err)
	}

	// A proxy in front of the server that stopped waiting: the request
	// reached it, and the server behind it may have recorded it.
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		if err := ask(t, proxy.URL); err == nil || !strings.Contains(err.Error(), accepted) {
			t.Errorf("a proxy's %d after the request was sent: %v", status, err)
		}
		proxy.Close()
	}
	// The server's own 503 comes before anything is recorded.
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"writes are not available"}`))
	}))
	defer unavailable.Close()
	if err := ask(t, unavailable.URL); !errors.As(err, &refusal) || strings.Contains(err.Error(), accepted) {
		t.Errorf("the server's 503 became %v", err)
	}
}

// fakeErasures is a server whose erasure of u1 answers the confirmed request
// with 202 and then reads as each of states in turn, one per GET.
func fakeErasures(t *testing.T, states ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var reads, blips atomic.Int32
	resource := func(state, phase string, deleted int) string {
		phaseJSON, errorJSON := "null", "null"
		if phase != "" {
			phaseJSON = `"` + phase + `"`
		}
		if state == "failed" {
			errorJSON = `"the disk is full"`
		}
		return fmt.Sprintf(`{"id":"4f0c9d3e8a1b2c3d4e5f60718293a4b5","state":%q,"phase":%s,"user_id":null,`+
			`"dry_run":false,"created_at":"2026-10-02T09:00:00Z","progress":{"traces_at_start":3,"traces_deleted":%d},`+
			`"deleted":{"traces":%d,"raw_spans":%d,"raw_batches_rewritten":1,"raw_batches_deleted":0},`+
			`"compaction":{"requested_at":null,"expected_by":null},"error":%s}`,
			state, phaseJSON, deleted, deleted, 2*deleted, errorJSON)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Query().Get("confirm") == "":
			_, _ = w.Write([]byte(`{"dry_run":true,"would_delete":{"traces":3},"confirm":"u1",` +
				`"running":{"id":"0badc0ffee0badc0ffee0badc0ffee00","state":"running","phase":"parsed"}}`))
		case r.Method == http.MethodDelete:
			if r.URL.Query().Get("wait") != "30" && r.URL.Query().Get("wait") != "" {
				t.Errorf("wait = %q", r.URL.Query().Get("wait"))
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(resource("queued", "", 0)))
		case reads.Load() == 0 && blips.Add(1) <= 2:
			// Two reads the server was too busy for, before any answer.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"the server is busy; retry shortly"}`))
		default:
			n := int(reads.Add(1)) - 1
			state := states[min(n, len(states)-1)]
			phase, deleted := "", 3
			if state == "running" {
				phase, deleted = "parsed", n+1
			}
			_, _ = w.Write([]byte(resource(state, phase, deleted)))
		}
	}))
	t.Cleanup(server.Close)
	return server, &reads
}

// An erasure not over within the wait is watched to its end, its progress on
// stderr one line per change; a failed one exits 1 with its sentence;
// --no-wait prints where to look instead (spec 047 #18).
func TestRemoveDataWatchesTheErasureToItsEnd(t *testing.T) {
	defer func(poll time.Duration) { erasurePoll = poll }(erasurePoll)
	erasurePoll = time.Millisecond
	h := newAdminCLI(t)

	server, _ := fakeErasures(t, "running", "running", "running", "done")
	out := h.run(t.Context(), true, "users", "rm-data", "--url", server.URL, "--project", "p", "--yes", "u1")
	if out.code != ExitOK {
		t.Fatalf("users rm-data exited %d: %s", out.code, out.stderr)
	}
	for _, line := range []string{"an erasure of this user is running: 0badc0ffee0badc0ffee0badc0ffee00 (parsed)",
		"erasing: queued\n", "erasing: parsed 1/3 traces\n", "erasing: parsed 2/3 traces\n",
		"erasing: parsed 3/3 traces\n"} {
		if !strings.Contains(out.stderr, line) {
			t.Errorf("stderr = %q, want %q", out.stderr, line)
		}
	}
	if !strings.Contains(out.stdout, "erased the data of u1") ||
		!strings.Contains(out.stdout, "removed 6 spans from 1 raw batches, 0 deleted") {
		t.Errorf("stdout = %q, want the end's counts", out.stdout)
	}

	server, _ = fakeErasures(t, "running", "failed")
	out = h.run(t.Context(), true, "users", "rm-data", "--url", server.URL, "--project", "p", "--yes", "u1")
	if out.code != ExitFailure || !strings.Contains(out.stderr, "could not finish — the disk is full") {
		t.Errorf("a failed erasure exited %d: %s", out.code, out.stderr)
	}

	server, reads := fakeErasures(t, "done")
	out = h.run(t.Context(), true, "users", "rm-data", "--url", server.URL, "--project", "p", "--yes",
		"--no-wait", "u1")
	if out.code != ExitOK || reads.Load() != 0 ||
		out.stdout != "erasure 4f0c9d3e8a1b2c3d4e5f60718293a4b5 queued; tracepad users erasure --project p --url "+
			server.URL+" 4f0c9d3e8a1b2c3d4e5f60718293a4b5\n" {
		t.Errorf("--no-wait exited %d after %d reads: %q", out.code, reads.Load(), out.stdout)
	}
}

// A watch the server refuses ends with the command that shows the erasure
// later, for the same project on the same server (spec 047 #31).
func TestAWatchThatEndsSaysHowToLookAgain(t *testing.T) {
	defer func(poll time.Duration) { erasurePoll = poll }(erasurePoll)
	erasurePoll = time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && r.URL.Query().Get("confirm") == "":
			_, _ = w.Write([]byte(`{"dry_run":true,"would_delete":{"traces":3},"confirm":"u1"}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"4f0c9d3e8a1b2c3d4e5f60718293a4b5","state":"queued","phase":null,` +
				`"progress":{"traces_at_start":null,"traces_deleted":0},"deleted":{}}`))
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this key may not read erasures"}`))
		}
	}))
	t.Cleanup(server.Close)
	h := newAdminCLI(t)
	out := h.run(t.Context(), true, "users", "rm-data", "--url", server.URL, "--project", "p", "--yes", "u1")
	want := "the erasure goes on on the server: tracepad users erasure --project p --url " + server.URL +
		" 4f0c9d3e8a1b2c3d4e5f60718293a4b5"
	if out.code != ExitFailure || !strings.Contains(out.stderr, want) {
		t.Errorf("a refused watch exited %d: %s\nwant %s", out.code, out.stderr, want)
	}
}

// `users erasure` and `users erasures` read what the server records, the user
// id only while an erasure runs (spec 047 #9, #14).
func TestUsersErasures(t *testing.T) {
	h := newAdminCLI(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "u1"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	out := h.run(t.Context(), true, "users", "erasures")
	if out.code != ExitOK || !strings.Contains(out.stdout, "no erasures") {
		t.Fatalf("users erasures before any exited %d: %q %s", out.code, out.stdout, out.stderr)
	}
	if out = h.run(t.Context(), true, "users", "rm-data", "--yes", "u1"); out.code != ExitOK {
		t.Fatalf("users rm-data exited %d: %s", out.code, out.stderr)
	}
	erasures, err := h.store.Erasures(t.Context(), h.projectID(t))
	if err != nil || len(erasures) != 1 {
		t.Fatalf("erasures = %v, %v", erasures, err)
	}
	id := erasures[0].ID
	out = h.run(t.Context(), true, "users", "erasures")
	if out.code != ExitOK || !strings.Contains(out.stdout, id) || !strings.Contains(out.stdout, "done") ||
		strings.Contains(out.stdout, "u1") {
		t.Errorf("users erasures exited %d: %q, want the ended erasure naming no one", out.code, out.stdout)
	}
	out = h.run(t.Context(), true, "users", "erasure", id)
	if out.code != ExitOK || !strings.Contains(out.stdout, "erasure "+id) ||
		!regexp.MustCompile(`traces\s+1`).MatchString(out.stdout) || strings.Contains(out.stdout, "user ") {
		t.Errorf("users erasure exited %d: %q", out.code, out.stdout)
	}
	if out = h.run(t.Context(), true, "users", "erasure", strings.Repeat("0", 32)); out.code != ExitFailure {
		t.Errorf("an unknown erasure exited %d: %s", out.code, out.stderr)
	}
}

// A server that predates the raw scrub answers no raw counts, and the command
// says so rather than printing zeros, which would read as an archive checked
// and found clean. Nor does it know `wait`, which it refuses before it erases
// anything: the command asks again without it, and reads the answer as the
// end, with or without --no-wait (spec 047 #30).
func TestAnErasureAnswerWithoutRawCountsSaysSo(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Has("wait") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unknown query parameter \"wait\" (accepted: confirm)"}`))
			return
		}
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
	out = h.run(t.Context(), true, "users", "rm-data", "--url", old.URL, "--project", "p", "--yes", "--no-wait", "u1")
	if out.code != ExitOK || !strings.Contains(out.stdout, "erased the data of u1") {
		t.Errorf("--no-wait exited %d: %q %s", out.code, out.stdout, out.stderr)
	}
}

// The command a lost erasure answer suggests asks the same server about the
// same project: the flags the operator gave are repeated, the key never is,
// and a word a shell would split or expand is quoted.
func TestTheRetryHintRepeatsTheProjectAndQuotes(t *testing.T) {
	fs := flag.NewFlagSet("users rm-data", flag.ContinueOnError)
	fs.String("url", "", "")
	fs.String("key", "", "")
	fs.String("project", "", "")
	fs.Bool("yes", false, "")
	if err := fs.Parse([]string{"--project", "shop eu", "--key", "tp-secret", "--url", "https://t.example:4318", "--yes"}); err != nil {
		t.Fatal(err)
	}
	got := erasuresCommand(fs)
	want := `tracepad users erasures --project 'shop eu' --url https://t.example:4318`
	if got != want {
		t.Errorf("hint = %s\nwant   %s", got, want)
	}
	if bare := erasuresCommand(flag.NewFlagSet("x", flag.ContinueOnError)); bare != "tracepad users erasures" {
		t.Errorf("hint without flags = %s", bare)
	}
	// The hint for one erasure carries them too, before its id (#31).
	if one := usersCommand(fs, "erasure", "4f0c"); one !=
		`tracepad users erasure --project 'shop eu' --url https://t.example:4318 4f0c` {
		t.Errorf("hint for one erasure = %s", one)
	}
}

// An erasure's report says what became of the bytes its rows left in the file:
// overwritten by a pass that finished, due by one, or never asked for (spec 044
// #11, #22).
func TestErasureReportSaysWhereItsCompactionIs(t *testing.T) {
	at := func(s string) *string { return &s }
	for _, c := range []struct {
		name                           string
		requested, expected, completed *string
		want                           string
	}{
		{"done", at("2026-10-02T10:00:00Z"), nil, at("2026-10-02T11:00:00Z"), "overwritten by the sweep that finished"},
		{"due", at("2026-10-02T10:00:00Z"), at("2026-10-02T11:00:00Z"), nil, "expected by"},
		{"not asked", nil, nil, nil, "nothing was freed"},
	} {
		var out bytes.Buffer
		r := &run{opt: Options{Stdout: &out, Now: time.Now}}
		var view erasureView
		view.Deleted = map[string]int64{"raw_spans": 0}
		view.Compaction.RequestedAt, view.Compaction.ExpectedBy, view.Compaction.CompletedAt =
			c.requested, c.expected, c.completed
		r.renderErased(view)
		if !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: %q, want %q", c.name, out.String(), c.want)
		}
	}
}
