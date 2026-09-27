package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Accounts in the store (spec 028, Testing). What is asserted here is what
// only the store can be wrong about: that the migration applies to a database
// that already has projects and keys, that a purge takes the memberships with
// it, that the last-owner invariant holds inside the write transaction, and
// that an expired session is refused before the sweeper reaches it.

// testHash is one bcrypt hash for the whole test binary. The cost is 12 by
// design (Decision 1), which is a quarter of a second: hashing per test would
// be minutes of the suite spent proving the same thing over and over.
var testHash = sync.OnceValues(func() ([]byte, error) { return HashPassword(anySlot(), testAccountPassword) })

const testAccountPassword = "correct horse battery"

func hashOnce(t *testing.T) []byte {
	t.Helper()
	hash, err := testHash()
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// accountFixture opens a store with a writer and one project.
type accountFixture struct {
	*Store
	writer  *Writer
	project *Project
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	st := openFresh(t)
	writer, err := st.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	project, err := st.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return &accountFixture{Store: st, writer: writer, project: project}
}

func (f *accountFixture) submit(t *testing.T, job WriteJob) {
	t.Helper()
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatalf("write failed: %v", err)
	}
}

// invite creates an account and accepts its invitation, which is the only way
// an account gets a password — the same two jobs the endpoints use.
func (f *accountFixture) invite(t *testing.T, email string, owner bool, memberships ...Membership) *Account {
	t.Helper()
	now := time.Now().UnixNano()
	token := "token-for-" + email
	create := &AccountCreate{
		Email: email, Owner: owner, Memberships: memberships,
		TokenID: SessionID(token), ExpiresAt: now + int64(7*24*time.Hour), Now: now,
	}
	f.submit(t, create)
	accept := &InviteAccept{
		SessionSeed: SessionSeed{
			SessionID: SessionID("session-for-" + email),
			ExpiresAt: now + int64(30*24*time.Hour),
			Now:       now,
		},
		TokenID: SessionID(token), NewHash: hashOnce(t),
	}
	f.submit(t, accept)
	return accept.Account
}

// TestMigrationAppliesOverProjectsAndKeys: 0017 runs on a database that
// already holds a project and its key, and a purge cascades the memberships
// (spec 028, Testing #1).
func TestAccountsMigrationAndPurgeCascade(t *testing.T) {
	f := newAccountFixture(t)
	account := f.invite(t, "helper@example.com", false,
		Membership{ProjectID: f.project.ID, Role: RoleViewer})

	memberships, err := f.Memberships(t.Context(), account.ID)
	if err != nil || len(memberships) != 1 || memberships[0].Name != "test" {
		t.Fatalf("memberships = %+v, err = %v", memberships, err)
	}

	// The purge drops the project row; the membership goes with it, and the
	// account stays — a person outlives a project.
	purge := &projectPurge{ProjectID: f.project.ID, Now: time.Now().UnixNano()}
	f.submit(t, &ProjectDelete{
		ProjectID: f.project.ID, Confirm: "test", Now: time.Now().UnixNano(),
	})
	purge.Now = time.Now().UnixNano() + int64(GraceWindow) + int64(time.Hour)
	f.submit(t, purge)
	if !purge.Purged {
		t.Fatal("the project was not purged, so the cascade was never exercised")
	}

	memberships, err = f.Memberships(t.Context(), account.ID)
	if err != nil || len(memberships) != 0 {
		t.Errorf("memberships = %+v, err = %v; the purge must take them", memberships, err)
	}
	if survivor, err := f.AccountByID(context.Background(), account.ID); err != nil || survivor == nil {
		t.Errorf("the account did not survive its project's purge: %v, %v", survivor, err)
	}
}

// TestPasswordRoundTrip is Decision 1's rule: a length and nothing else, at
// the stated cost.
func TestPasswordRoundTrip(t *testing.T) {
	hash := hashOnce(t)
	account := &Account{hash: hash}
	if !account.Verify(anySlot(), testAccountPassword) {
		t.Error("the password does not verify against its own hash")
	}
	if account.Verify(anySlot(), testAccountPassword+"!") {
		t.Error("a wrong password verified")
	}

	// An invited account has no password, so nothing verifies — which is
	// what makes `pending` answer the login with the same 401 as a wrong
	// password (Decision 8).
	if (&Account{}).Verify(anySlot(), "") {
		t.Error("an account with no password must verify nothing")
	}

	// The cost is stated rather than defaulted, so it is asserted.
	if got := string(hash[:7]); got != "$2a$12$" && got != "$2b$12$" {
		t.Errorf("hash prefix = %q, want cost %d", got, PasswordCost)
	}

	if _, err := HashPassword(anySlot(), strings.Repeat("a", MinPasswordLength-1)); err == nil {
		t.Errorf("a %d-character password was accepted", MinPasswordLength-1)
	}
	if _, err := HashPassword(anySlot(), strings.Repeat("a", MaxPasswordLength+1)); err == nil {
		t.Errorf("a %d-character password was accepted", MaxPasswordLength+1)
	}
	if _, err := HashPassword(anySlot(), strings.Repeat("a", MinPasswordLength)); err != nil {
		t.Errorf("the shortest allowed password was refused: %v", err)
	}
}

// TestLastOwnerCannotStandDown is the one invariant a server needs to never be
// locked out of itself (Decision 2), and the reason the check is inside the
// write transaction.
func TestLastOwnerCannotStandDown(t *testing.T) {
	f := newAccountFixture(t)
	first := f.invite(t, "founder@example.com", true)

	no, yes := false, true
	for _, change := range []*AccountUpdate{
		{AccountID: first.ID, Owner: &no},
		{AccountID: first.ID, Disabled: &yes},
	} {
		change.Now = time.Now().UnixNano()
		err := f.writer.Submit(t.Context(), change)
		if !rejected(err) || !strings.Contains(err.Error(), "last owner") {
			t.Fatalf("err = %v, want a refusal naming the last owner", err)
		}
	}
	if err := f.writer.Submit(t.Context(), &AccountDelete{
		AccountID: first.ID, Confirm: first.Email,
	}); !rejected(err) || !strings.Contains(err.Error(), "last owner") {
		t.Fatalf("err = %v, want a refusal naming the last owner", err)
	}

	// With a second owner it is allowed, and the second one then cannot
	// stand down either.
	second := f.invite(t, "partner@example.com", true)
	f.submit(t, &AccountUpdate{AccountID: first.ID, Owner: &no, Now: time.Now().UnixNano()})
	if owners, _ := f.EnabledOwners(t.Context()); owners != 1 {
		t.Fatalf("enabled owners = %d, want 1", owners)
	}
	err := f.writer.Submit(t.Context(), &AccountUpdate{
		AccountID: second.ID, Disabled: &yes, Now: time.Now().UnixNano(),
	})
	if !rejected(err) {
		t.Fatalf("err = %v, want the second owner to be the last one now", err)
	}

	// A disabled owner counts as no owner (spec 028, edge cases): disabling
	// the only one is refused, so the state cannot be reached that way — but
	// a database that holds it must answer "no owners", which is what makes
	// the setup screen come back.
	if _, err := f.db.Exec(`UPDATE accounts SET disabled = 1 WHERE id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	if owners, _ := f.EnabledOwners(t.Context()); owners != 0 {
		t.Errorf("enabled owners = %d, want a disabled owner to count as none", owners)
	}
}

// TestAPendingOwnerIsNoOwner is the other half of Decision 22, and the one
// that is reachable by ordinary use: an owner invites a second owner, the
// invitation is never opened, and the first one stands down. Counting the
// invited account would leave a server nobody can sign in to — and, because
// it believes it has an owner, one that prints no setup link either.
func TestAPendingOwnerIsNoOwner(t *testing.T) {
	f := newAccountFixture(t)
	founder := f.invite(t, "founder@example.com", true)

	// Invited as an owner and never accepted.
	now := time.Now().UnixNano()
	f.submit(t, &AccountCreate{
		Email: "partner@example.com", Owner: true,
		TokenID: SessionID("partner"), ExpiresAt: now + int64(7*24*time.Hour), Now: now,
	})
	if owners, _ := f.EnabledOwners(t.Context()); owners != 1 {
		t.Fatalf("enabled owners = %d, want only the one who can sign in", owners)
	}

	no, yes := false, true
	for _, change := range []*AccountUpdate{
		{AccountID: founder.ID, Owner: &no},
		{AccountID: founder.ID, Disabled: &yes},
	} {
		change.Now = time.Now().UnixNano()
		err := f.writer.Submit(t.Context(), change)
		if !rejected(err) || !strings.Contains(err.Error(), "last owner") {
			t.Fatalf("err = %v, want the pending owner not to count", err)
		}
	}
	if err := f.writer.Submit(t.Context(), &AccountDelete{
		AccountID: founder.ID, Confirm: founder.Email,
	}); !rejected(err) || !strings.Contains(err.Error(), "last owner") {
		t.Fatalf("err = %v, want the pending owner not to count", err)
	}

	// Once the invitation is accepted the count is two and the founder may
	// stand down — which is the whole of what "pending" was holding up.
	f.submit(t, &InviteAccept{
		SessionSeed: SessionSeed{SessionID: SessionID("partner-session"),
			ExpiresAt: now + int64(30*24*time.Hour), Now: now},
		TokenID: SessionID("partner"), NewHash: hashOnce(t),
	})
	if owners, _ := f.EnabledOwners(t.Context()); owners != 2 {
		t.Fatalf("enabled owners = %d, want both", owners)
	}
	f.submit(t, &AccountUpdate{AccountID: founder.ID, Owner: &no, Now: time.Now().UnixNano()})
}

// TestSetupComesBackForAServerWithNoOwnerWhoCanSignIn: a pending owner must
// not make the setup screen go away, because the person it was created for
// cannot open the door either (Decision 22).
func TestSetupComesBackForAServerWithNoOwnerWhoCanSignIn(t *testing.T) {
	f := newAccountFixture(t)
	now := time.Now().UnixNano()
	f.submit(t, &AccountCreate{
		Email: "invited@example.com", Owner: true,
		TokenID: SessionID("invited"), ExpiresAt: now + int64(7*24*time.Hour), Now: now,
	})

	if owners, _ := f.EnabledOwners(t.Context()); owners != 0 {
		t.Fatalf("enabled owners = %d, want the setup link to keep being printed", owners)
	}
	// And setup still works: it promotes the row that is already there
	// rather than colliding with the unique email (Decision 20).
	setup := &SetupOwner{
		SessionSeed: SessionSeed{SessionID: SessionID("setup"),
			ExpiresAt: now + int64(30*24*time.Hour), Now: now},
		Email: "invited@example.com", Name: "The Founder", Hash: hashOnce(t),
	}
	f.submit(t, setup)
	if setup.Account == nil || !setup.Account.Owner || setup.Account.Pending {
		t.Fatalf("account = %+v, want an owner who can sign in", setup.Account)
	}
	if owners, _ := f.EnabledOwners(t.Context()); owners != 1 {
		t.Errorf("enabled owners = %d after the setup", owners)
	}
}

// TestOwnerPromotionDropsMemberships: an owner has every project, so the rows
// that said which ones go (Decision 12) — and demotion leaves none, so the
// account sees nothing until it is given projects.
func TestOwnerPromotionDropsMemberships(t *testing.T) {
	f := newAccountFixture(t)
	f.invite(t, "founder@example.com", true)
	helper := f.invite(t, "helper@example.com", false,
		Membership{ProjectID: f.project.ID, Role: RoleEditor})

	yes, no := true, false
	f.submit(t, &AccountUpdate{AccountID: helper.ID, Owner: &yes, Now: time.Now().UnixNano()})
	if rows, _ := f.Memberships(t.Context(), helper.ID); len(rows) != 0 {
		t.Errorf("memberships = %+v after promotion, want none", rows)
	}
	promoted, _ := f.AccountByID(context.Background(), helper.ID)
	projects, err := f.AccountProjects(t.Context(), promoted)
	if err != nil || len(projects) != 1 || projects[0].Role != RoleOwner {
		t.Fatalf("an owner's projects = %+v, err = %v", projects, err)
	}

	f.submit(t, &AccountUpdate{AccountID: helper.ID, Owner: &no, Now: time.Now().UnixNano()})
	demoted, _ := f.AccountByID(context.Background(), helper.ID)
	projects, err = f.AccountProjects(t.Context(), demoted)
	if err != nil || len(projects) != 0 {
		t.Errorf("a demoted owner's projects = %+v, want none until it is given some", projects)
	}
}

// TestDisablingEndsSessions: taking access away has to take it away now, not
// at the end of the month a session had left (Decision 12).
func TestDisablingEndsSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.invite(t, "founder@example.com", true)
	helper := f.invite(t, "helper@example.com", false)

	now := time.Now().UnixNano()
	if session, _, _ := f.SessionByCookie(context.Background(), "session-for-helper@example.com", now); session == nil {
		t.Fatal("accepting an invitation must sign the person in")
	}

	yes := true
	f.submit(t, &AccountUpdate{AccountID: helper.ID, Disabled: &yes, Now: now})
	if session, _, _ := f.SessionByCookie(context.Background(), "session-for-helper@example.com", now); session != nil {
		t.Error("a disabled account's sessions must end at once")
	}
}

// TestInvitationIsSingleUseAndExpires, and a re-invitation is the reset: it
// ends the sessions and leaves the old password working until the link is
// used (Decision 10).
func TestInvitationIsSingleUseAndExpires(t *testing.T) {
	f := newAccountFixture(t)
	account := f.invite(t, "helper@example.com", false)
	now := time.Now().UnixNano()

	// Spent: the same token again is refused.
	err := f.writer.Submit(t.Context(), &InviteAccept{
		SessionSeed: SessionSeed{SessionID: SessionID("second"), ExpiresAt: now + 1e12, Now: now},
		TokenID:     SessionID("token-for-helper@example.com"), NewHash: hashOnce(t),
	})
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("err = %v, want a spent invitation to be refused", err)
	}

	// Accepting stamped the login.
	accepted, _ := f.AccountByID(context.Background(), account.ID)
	if accepted.LastLoginAt == nil {
		t.Error("accepting an invitation must stamp last_login_at")
	}
	if accepted.Pending {
		t.Error("an accepted account is not pending any more")
	}

	// A reset: the sessions go, the old password stays.
	f.submit(t, &InviteMint{
		AccountID: account.ID, TokenID: SessionID("reset"),
		ExpiresAt: now + int64(7*24*time.Hour), Now: now,
	})
	if session, _, _ := f.SessionByCookie(context.Background(), "session-for-helper@example.com", now); session != nil {
		t.Error("a reset must end the account's sessions")
	}
	stillThere, _ := f.AccountByID(context.Background(), account.ID)
	if !stillThere.Verify(anySlot(), testAccountPassword) {
		t.Error("a reset must leave the old password working until the link is used")
	}

	// An expired token is refused with the same sentence as a spent one.
	f.submit(t, &InviteMint{
		AccountID: account.ID, TokenID: SessionID("stale"), ExpiresAt: now - 1, Now: now,
	})
	err = f.writer.Submit(t.Context(), &InviteAccept{
		SessionSeed: SessionSeed{SessionID: SessionID("third"), ExpiresAt: now + 1e12, Now: now},
		TokenID:     SessionID("stale"), NewHash: hashOnce(t),
	})
	if err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("err = %v, want an expired invitation to be refused", err)
	}
}

// TestExpiredSessionsAreRefusedThenSwept: the sweep is a cadence, not a
// guarantee, so a session that has run out stops working at the moment it does
// and the rows go on the next pass (Decision 4).
func TestExpiredSessionsAreRefusedThenSwept(t *testing.T) {
	f := newAccountFixture(t)
	account := f.invite(t, "helper@example.com", false)
	now := time.Now().UnixNano()

	f.submit(t, &SessionSlide{
		SessionID: SessionID("session-for-helper@example.com"), ExpiresAt: now - 1, Now: now,
	})
	if session, _, _ := f.SessionByCookie(context.Background(), "session-for-helper@example.com", now); session != nil {
		t.Fatal("an expired session must be refused before the sweeper reaches it")
	}
	if rows, _ := f.AccountSessions(t.Context(), account.ID, now); len(rows) != 0 {
		t.Error("an expired session must not be on the account's list either")
	}

	sweep := &AccountSweep{Now: now, Limit: 100}
	f.submit(t, sweep)
	if sweep.Sessions != 1 {
		t.Errorf("swept %d sessions, want the one that had run out", sweep.Sessions)
	}

	// The invitation this account was created with is spent, so the token
	// sweep has nothing to find; one that expired unspent does go.
	f.submit(t, &InviteMint{
		AccountID: account.ID, TokenID: SessionID("stale"), ExpiresAt: now - 1, Now: now,
	})
	sweep = &AccountSweep{Now: now, Limit: 100}
	f.submit(t, sweep)
	if sweep.Tokens != 1 {
		t.Errorf("swept %d invitations, want the one that had run out", sweep.Tokens)
	}
}

// TestEmailIsCaseInsensitiveAndUnique: a different capitalisation is the same
// account, and the stored spelling is the one the owner typed (spec 028, edge
// cases).
func TestEmailIsCaseInsensitiveAndUnique(t *testing.T) {
	f := newAccountFixture(t)
	f.invite(t, "Helper@Example.com", false)

	found, err := f.AccountByEmail(t.Context(), "helper@EXAMPLE.COM")
	if err != nil || found == nil {
		t.Fatalf("account = %v, err = %v; the lookup must ignore case", found, err)
	}
	if found.Email != "Helper@Example.com" {
		t.Errorf("email = %q, want the spelling that was typed", found.Email)
	}

	now := time.Now().UnixNano()
	err = f.writer.Submit(t.Context(), &AccountCreate{
		Email: "HELPER@example.com", TokenID: SessionID("another"),
		ExpiresAt: now + 1e12, Now: now,
	})
	if !rejected(err) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want a conflict naming the account that is already there", err)
	}
}

// TestProjectRoleIsOwnerForAnOwner: the flag is the membership, so an owner
// has a role in a project it has no row for.
func TestProjectRole(t *testing.T) {
	f := newAccountFixture(t)
	founder := f.invite(t, "founder@example.com", true)
	helper := f.invite(t, "helper@example.com", false,
		Membership{ProjectID: f.project.ID, Role: RoleViewer})

	if _, role, _ := f.ProjectWithRole(context.Background(), founder, f.project.ID); role != RoleOwner {
		t.Errorf("an owner's role = %q, want %q", role, RoleOwner)
	}
	if _, role, _ := f.ProjectWithRole(context.Background(), founder, "0123456789abcdef0123456789abcdef"); role != RoleOwner {
		t.Error("an owner has every project, including ones that do not exist; " +
			"the project lookup is what answers 404")
	}
	if _, role, _ := f.ProjectWithRole(context.Background(), helper, f.project.ID); role != RoleViewer {
		t.Errorf("a member's role = %q, want %q", role, RoleViewer)
	}
	if _, role, _ := f.ProjectWithRole(context.Background(), helper, "0123456789abcdef0123456789abcdef"); role != "" {
		t.Errorf("role in a project one is not a member of = %q, want none", role)
	}

	members, err := f.ProjectMembers(t.Context(), f.project.ID)
	if err != nil || len(members) != 1 || members[0].Email != "helper@example.com" {
		t.Fatalf("members = %+v, err = %v; owners are not listed", members, err)
	}
}
