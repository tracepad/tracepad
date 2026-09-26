package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// Key provenance in the store (spec 045, Testing #5–#7 and #10).

// TestMigration0023KeepsEveryKey: 0023 on a database 0022 left, with projects,
// keys and an account. Every key comes out with all three scopes, `unknown`
// and never used, and a secret minted before the migration authenticates
// after it (Testing #5).
func TestMigration0023KeepsEveryKey(t *testing.T) {
	var names []string
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() < "0023" {
			names = append(names, e.Name())
		}
	}
	path := openAtSchemas(t, names...)
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			`INSERT INTO projects (id, name) VALUES ('p1', 'one'), ('p2', 'two')`,
			`INSERT INTO accounts (id, email, owner, created_at) VALUES ('a1', 'her@example.com', 1, 1)`,
			`INSERT INTO memberships (account_id, project_id, role, created_at) VALUES ('a1', 'p2', 'editor', 1)`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		// The shape 0001 wrote: no name, no scopes, nobody's.
		for _, key := range []struct{ project, public, secret, at string }{
			{"p1", "tp-pk-1", "tp-sk-1", "2026-01-01T00:00:00.000Z"},
			{"p1", "tp-pk-2", "tp-sk-2", "2026-02-01T00:00:00.000Z"},
			{"p2", "tp-pk-3", "tp-sk-3", "2026-03-01T00:00:00.000Z"},
		} {
			hash := sha256Of(key.secret)
			if _, err := tx.Exec(`INSERT INTO api_keys (public_key, secret_hash, project_id, created_at)
			                      VALUES (?, ?, ?, ?)`, key.public, hash, key.project, key.at); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()

	keys, err := s.ProjectKeys("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].PublicKey != "tp-pk-1" || keys[0].CreatedAt != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("p1's keys after the upgrade = %+v", keys)
	}
	for _, key := range keys {
		if strings.Join(key.Scopes, " ") != AllScopes || key.CreatedBy.Via != MintedBeforeProvenance ||
			key.CreatedBy.AccountID != "" || key.Standing != "" || key.LastUsedAt != nil || key.Name != "" {
			t.Errorf("a key from before 0023 = %+v, want every scope, unknown, never used", key)
		}
	}
	project, key, err := s.KeyBySecret(context.Background(), "tp-sk-3")
	if err != nil || project == nil || project.ID != "p2" || key.PublicKey != "tp-pk-3" {
		t.Fatalf("a secret minted before the upgrade resolves to %+v, %+v (%v)", project, key, err)
	}

	// The rebuilt table says what it may do and who made it, or refuses.
	for _, insert := range []string{
		`INSERT INTO api_keys (public_key, secret_hash, project_id, created_via) VALUES ('x', X'01', 'p1', 'startup')`,
		`INSERT INTO api_keys (public_key, secret_hash, project_id, scopes) VALUES ('x', X'01', 'p1', 'ingest')`,
		`INSERT INTO api_keys (public_key, secret_hash, project_id, scopes, created_via) VALUES ('x', X'01', 'p1', 'read ingest', 'startup')`,
		`INSERT INTO api_keys (public_key, secret_hash, project_id, scopes, created_via) VALUES ('x', X'01', 'p1', 'ingest', 'somebody')`,
	} {
		if _, err := s.db.Exec(insert); err == nil {
			t.Errorf("the schema took %s", insert)
		}
	}
	// And a purge still takes a project's keys with it.
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ProjectKeys("p1"); len(keys) != 0 {
		t.Errorf("a purged project's keys = %+v", keys)
	}
}

func sha256Of(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// TestStandingMovesWithTheAccount: the listing computes the minter's standing
// when it is read, so it follows the account through a demotion, a removal,
// a disabling and a deletion — after which the email is still answered
// (Testing #6).
func TestStandingMovesWithTheAccount(t *testing.T) {
	f := newAccountFixture(t)
	editor := f.invite(t, "editor@example.com", false,
		Membership{ProjectID: f.project.ID, Role: RoleEditor})
	owner := f.invite(t, "owner@example.com", true)

	mint := func(name string, origin KeyOrigin) {
		t.Helper()
		keys, err := GenerateKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		f.submit(t, &KeyCreate{ProjectID: f.project.ID, Keys: keys, Name: name, Origin: origin})
	}
	mint("by the editor", OriginAccount(editor))
	mint("by the owner", OriginAccount(owner))
	mint("by the token", KeyOrigin{Via: MintedByAdminToken})

	standings := func() map[string]KeyInfo {
		t.Helper()
		keys, err := f.ProjectKeys(f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]KeyInfo{}
		for _, key := range keys {
			byName[key.Name] = key
		}
		return byName
	}
	expect := func(step, want string) {
		t.Helper()
		key := standings()["by the editor"]
		if key.Standing != want || key.CreatedBy.Email != "editor@example.com" {
			t.Errorf("%s: the editor's key = %+v, want standing %q and the email kept", step, key, want)
		}
	}

	now := standings()
	if key := now[""]; key.CreatedBy.Via != MintedAtStartup || key.Standing != "" {
		t.Errorf("the fixture's own key = %+v, want the server's", key)
	}
	if key := now["by the owner"]; key.Standing != RoleOwner || key.CreatedBy.AccountID != owner.ID {
		t.Errorf("the owner's key = %+v, want standing owner from the flag", key)
	}
	if key := now["by the token"]; key.CreatedBy.Via != MintedByAdminToken || key.Standing != "" ||
		key.CreatedBy.Email != "" {
		t.Errorf("the token's key = %+v", key)
	}
	expect("minted", RoleEditor)

	f.submit(t, &MembershipPut{AccountID: editor.ID, ProjectID: f.project.ID, Role: RoleViewer,
		Now: time.Now().UnixNano()})
	expect("demoted", RoleViewer)

	f.submit(t, &MembershipDelete{AccountID: editor.ID, ProjectID: f.project.ID})
	expect("removed", StandingRemoved)

	disabled := true
	f.submit(t, &AccountUpdate{AccountID: editor.ID, Disabled: &disabled, Now: time.Now().UnixNano()})
	expect("disabled", StandingDisabled)

	minted, err := f.KeysMintedBy(editor.ID)
	if err != nil || len(minted) != 1 || minted[0].Key.Name != "by the editor" ||
		minted[0].ProjectID != f.project.ID || minted[0].ProjectName != "test" {
		t.Fatalf("the keys the editor minted = %+v (%v)", minted, err)
	}

	f.submit(t, &AccountDelete{AccountID: editor.ID, Confirm: "editor@example.com"})
	expect("deleted", StandingDeleted)
	if key := standings()["by the editor"]; key.CreatedBy.AccountID != "" {
		t.Errorf("a deleted minter's id = %q, want the reference gone", key.CreatedBy.AccountID)
	}
	// Deleting the account took no key with it (#10).
	if keys, _ := f.ProjectKeys(f.project.ID); len(keys) != 4 {
		t.Errorf("keys after the deletion = %d, want all four", len(keys))
	}
}

// TestKeyUseNeverMovesBack: a use is written as the later of the stored and
// the flushed time, and a use of a key revoked since is dropped without error
// (Testing #7).
func TestKeyUseNeverMovesBack(t *testing.T) {
	f := newAccountFixture(t)
	used := func() *int64 {
		t.Helper()
		keys, err := f.ProjectKeys(f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		return keys[0].LastUsedAt
	}
	if at := used(); at != nil {
		t.Fatalf("a fresh key was last used at %d", *at)
	}
	f.submit(t, &KeyUse{Seen: map[string]int64{"tp-pk-test": 2000}})
	f.submit(t, &KeyUse{Seen: map[string]int64{"tp-pk-test": 1000}})
	if at := used(); at == nil || *at != 2000 {
		t.Errorf("after a late job the key was last used at %v, want 2000", at)
	}
	f.submit(t, &KeyUse{Seen: map[string]int64{"tp-pk-test": 3000, "tp-pk-revoked": 3000}})
	if at := used(); at == nil || *at != 3000 {
		t.Errorf("after a newer use the key was last used at %v, want 3000", at)
	}
}
