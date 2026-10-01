package store

import (
	"database/sql"
	"strings"
	"testing"
)

// An erasure learns its own compaction finished from the pass that covered its
// latest request, and only that one (spec 044 #22). Coverage is counted, not
// timed: a request's stamp comes from a clock, and after a pass has cleared the
// pending one, a clock stepped back makes a later request's stamp the smaller.
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
	// ask is what an erasure's job does: request, and record the request on
	// its row, in one transaction.
	ask := func(clock int64) {
		in(func(tx *sql.Tx) error {
			requested, err := requestCompaction(tx, clock)
			if err != nil {
				return err
			}
			_, err = recordProgress(tx, "e1", DeleteCounts{}, requested, "")
			return err
		})
	}
	// pass is what the sweeper does: read the state, then finish.
	pass := func(at int64) {
		t.Helper()
		state, err := s.Compaction(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		in((&compactionDone{Started: state.RequestedAt, Covers: state.Requests, At: at}).apply)
	}

	ask(1000) // request 1
	in((&compactionDone{Started: 0, Covers: 0, At: 1500}).apply)
	if got := stamp(); got.Valid {
		t.Errorf("a pass that began before the request stamped the erasure at %d", got.Int64)
	}
	pass(2000)
	if got := stamp(); got.Int64 != 2000 {
		t.Errorf("compacted_at = %v, want the pass that covered request 1", got)
	}
	in((&compactionDone{Started: 0, Covers: 1, At: 2500}).apply)
	if got := stamp(); got.Int64 != 2000 {
		t.Errorf("compacted_at = %v after a later pass, want it unmoved at 2000", got)
	}

	// The clock steps back: request 2's stamp is 10, below request 1's 1000.
	// It is still the later request, and the stamp goes.
	ask(10)
	if got := stamp(); got.Valid {
		t.Errorf("compacted_at = %v after a later request under a clock stepped back, want none", got)
	}
	in((&compactionDone{Started: 0, Covers: 1, At: 3000}).apply)
	if got := stamp(); got.Valid {
		t.Errorf("a pass that covered only request 1 stamped the erasure at %d", got.Int64)
	}
	pass(4000)
	if got := stamp(); got.Int64 != 4000 {
		t.Errorf("compacted_at = %v, want the pass that covered request 2", got)
	}
}

// A finished pass stamps the erasures it covered by a seek on the partial
// index of the ones waiting, inside the writer's transaction: never a walk of
// every erasure there has been.
func TestStampingCoveredErasuresSeeks(t *testing.T) {
	s := openFresh(t)
	plan, err := s.explainQueryPlan(stampCovered, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_erasures_awaiting_compaction") || strings.Contains(joined, "SCAN erasures") {
		t.Errorf("the stamp does not seek its index:\n%s", joined)
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
			`DROP INDEX idx_erasures_awaiting_compaction`,
			`ALTER TABLE erasures DROP COLUMN compacted_at`,
			`ALTER TABLE erasures DROP COLUMN compaction_request`,
			`ALTER TABLE compaction DROP COLUMN requests`,
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
		// Pending, the erasure that asked waits for request 1, which the
		// next pass covers.
		if pending {
			state, err := upgraded.Compaction(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var request int64
			if err := upgraded.db.QueryRow(`SELECT compaction_request FROM erasures WHERE id = 'asked'`).
				Scan(&request); err != nil {
				t.Fatal(err)
			}
			if state.Requests != 1 || request != 1 {
				t.Errorf("requests = %d, the erasure's = %d, want both 1", state.Requests, request)
			}
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
