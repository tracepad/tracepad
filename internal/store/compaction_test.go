package store

import (
	"database/sql"
	"testing"
)

// An erasure learns its own compaction finished from the pass that covered its
// latest request, and only that one (spec 044 #22): a pass that started from an
// earlier request does not cover it, a later pass does not move the stamp, and
// a request the erasure makes after being covered takes the stamp away until a
// pass covers that too.
func TestAnErasureIsStampedByThePassThatCoveredIt(t *testing.T) {
	s := openFresh(t)
	project, err := s.CreateProject("app", KeyPair{PublicKey: "tp-pk-1", Secret: "tp-sk-1"})
	if err != nil {
		t.Fatal(err)
	}
	in := func(apply func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := apply(tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO erasures (id, project_id, user_id, state, created_at, now, compaction)
		VALUES ('e1', ?, 'u', ?, 1, 1, 0)`, project.ID, ErasureRunning); err != nil {
		t.Fatal(err)
	}
	stamp := func() sql.NullInt64 {
		t.Helper()
		var at sql.NullInt64
		if err := s.db.QueryRow(`SELECT compacted_at FROM erasures WHERE id = 'e1'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	progress := func(requested int64) {
		in(func(tx *sql.Tx) error { _, err := recordProgress(tx, "e1", DeleteCounts{}, requested, ""); return err })
	}
	done := func(started, at int64) {
		in((&compactionDone{Started: started, At: at}).apply)
	}

	progress(100)
	done(99, 1000)
	if got := stamp(); got.Valid {
		t.Errorf("a pass started from an earlier request stamped the erasure at %d", got.Int64)
	}
	done(100, 2000)
	if got := stamp(); got.Int64 != 2000 {
		t.Errorf("compacted_at = %v, want the pass that covered request 100", got)
	}
	done(300, 3000)
	if got := stamp(); got.Int64 != 2000 {
		t.Errorf("compacted_at = %v after a later pass, want it unmoved at 2000", got)
	}
	progress(400)
	if got := stamp(); got.Valid {
		t.Errorf("compacted_at = %v after a later request, want none until a pass covers it", got)
	}
	progress(350)
	done(400, 4000)
	if got := stamp(); got.Int64 != 4000 {
		t.Errorf("compacted_at = %v, want the pass that covered request 400", got)
	}
}

// The upgrade stamps what the store already knows (spec 044 #22): with nothing
// pending every erasure that asked was covered, by the latest pass at the
// latest; with a request pending, the next pass stamps them.
func TestMigration0034StampsTheErasuresAlreadyCompacted(t *testing.T) {
	for _, pending := range []bool{false, true} {
		s, path := openTemp(t)
		project, err := s.CreateProject("app", KeyPair{PublicKey: "tp-pk-1", Secret: "tp-sk-1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{
			`ALTER TABLE erasures DROP COLUMN compacted_at`,
			`DELETE FROM schema_migrations WHERE filename = '0034_erasure_compacted.sql'`,
			`INSERT INTO erasures (id, project_id, state, created_at, now, compaction) VALUES
			   ('asked', '` + project.ID + `', 'done', 1, 1, 100), ('nothing', '` + project.ID + `', 'done', 1, 1, 0)`,
			`INSERT INTO compaction (id, requested_at, completed_at) VALUES (1, NULL, 500)
			   ON CONFLICT (id) DO UPDATE SET requested_at = NULL, completed_at = 500`,
		} {
			if _, err := s.db.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if pending {
			if _, err := s.db.Exec(`UPDATE compaction SET requested_at = 600 WHERE id = 1`); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		upgraded, err := Open(path)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		for id, want := range map[string]int64{"asked": 500, "nothing": 0} {
			if pending {
				want = 0
			}
			var at sql.NullInt64
			if err := upgraded.db.QueryRow(`SELECT compacted_at FROM erasures WHERE id = ?`, id).Scan(&at); err != nil {
				t.Fatal(err)
			}
			if at.Int64 != want {
				t.Errorf("pending=%v: erasure %s compacted_at = %v, want %d", pending, id, at, want)
			}
		}
		upgraded.Close()
	}
}
