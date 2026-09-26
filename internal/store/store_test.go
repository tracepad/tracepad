package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMigrateFreshAndReopen(t *testing.T) {
	s, path := openTemp(t)
	if n, err := s.CountProjects(); err != nil || n != 0 {
		t.Fatalf("fresh db: n=%d err=%v", n, err)
	}
	s.Close()

	// Reopen: migrations must be a no-op, no backup file for a fresh db.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if matches, _ := filepath.Glob(path + ".pre-*.bak"); len(matches) != 0 {
		t.Fatalf("unexpected backup files: %v", matches)
	}
}

func TestBootstrapDefaultOnceAndIdempotent(t *testing.T) {
	s, _ := openTemp(t)

	boot, err := s.Bootstrap(nil)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(boot.Created) != 1 || boot.Created[0].Project.Name != "default" {
		t.Fatalf("want created default, got %+v", boot.Created)
	}
	if boot.Created[0].Declared {
		t.Fatal("the generated default reported as declared: its secret would never be printed")
	}
	secret := boot.Created[0].Keys.Secret

	// Second run: nothing new.
	boot2, err := s.Bootstrap(nil)
	if err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}
	if len(boot2.Created) != 0 {
		t.Fatalf("second bootstrap created %+v", boot2.Created)
	}

	// Auth lookup by secret works; wrong secret does not.
	p, key, err := s.KeyBySecret(secret)
	if err != nil || p == nil || p.Name != "default" {
		t.Fatalf("KeyBySecret: p=%+v err=%v", p, err)
	}
	// The server made this key by itself, and it may do everything a key
	// may (spec 045 #5, #8).
	if key.PublicKey != boot.Created[0].Keys.PublicKey || key.CreatedBy.Via != MintedAtStartup ||
		strings.Join(key.Scopes, " ") != AllScopes {
		t.Fatalf("the first-start key = %+v, want the server's own with every scope", key)
	}
	if p, key, _ := s.KeyBySecret("tp-sk-wrong"); p != nil || key != nil {
		t.Fatalf("wrong secret resolved to %+v, %+v", p, key)
	}
}

func TestBootstrapDeclarativeIdempotent(t *testing.T) {
	s, _ := openTemp(t)
	specs := []ProvisionSpec{
		{Name: "app", PublicKey: "tp-pk-a", SecretKey: "tp-sk-a"},
		{Name: "eval", PublicKey: "tp-pk-b", SecretKey: "tp-sk-b"},
	}
	first, err := s.Bootstrap(specs)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// The operator wrote these secrets, so they are marked for the banner
	// to leave out (spec 001 #12).
	if len(first.Created) != 2 || !first.Created[0].Declared || !first.Created[1].Declared {
		t.Fatalf("want two declared projects created, got %+v", first.Created)
	}
	// Restart with same env: no new projects, keys untouched.
	boot, err := s.Bootstrap(specs)
	if err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}
	if len(boot.Created) != 0 {
		t.Fatalf("re-bootstrap created %+v", boot.Created)
	}
	if p, key, _ := s.KeyBySecret("tp-sk-b"); p == nil || p.Name != "eval" ||
		key.CreatedBy.Via != MintedAtStartup {
		t.Fatalf("declared key does not resolve as the server's own, got %+v, %+v", p, key)
	}
	// Declared projects present, no stray "default".
	if p, _ := s.ProjectByName("default"); p != nil {
		t.Fatal("default project created despite declarative specs")
	}
}

func TestUnknownFutureMigrationRefused(t *testing.T) {
	s, path := openTemp(t)
	if _, err := s.db.Exec(`INSERT INTO schema_migrations (filename) VALUES ('9999_future.sql')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected refusal to open db migrated by a newer binary")
	}
}

func TestBackupIncludesWALData(t *testing.T) {
	// Committed rows may still live in the WAL, not the main file; the
	// snapshot must include them (spec 001 #11). Taken while the store is
	// open and unsynced — the plain-file-copy regression scenario.
	s, path := openTemp(t)
	if _, err := s.Bootstrap(nil); err != nil {
		t.Fatal(err)
	}
	s.fresh = false
	if err := s.backupBefore("migrations/0001_init.sql"); err != nil {
		t.Fatalf("backupBefore: %v", err)
	}
	b, err := Open(path + ".pre-0001_init.bak")
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer b.Close()
	if n, _ := b.CountProjects(); n != 1 {
		t.Fatalf("backup lost WAL data: projects = %d, want 1", n)
	}

	// Re-running with an existing backup must overwrite, not fail
	// (VACUUM INTO refuses existing targets on its own).
	if err := s.backupBefore("migrations/0001_init.sql"); err != nil {
		t.Fatalf("backupBefore over existing backup: %v", err)
	}
}

func TestBackupBeforeMigration(t *testing.T) {
	// Simulate an upgrade: open a db, then pretend 0001 is pending again by
	// clearing the record — the runner must back the file up first.
	s, path := openTemp(t)
	if _, err := s.Bootstrap(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE filename = '0001_init.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE api_keys; DROP TABLE projects`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen with pending migration: %v", err)
	}
	defer s2.Close()
	if _, err := os.Stat(path + ".pre-0001_init.bak"); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
}
