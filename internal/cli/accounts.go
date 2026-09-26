package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/termsafe"
)

/*
Accounts (spec 028 #16). People sign in and programs use keys, so this is the
command an owner runs from a terminal: invite somebody, give them a project,
take it away, stand somebody up as an owner, remove a person who is gone.

Like the rest of the CLI it is a client of the API and nothing else (spec 004
#1) — every command here is one of the `/api/v1/accounts` routes. Those carry
the `owner` policy, which is an owner's session or the admin token; a terminal
has no cookie, so in practice this command runs on `TRACEPAD_ADMIN_TOKEN`,
passed the way `projects create` takes it. That is also why the token is worth
keeping: `tracepad accounts invite <email>` is the way back in when every
owner's password is lost (Decision 16).

Every command takes the account as `<id|email>`. An email is what a person
knows about somebody and an id is what the routes take, so both are accepted
and `accountID` is where the one becomes the other.
*/

// accountView is an account as the API renders it. `pending` is the server's
// word for "has been invited and has not accepted yet", and it is derived from
// having no password rather than stored, so it can never disagree with itself.
type accountView struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Owner    bool   `json:"owner"`
	Disabled bool   `json:"disabled"`
	Pending  bool   `json:"pending"`
	// LastLoginAt is null until the first sign-in, which decodes to the
	// empty string and prints as "never".
	LastLoginAt string           `json:"last_login_at"`
	CreatedAt   string           `json:"created_at"`
	Projects    []membershipView `json:"projects"`
}

// membershipView is one project an account can reach, with its role there.
type membershipView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// accountListing is what `GET /api/v1/accounts` answers with.
type accountListing struct {
	Accounts []accountView `json:"accounts"`
}

// invitation is the half of a response that is shown once and stored as a
// hash: the link, and how long it is good for.
type invitation struct {
	InviteURL       string `json:"invite_url"`
	InviteExpiresAt string `json:"invite_expires_at"`
	Note            string `json:"note"`
}

func (r *run) accounts(ctx context.Context, args []string) error {
	sub, rest := split(args)
	switch sub {
	case "ls":
		return r.accountsList(ctx, rest)
	case "show":
		return r.accountsShow(ctx, rest)
	case "create":
		return r.accountsCreate(ctx, rest)
	case "invite":
		return r.accountsInvite(ctx, rest)
	case "set":
		return r.accountsSet(ctx, rest)
	case "grant":
		return r.accountsGrant(ctx, rest)
	case "revoke":
		return r.accountsRevoke(ctx, rest)
	case "rm":
		return r.accountsRemove(ctx, rest)
	}
	return usageErrorf(
		"accounts takes ls, show, create, invite, set, grant, revoke or rm, got %q", sub)
}

// accountsList answers "who can see what" in one table, which is why the
// projects are on the row rather than behind a `show`.
func (r *run) accountsList(ctx context.Context, args []string) error {
	fs := r.flags("accounts ls")
	if _, err := r.parse(fs, args, 0); err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/accounts", nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	listing, err := decode[accountListing](body)
	if err != nil {
		return err
	}
	if len(listing.Accounts) == 0 {
		fmt.Fprintln(r.opt.Stdout,
			"no accounts: this server has no owner yet, and prints a setup link at every start")
		return nil
	}
	t := newTable(r.opt.Stdout, "EMAIL", "NAME", "STANDING", "LAST LOGIN", "PROJECTS")
	for _, account := range listing.Accounts {
		t.row(account.Email, orDash(account.Name), accountStanding(account),
			lastLogin(account), accountProjects(account))
	}
	t.flush()
	return nil
}

func (r *run) accountsShow(ctx context.Context, args []string) error {
	fs := r.flags("accounts show")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}

	body, err := r.api.Get(ctx, "/api/v1/accounts/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	answer, err := decode[struct {
		Account accountView `json:"account"`
	}](body)
	if err != nil {
		return err
	}
	renderAccount(r, answer.Account)
	return nil
}

// accountsCreate invites somebody. The link it prints is in this one response
// and in no other, ever — only its hash is stored, the way a project key's
// secret is — so the command says so rather than letting a scrolled-off
// terminal be the surprise.
func (r *run) accountsCreate(ctx context.Context, args []string) error {
	var (
		name     string
		owner    bool
		projects repeated
	)
	fs := r.flags("accounts create")
	fs.StringVar(&name, "name", "", "")
	fs.BoolVar(&owner, "owner", false, "")
	fs.Var(&projects, "project", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	memberships, err := readMemberships(projects)
	if err != nil {
		return err
	}
	// The server refuses this too (422). It is refused here as well because
	// it is a mistake in how the command was typed, and those exit 2: the
	// two flags say opposite things about the same account.
	if owner && len(memberships) > 0 {
		return usageErrorf("an owner has every project; pass --owner or --project, not both")
	}

	request := map[string]any{"email": rest[0], "name": name, "owner": owner}
	if len(memberships) > 0 {
		request["memberships"] = memberships
	}
	body, err := r.api.Send(ctx, http.MethodPost, "/api/v1/accounts", nil, request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	created, err := decode[struct {
		Account accountView `json:"account"`
		invitation
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "account %s created (%s)\n\n",
		termsafe.String(created.Account.Email), termsafe.String(created.Account.ID))
	fmt.Fprintf(r.opt.Stdout, "  %s\n\n", termsafe.String(created.InviteURL))
	fmt.Fprintf(r.opt.Stdout,
		"the invitation link is shown only here; only its hash is stored.\n"+
			"It is good until %s, and it is what sets their password —\n"+
			"which is why the password never passes through your hands.\n",
		shortTime(created.InviteExpiresAt))
	return nil
}

// accountsInvite mints a fresh link for an account that already exists, which
// is also the password reset (Decision 10).
//
// It reads the account first so that it can say **whose** link this is. The
// whole output of this command is a secret about to be carried to a person by
// hand, and `accounts invite 4b1e…` would otherwise hand back a link with
// nothing to check the id against — which is the one mistake worth catching
// before the link is pasted into a chat.
func (r *run) accountsInvite(ctx context.Context, args []string) error {
	fs := r.flags("accounts invite")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}
	who, err := r.readAccount(ctx, id)
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPost,
		"/api/v1/accounts/"+url.PathEscape(id)+"/invite", nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	minted, err := decode[invitation](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "a fresh link for %s\n\n", termsafe.String(named(who)))
	fmt.Fprintf(r.opt.Stdout, "  %s\n\n", termsafe.String(minted.InviteURL))
	fmt.Fprintf(r.opt.Stdout, "the link is shown only here and is good until %s\n",
		shortTime(minted.InviteExpiresAt))
	// The server's own sentence about what this did, rather than a second
	// copy of it here: the rule that the old password keeps working until
	// the link is used is the server's, and one wording cannot drift.
	if minted.Note != "" {
		fmt.Fprintln(r.opt.Stdout, termsafe.Text(minted.Note))
	}
	return nil
}

// accountsSet changes what an account is: its name, its standing, whether it
// can sign in at all.
func (r *run) accountsSet(ctx context.Context, args []string) error {
	var (
		name    string
		owner   bool
		noOwner bool
		disable bool
		enable  bool
	)
	fs := r.flags("accounts set")
	fs.StringVar(&name, "name", "", "")
	fs.BoolVar(&owner, "owner", false, "")
	fs.BoolVar(&noOwner, "no-owner", false, "")
	fs.BoolVar(&disable, "disable", false, "")
	fs.BoolVar(&enable, "enable", false, "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if owner && noOwner {
		return usageErrorf("--owner and --no-owner say different things; pick one")
	}
	if disable && enable {
		return usageErrorf("--disable and --enable say different things; pick one")
	}

	request := map[string]any{}
	// Asked as "was it passed" rather than "is it empty", because clearing
	// the display name is a thing to want and `--name ""` is how it is said
	// (spec 003 #23).
	if wasGiven(fs, "name") {
		request["name"] = name
	}
	switch {
	case owner:
		request["owner"] = true
	case noOwner:
		request["owner"] = false
	}
	switch {
	case disable:
		request["disabled"] = true
	case enable:
		request["disabled"] = false
	}
	if len(request) == 0 {
		return usageErrorf(
			"accounts set needs --name, --owner, --no-owner, --disable or --enable")
	}

	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}
	body, err := r.api.Send(ctx, http.MethodPatch,
		"/api/v1/accounts/"+url.PathEscape(id), nil, request)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	answer, err := decode[struct {
		Account accountView `json:"account"`
	}](body)
	if err != nil {
		return err
	}
	renderAccount(r, answer.Account)
	return nil
}

// accountsGrant gives an account a role in a project, or changes the one it
// has. It is a PUT because saying it twice is the same as saying it once.
func (r *run) accountsGrant(ctx context.Context, args []string) error {
	fs := r.flags("accounts grant")
	rest, err := r.parse(fs, args, 3)
	if err != nil {
		return err
	}
	if !store.ValidRole(rest[2]) {
		return usageErrorf("a role is %s or %s, not %q",
			store.RoleViewer, store.RoleEditor, rest[2])
	}
	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodPut, membershipPath(id, rest[1]), nil,
		map[string]any{"role": rest[2]})
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	answer, err := decode[struct {
		Membership struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"membership"`
	}](body)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.opt.Stdout, "%s is a %s of %s\n",
		rest[0], termsafe.String(answer.Membership.Role), termsafe.String(answer.Membership.Name))
	return nil
}

// accountsRevoke takes one project away from one account, and then says what
// the account can reach — rather than asserting what the request was for.
//
// Removing a membership is idempotent: the endpoint answers `204` whether or
// not there was a row, so "no longer reaches it" would be a sentence this
// command cannot know to be true. It is outright false for an **owner**, who
// has no membership rows and every project there is — the same account the
// mirror `accounts grant` refuses with "an owner has every project" — and for
// a project id that was merely mistyped. Reading the account back is one
// request, and it turns a claim into the answer.
func (r *run) accountsRevoke(ctx context.Context, args []string) error {
	fs := r.flags("accounts revoke")
	rest, err := r.parse(fs, args, 2)
	if err != nil {
		return err
	}
	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}

	body, err := r.api.Send(ctx, http.MethodDelete, membershipPath(id, rest[1]), nil, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(body)
	}
	who, err := r.readAccount(ctx, id)
	if err != nil {
		return err
	}
	renderAccount(r, who)
	return nil
}

// accountsRemove deletes a person who is gone. It takes their memberships,
// sessions and invitations and nothing else.
//
// The ceremony is spec 005 #8's — ask what it would do, then echo back what
// the server named — and the echo here is the **email**, the one thing about
// an account a person means. `--confirm <email>` is that echo typed in advance
// (Decision 16), which is what a script has instead of a terminal; it is not a
// `--yes`, because naming the account is the whole point and a script that
// deletes whichever account the id resolved to is the accident this prevents.
//
// The preview is always asked for first, and what was typed is checked against
// the email the server named rather than sent on to be compared there. The
// server compares the echo exactly, and an email is a case-insensitively
// unique identifier: an account registered as `Helper@Example.com` would
// otherwise refuse `--confirm helper@example.com`, on the one path that has
// nobody to read the refusal. What the caller has to get right is *which
// account*, and case is not that.
func (r *run) accountsRemove(ctx context.Context, args []string) error {
	var confirm string
	fs := r.flags("accounts rm")
	fs.StringVar(&confirm, "confirm", "", "")
	rest, err := r.parse(fs, args, 1)
	if err != nil {
		return err
	}
	if confirm == "" && wasGiven(fs, "confirm") {
		return usageErrorf("--confirm needs the account's email; it was passed empty")
	}
	id, err := r.accountID(ctx, rest[0])
	if err != nil {
		return err
	}
	path := "/api/v1/accounts/" + url.PathEscape(id)

	answer, err := r.api.Send(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return err
	}
	dry, err := decode[preview](answer)
	if err != nil {
		return err
	}
	// The email is the server's, and one line: a newline in it would open
	// a line of its own on the preview (#35).
	shown := termsafe.String(dry.Confirm)
	r.renderPreview(dry, "delete the account "+shown)
	switch {
	case confirm != "":
		if !strings.EqualFold(confirm, dry.Confirm) {
			return fmt.Errorf(
				"--confirm is %q and this account's email is %q; nothing was done",
				confirm, dry.Confirm)
		}
	case r.opt.TTY:
		if err := r.askToConfirm(dry.Confirm); err != nil {
			return err
		}
	default:
		return fmt.Errorf(
			"this would delete the account %s; it was not done. Re-run with --confirm %s",
			shown, shown)
	}

	// The server's own spelling, never one this command made up: what
	// `--confirm` bought is that the caller had to name the account, and the
	// value on the wire is still the one the server just said it wanted.
	confirmed := url.Values{}
	confirmed.Set("confirm", dry.Confirm)
	answer, err = r.api.Send(ctx, http.MethodDelete, path, confirmed, nil)
	if err != nil {
		return err
	}
	if r.wantJSON() {
		return r.emit(answer)
	}
	fmt.Fprintf(r.opt.Stdout,
		"account %s deleted; its memberships, sessions and invitations went with it\n",
		termsafe.String(dry.Confirm))
	return nil
}

// --- Shared -----------------------------------------------------------------

// accountID resolves the `<id|email>` every one of these commands takes.
//
// An `@` is what tells the two apart: an email has one and an id is hex
// (Decision 1). The lookup is the listing, because the API has no
// find-by-email route and inventing one in the client would be logic the API
// does not have (spec 004 #1).
func (r *run) accountID(ctx context.Context, given string) (string, error) {
	if given == "" {
		return "", usageErrorf(
			"name the account: its id, or the email it signs in with")
	}
	if !strings.Contains(given, "@") {
		return given, nil
	}
	body, err := r.api.Get(ctx, "/api/v1/accounts", nil)
	if err != nil {
		return "", err
	}
	listing, err := decode[accountListing](body)
	if err != nil {
		return "", err
	}
	for _, account := range listing.Accounts {
		// Case-insensitively, because that is how the column is unique and
		// how the login form matches it.
		if strings.EqualFold(account.Email, given) {
			return account.ID, nil
		}
	}
	return "", fmt.Errorf("no account signs in as %s", given)
}

func membershipPath(accountID, projectID string) string {
	return "/api/v1/accounts/" + url.PathEscape(accountID) +
		"/projects/" + url.PathEscape(projectID)
}

// readMemberships turns `--project <id>:<role>` into what the endpoint takes.
// One flag carrying the pair rather than two flags beside each other: a
// project without a role is not a membership, and `--project a --role viewer
// --project b --role editor` is a shape the reader has to pair up by eye.
func readMemberships(values []string) ([]map[string]any, error) {
	memberships := make([]map[string]any, 0, len(values))
	for _, value := range values {
		id, role, paired := strings.Cut(value, ":")
		if !paired || id == "" || role == "" {
			return nil, usageErrorf(
				"--project takes <project-id>:<role>, got %q", value)
		}
		if !store.ValidRole(role) {
			return nil, usageErrorf("a role is %s or %s, not %q",
				store.RoleViewer, store.RoleEditor, role)
		}
		memberships = append(memberships,
			map[string]any{"project_id": id, "role": role})
	}
	return memberships, nil
}

// readAccount is one account by id, for the two commands whose own answer does
// not carry it: a revoke and an invitation both come back saying nothing about
// whose they were.
func (r *run) readAccount(ctx context.Context, id string) (accountView, error) {
	body, err := r.api.Get(ctx, "/api/v1/accounts/"+url.PathEscape(id), nil)
	if err != nil {
		return accountView{}, err
	}
	answer, err := decode[struct {
		Account accountView `json:"account"`
	}](body)
	return answer.Account, err
}

// named is how an account is addressed in a sentence: the email, which is what
// it signs in with, and the display name when there is one to tell two
// addresses at the same company apart.
func named(view accountView) string {
	if view.Name == "" {
		return view.Email
	}
	return view.Email + " (" + view.Name + ")"
}

func renderAccount(r *run, view accountView) {
	fmt.Fprintf(r.opt.Stdout, "account %s\n", termsafe.String(view.Email))
	fmt.Fprintf(r.opt.Stdout, "  id          %s\n", termsafe.String(view.ID))
	fmt.Fprintf(r.opt.Stdout, "  name        %s\n", termsafe.String(orDash(view.Name)))
	fmt.Fprintf(r.opt.Stdout, "  standing    %s\n", accountStanding(view))
	fmt.Fprintf(r.opt.Stdout, "  created     %s\n", shortTime(view.CreatedAt))
	fmt.Fprintf(r.opt.Stdout, "  last login  %s\n", lastLogin(view))
	if view.Owner {
		fmt.Fprintln(r.opt.Stdout, "  projects    every project, present and future")
		return
	}
	if len(view.Projects) == 0 {
		fmt.Fprintln(r.opt.Stdout, "  projects    none: this account signs in and sees nothing")
		return
	}
	fmt.Fprintln(r.opt.Stdout, "  projects")
	t := newTable(r.opt.Stdout)
	for _, project := range view.Projects {
		t.row("    "+project.Name, project.Role, project.ID)
	}
	t.flush()
}

// accountStanding is what this account is, in one cell. Disabled and invited
// are said alongside `owner` rather than instead of it, because "owner,
// invited" is the state the last-owner rule is about: an owner only counts
// once they can actually open the door, so inviting a successor is not yet
// standing down (Decision 22).
func accountStanding(view accountView) string {
	standing := "member"
	if view.Owner {
		standing = "owner"
	}
	switch {
	case view.Disabled:
		return standing + ", disabled"
	case view.Pending:
		return standing + ", invited"
	}
	return standing
}

// accountProjects is the listing's answer to "who can see what". An owner has
// no membership rows and every project, which is a sentence rather than a
// blank cell.
func accountProjects(view accountView) string {
	if view.Owner {
		return "every project"
	}
	if len(view.Projects) == 0 {
		return "none"
	}
	named := make([]string, 0, len(view.Projects))
	for _, project := range view.Projects {
		named = append(named, project.Name+" ("+project.Role+")")
	}
	return strings.Join(named, ", ")
}

// lastLogin is "never" for an account that has not signed in, which is the
// state an invitation leaves it in.
func lastLogin(view accountView) string { return timeOrNever(view.LastLoginAt) }
