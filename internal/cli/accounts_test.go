package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// The accounts commands (spec 028 #16), each one against a real server.
//
// What is worth testing here is the same thing the administrative tests are
// about: not the rendering, but that the command means what it says. An
// invitation link is shown once and the command has to say so; a deletion is
// refused until the **email** comes back, and a wrong one is refused by the
// server rather than by a comparison this client made up.

const (
	helperEmail  = "helper@example.com"
	partnerEmail = "partner@example.com"
)

// newAccountsCLI is the admin harness: the account routes carry the `owner`
// policy, and a terminal has no cookie, so the credential is the admin token.
func newAccountsCLI(t *testing.T) *harness {
	t.Helper()
	h := newAdminCLI(t)
	h.asAdmin()
	return h
}

// invite creates an account and returns the link the command printed, which is
// the only place it is ever shown.
func (h *harness) invite(t *testing.T, args ...string) string {
	t.Helper()
	out := h.run(t.Context(), true, append([]string{"accounts", "create"}, args...)...)
	if out.code != ExitOK {
		t.Fatalf("accounts create %v exited %d: %s", args, out.code, out.stderr)
	}
	for _, word := range strings.Fields(out.stdout) {
		if strings.Contains(word, "/invite#token=") {
			return word
		}
	}
	t.Fatalf("accounts create printed no invitation link:\n%s", out.stdout)
	return ""
}

// TestAccountsCreateShowsTheLinkOnce walks an invitation: the account is made
// with a project and a role, the link is printed with the sentence that says
// it will not be printed again, and the account reads back as invited.
func TestAccountsCreateShowsTheLinkOnce(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)

	out := h.run(t.Context(), true, "accounts", "create", helperEmail,
		"--name", "The Helper", "--project", project+":viewer")
	if out.code != ExitOK {
		t.Fatalf("accounts create exited %d: %s", out.code, out.stderr)
	}
	for _, want := range []string{helperEmail, "/invite#token=", "shown only here"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("accounts create printed:\n%s\nwant it to carry %q", out.stdout, want)
		}
	}

	// And by email, which is what the person running this knows about
	// somebody — the id is what the routes take.
	out = h.run(t.Context(), true, "accounts", "show", helperEmail)
	if out.code != ExitOK {
		t.Fatalf("accounts show exited %d: %s", out.code, out.stderr)
	}
	for _, want := range []string{"The Helper", "member, invited", "never", "test", "viewer"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("accounts show printed:\n%s\nwant it to carry %q", out.stdout, want)
		}
	}

	// Piped, every command is the API's own bytes (spec 004 #1).
	out = h.run(t.Context(), false, "accounts", "ls")
	if !json.Valid([]byte(out.stdout)) {
		t.Errorf("piped output is not JSON:\n%s", out.stdout)
	}
}

// TestAccountsListAnswersWhoCanSeeWhat: the projects are on the row, because
// that is the question the listing is opened to answer.
func TestAccountsListAnswersWhoCanSeeWhat(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)

	out := h.run(t.Context(), true, "accounts", "ls")
	if out.code != ExitOK {
		t.Fatalf("accounts ls exited %d: %s", out.code, out.stderr)
	}
	// A server with no owner yet says so rather than printing an empty
	// table: the setup link at startup is what makes the first one.
	if !strings.Contains(out.stdout, "no owner yet") {
		t.Errorf("accounts ls on an empty server printed:\n%s\nwant it to say why", out.stdout)
	}

	h.invite(t, helperEmail, "--project", project+":editor")
	h.invite(t, partnerEmail, "--owner")

	out = h.run(t.Context(), true, "accounts", "ls")
	if out.code != ExitOK {
		t.Fatalf("accounts ls exited %d: %s", out.code, out.stderr)
	}
	for _, want := range []string{
		helperEmail, "test (editor)",
		// An owner has no membership rows and every project, which is a
		// sentence rather than a blank cell (Decision 2).
		partnerEmail, "owner, invited", "every project",
	} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("accounts ls printed:\n%s\nwant it to carry %q", out.stdout, want)
		}
	}
}

// TestAccountsGrantAndRevoke moves one project in and out of one account.
func TestAccountsGrantAndRevoke(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)
	h.invite(t, helperEmail)

	out := h.run(t.Context(), true, "accounts", "grant", helperEmail, project, "editor")
	if out.code != ExitOK {
		t.Fatalf("accounts grant exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "editor") || !strings.Contains(out.stdout, "test") {
		t.Errorf("accounts grant printed:\n%s\nwant the role and the project", out.stdout)
	}

	// Saying it again is the same as saying it once, and it is how a role
	// is changed.
	out = h.run(t.Context(), true, "accounts", "grant", helperEmail, project, "viewer")
	if out.code != ExitOK {
		t.Fatalf("accounts grant (again) exited %d: %s", out.code, out.stderr)
	}
	out = h.run(t.Context(), true, "accounts", "show", helperEmail)
	if !strings.Contains(out.stdout, "viewer") || strings.Contains(out.stdout, "editor") {
		t.Errorf("accounts show printed:\n%s\nwant one role, the newer one", out.stdout)
	}

	out = h.run(t.Context(), true, "accounts", "revoke", helperEmail, project)
	if out.code != ExitOK {
		t.Fatalf("accounts revoke exited %d: %s", out.code, out.stderr)
	}
	out = h.run(t.Context(), true, "accounts", "show", helperEmail)
	if !strings.Contains(out.stdout, "signs in and sees nothing") {
		t.Errorf("accounts show printed:\n%s\nwant the account left with no projects", out.stdout)
	}

	// A revoke answers 204 and no body at all, which piped is nothing —
	// not a blank line, which is not JSON (#12).
	out = h.run(t.Context(), false, "accounts", "revoke", helperEmail, project)
	if out.code != ExitOK {
		t.Fatalf("accounts revoke --json exited %d: %s", out.code, out.stderr)
	}
	if out.stdout != "" {
		t.Errorf("piped accounts revoke printed %q, want nothing", out.stdout)
	}
}

// TestAccountsSetChangesStanding walks the flags of `accounts set`, including
// the two pairs that say opposite things.
func TestAccountsSetChangesStanding(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)
	h.invite(t, helperEmail, "--name", "The Helper", "--project", project+":viewer")

	out := h.run(t.Context(), true, "accounts", "set", helperEmail, "--name", "Helper II")
	if out.code != ExitOK {
		t.Fatalf("accounts set --name exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "Helper II") {
		t.Errorf("accounts set printed:\n%s\nwant the new name", out.stdout)
	}

	// Making somebody an owner takes their memberships away, because an
	// owner has every project (Decision 2).
	out = h.run(t.Context(), true, "accounts", "set", helperEmail, "--owner")
	if out.code != ExitOK {
		t.Fatalf("accounts set --owner exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "every project") {
		t.Errorf("accounts set --owner printed:\n%s\nwant every project", out.stdout)
	}

	// Demoting leaves none, so the account sees nothing until it is given
	// projects again. It is a pending owner, so the last-owner rule does
	// not hold it: an owner only counts once it can open the door.
	out = h.run(t.Context(), true, "accounts", "set", helperEmail, "--no-owner", "--disable")
	if out.code != ExitOK {
		t.Fatalf("accounts set --no-owner --disable exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "member, disabled") {
		t.Errorf("accounts set printed:\n%s\nwant the standing it now has", out.stdout)
	}
	out = h.run(t.Context(), true, "accounts", "set", helperEmail, "--enable")
	if out.code != ExitOK {
		t.Fatalf("accounts set --enable exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "member, invited") {
		t.Errorf("accounts set --enable printed:\n%s\nwant it back to invited", out.stdout)
	}
}

// TestAccountsSetRefusesWhatItCannotMean: two flags that say opposite things
// about one field are a usage error rather than one of them quietly winning
// (spec 003 #23), and a `set` that changes nothing is a command that was
// mistyped.
func TestAccountsSetRefusesWhatItCannotMean(t *testing.T) {
	h := newAccountsCLI(t)
	h.invite(t, helperEmail)

	for name, args := range map[string][]string{
		"owner both ways":   {"accounts", "set", helperEmail, "--owner", "--no-owner"},
		"enabled both ways": {"accounts", "set", helperEmail, "--disable", "--enable"},
		"nothing to change": {"accounts", "set", helperEmail},
	} {
		t.Run(name, func(t *testing.T) {
			out := h.run(t.Context(), true, args...)
			if out.code != ExitUsage {
				t.Fatalf("%v exited %d, want %d: %s", args, out.code, ExitUsage, out.stderr)
			}
		})
	}

	// Clearing the display name is a thing to want, and `--name ""` is how
	// it is said — so an empty value is a value here, not an absence.
	out := h.run(t.Context(), true, "accounts", "set", helperEmail, "--name", "")
	if out.code != ExitOK {
		t.Fatalf("accounts set --name \"\" exited %d: %s", out.code, out.stderr)
	}
}

// TestAccountsCreateRefusesAnOwnerWithProjects and the malformed pairs: what a
// `--project` has to be is a project and a role, and the two flags that say
// opposite things about one account are refused before the request.
func TestAccountsCreateRefusesAnOwnerWithProjects(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)

	for name, args := range map[string][]string{
		"an owner with a project": {"accounts", "create", helperEmail,
			"--owner", "--project", project + ":viewer"},
		"a project with no role": {"accounts", "create", helperEmail, "--project", project},
		"a role nobody has": {"accounts", "create", helperEmail,
			"--project", project + ":annotator"},
		"an empty --project": {"accounts", "create", helperEmail, "--project", ""},
	} {
		t.Run(name, func(t *testing.T) {
			out := h.run(t.Context(), true, args...)
			if out.code != ExitUsage {
				t.Fatalf("%v exited %d, want %d: %s", args, out.code, ExitUsage, out.stderr)
			}
		})
	}

	// None of them made an account.
	out := h.run(t.Context(), true, "accounts", "ls")
	if !strings.Contains(out.stdout, "no owner yet") {
		t.Errorf("accounts ls printed:\n%s\nwant nothing to have been created", out.stdout)
	}
}

// TestAccountsInviteMintsAFreshLink is the password reset (Decision 10). The
// command prints the server's own sentence about what it did, because the rule
// that the old password keeps working is the server's.
func TestAccountsInviteMintsAFreshLink(t *testing.T) {
	h := newAccountsCLI(t)
	first := h.invite(t, helperEmail)

	out := h.run(t.Context(), true, "accounts", "invite", helperEmail)
	if out.code != ExitOK {
		t.Fatalf("accounts invite exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "/invite#token=") {
		t.Errorf("accounts invite printed:\n%s\nwant a link", out.stdout)
	}
	// Whose link it is, which the response itself does not say. The whole
	// output is a secret about to be carried to a person by hand, and by id
	// there would be nothing to check the id against (found in review of
	// PR #52).
	if !strings.Contains(out.stdout, helperEmail) {
		t.Errorf("accounts invite printed:\n%s\nwant the account it is for", out.stdout)
	}
	if strings.Contains(out.stdout, first) {
		t.Errorf("accounts invite printed the earlier link again:\n%s", out.stdout)
	}
	if !strings.Contains(out.stdout, "old password") {
		t.Errorf("accounts invite printed:\n%s\nwant the server's note about the old password",
			out.stdout)
	}
}

// TestAccountsRemoveWantsTheEmailBack is the ceremony of spec 005 #8 with the
// echo Decision 12 chose: the email, the one thing about an account a person
// means.
func TestAccountsRemoveWantsTheEmailBack(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)
	h.invite(t, helperEmail, "--project", project+":viewer")

	// Piped and unconfirmed: the preview on stderr, nothing done, exit 1.
	out := h.run(t.Context(), false, "accounts", "rm", helperEmail)
	if out.code != ExitFailure {
		t.Fatalf("accounts rm exited %d, want %d: %s", out.code, ExitFailure, out.stderr)
	}
	for _, want := range []string{"memberships", "--confirm " + helperEmail} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", out.stderr, want)
		}
	}

	// A different account's email is refused, and the refusal names both:
	// the one that was typed and the one this account actually signs in
	// with. The comparison is against the email the *server* named in its
	// preview, which is also what goes on the wire.
	out = h.run(t.Context(), true, "accounts", "rm", helperEmail, "--confirm", partnerEmail)
	if out.code != ExitFailure {
		t.Fatalf("accounts rm --confirm <wrong> exited %d, want %d: %s",
			out.code, ExitFailure, out.stderr)
	}
	for _, want := range []string{helperEmail, partnerEmail, "nothing was done"} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", out.stderr, want)
		}
	}

	// `--confirm` that arrived without a value is the shape an unset shell
	// variable expands to, and reading it as "no --confirm" would turn a
	// deletion into a preview at the exact moment the script meant it.
	out = h.run(t.Context(), true, "accounts", "rm", helperEmail, "--confirm", "")
	if out.code != ExitUsage {
		t.Fatalf("accounts rm --confirm \"\" exited %d, want %d: %s",
			out.code, ExitUsage, out.stderr)
	}

	// Still there.
	out = h.run(t.Context(), true, "accounts", "show", helperEmail)
	if out.code != ExitOK {
		t.Fatalf("accounts show exited %d: %s", out.code, out.stderr)
	}

	out = h.run(t.Context(), true, "accounts", "rm", helperEmail, "--confirm", helperEmail)
	if out.code != ExitOK {
		t.Fatalf("accounts rm --confirm exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "deleted") {
		t.Errorf("accounts rm printed:\n%s\nwant it to say so", out.stdout)
	}
	out = h.run(t.Context(), true, "accounts", "show", helperEmail)
	if out.code != ExitFailure {
		t.Fatalf("accounts show after rm exited %d, want %d", out.code, ExitFailure)
	}
}

// TestAccountsRemoveListsTheKeysItMinted: the preview names the keys the
// account minted, which the deletion leaves working (spec 045 #10).
func TestAccountsRemoveListsTheKeysItMinted(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)
	h.invite(t, helperEmail, "--project", project+":editor")
	helper, err := h.store.AccountByEmail(t.Context(), helperEmail)
	if err != nil || helper == nil {
		t.Fatalf("the invited account: %v", err)
	}
	// Minted the way a signed-in editor mints, which a terminal cannot be.
	keys, err := store.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.writer.Submit(t.Context(), &store.KeyCreate{
		ProjectID: project, Keys: keys, Name: "helper's script", Origin: store.OriginAccount(helper),
	}); err != nil {
		t.Fatal(err)
	}

	out := h.run(t.Context(), false, "accounts", "rm", helperEmail)
	if out.code != ExitFailure {
		t.Fatalf("accounts rm exited %d, want %d: %s", out.code, ExitFailure, out.stderr)
	}
	for _, want := range []string{"keeps key", keys.PublicKey, "helper's script", "in test", "never",
		"keep working until they are revoked"} {
		if !strings.Contains(out.stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", out.stderr, want)
		}
	}
}

// TestAccountsRemoveTakesTheEmailInAnyCase: an email is a case-insensitively
// unique identifier — that is how the column is unique and how the login form
// matches it — so an account registered as `Helper@Example.com` is named by
// typing it in any case.
//
// The server compares the echo exactly, which is right for the server: what
// reaches it is the spelling it named itself. What the caller has to get right
// is *which account*, and case is not that — and this is the path with nobody
// there to read a refusal (found in review of PR #52).
func TestAccountsRemoveTakesTheEmailInAnyCase(t *testing.T) {
	h := newAccountsCLI(t)
	const mixed = "Helper@Example.com"
	h.invite(t, mixed)

	out := h.run(t.Context(), true, "accounts", "rm", strings.ToLower(mixed),
		"--confirm", strings.ToLower(mixed))
	if out.code != ExitOK {
		t.Fatalf("accounts rm exited %d: %s", out.code, out.stderr)
	}
	// And what it says it deleted is the spelling the account was stored
	// under, not the one that was typed.
	if !strings.Contains(out.stdout, mixed) {
		t.Errorf("accounts rm printed:\n%s\nwant the account's own spelling", out.stdout)
	}
}

// TestAccountsRevokeSaysWhatTheAccountReaches: removing a membership is
// idempotent — the endpoint answers 204 whether or not there was a row — so
// the command reads the account back rather than asserting that access is
// gone. For an owner it is not gone at all: an owner has no membership rows
// and every project there is (found in review of PR #52).
func TestAccountsRevokeSaysWhatTheAccountReaches(t *testing.T) {
	h := newAccountsCLI(t)
	project := h.projectID(t)
	h.invite(t, partnerEmail, "--owner")

	out := h.run(t.Context(), true, "accounts", "revoke", partnerEmail, project)
	if out.code != ExitOK {
		t.Fatalf("accounts revoke exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "every project") {
		t.Errorf("accounts revoke printed:\n%s\n"+
			"want the truth about an owner, which is that it still reaches everything",
			out.stdout)
	}

	// A project the account never had is the same shape: nothing changed,
	// and the command says what is rather than what it did.
	h.invite(t, helperEmail, "--project", project+":viewer")
	out = h.run(t.Context(), true, "accounts", "revoke", helperEmail, "no-such-project")
	if out.code != ExitOK {
		t.Fatalf("accounts revoke <unknown project> exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stdout, "viewer") {
		t.Errorf("accounts revoke printed:\n%s\nwant the membership it still has", out.stdout)
	}
}

// TestAccountsRemoveTypedAtATerminal is the other half of the ceremony: at a
// terminal the echo is typed rather than passed, and typing the wrong thing
// does nothing.
func TestAccountsRemoveTypedAtATerminal(t *testing.T) {
	h := newAccountsCLI(t)
	h.invite(t, helperEmail)

	h.stdin = partnerEmail + "\n"
	out := h.run(t.Context(), true, "accounts", "rm", helperEmail)
	if out.code != ExitFailure {
		t.Fatalf("accounts rm with the wrong echo exited %d, want %d: %s",
			out.code, ExitFailure, out.stderr)
	}
	if !strings.Contains(out.stderr, "nothing was done") {
		t.Errorf("stderr = %q, want it to say nothing happened", out.stderr)
	}

	h.stdin = helperEmail + "\n"
	out = h.run(t.Context(), true, "accounts", "rm", helperEmail)
	if out.code != ExitOK {
		t.Fatalf("accounts rm with the echo exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "this would delete the account") {
		t.Errorf("stderr = %q, want the preview before the question", out.stderr)
	}
}

// TestAccountsNameAnAccountThatIsThere: an email nobody signs in as is a
// failure and not a usage error, and it says which email it was.
func TestAccountsNameAnAccountThatIsThere(t *testing.T) {
	h := newAccountsCLI(t)

	out := h.run(t.Context(), true, "accounts", "show", "nobody@example.com")
	if out.code != ExitFailure {
		t.Fatalf("accounts show exited %d, want %d: %s", out.code, ExitFailure, out.stderr)
	}
	if !strings.Contains(out.stderr, "nobody@example.com") {
		t.Errorf("stderr = %q, want the email it was given", out.stderr)
	}

	// An argument with no `@` is read as an id, which the server answers
	// for: the client does not guess which of the two a word is beyond the
	// one character that tells them apart.
	out = h.run(t.Context(), true, "accounts", "show", "0123456789abcdef")
	if out.code != ExitFailure {
		t.Fatalf("accounts show <id> exited %d, want %d: %s", out.code, ExitFailure, out.stderr)
	}
	if !strings.Contains(out.stderr, "no such account") {
		t.Errorf("stderr = %q, want the server's own answer", out.stderr)
	}
}

// TestAccountsSubcommandIsNamed keeps the dispatch honest: an unknown one says
// what the command takes, and the binary routes every word this package
// implements (spec 020 #14).
func TestAccountsSubcommandIsNamed(t *testing.T) {
	h := newAccountsCLI(t)

	out := h.run(t.Context(), true, "accounts", "list")
	if out.code != ExitUsage {
		t.Fatalf("accounts list exited %d, want %d", out.code, ExitUsage)
	}
	if !strings.Contains(out.stderr, "accounts takes ls") {
		t.Errorf("stderr = %q, want the subcommands it takes", out.stderr)
	}

	var routed bool
	for _, name := range Commands() {
		if name == "accounts" {
			routed = true
		}
	}
	if !routed {
		t.Error("`accounts` is not in Commands(), so the binary would answer unknown command")
	}
}
