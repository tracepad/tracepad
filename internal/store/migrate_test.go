package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Migration 0005 rebuilds two tables (spec 005, Data contract), which is the
// riskiest shape a migration can have: with foreign keys on, dropping
// `projects` would fire every cascade hanging off it and empty the database.
// These tests upgrade a real 0004 database and check that the data is still
// there afterwards — and that the cascades still are too.

// openAtSchema0004 builds a database exactly as the previous release left it:
// migrations 0001–0004 applied and recorded, and rows in the tables 0005
// rebuilds.
func openAtSchema0004(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tracepad.db")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0001_init.sql", "0002_traces.sql",
		"0003_scores_prompts.sql", "0004_read_indexes.sql"} {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestMigration0005UpgradesAPopulatedDatabase(t *testing.T) {
	path := openAtSchema0004(t)

	// Rows in every table 0005 touches or cascades from.
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := db.Exec(query, args...); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
		}
		exec(`INSERT INTO projects (id, name, retention_days) VALUES ('p1', 'app', 30)`)
		exec(`INSERT INTO api_keys (public_key, secret_hash, project_id) VALUES ('tp-pk-1', X'00', 'p1')`)
		exec(`INSERT INTO payloads (id, compression, size_raw, body) VALUES (1, 'none', 2, X'7b7d')`)
		// One trace whose client clock is in the past, one in the future:
		// the backfill is min(timestamp, migration time) (spec 005 #1).
		exec(`INSERT INTO traces (project_id, id, name, timestamp, metadata_id, observation_count)
		      VALUES ('p1', 'old', 'past', ?, 1, 1)`,
			time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
		exec(`INSERT INTO traces (project_id, id, name, timestamp) VALUES ('p1', 'new', 'future', ?)`,
			time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
		exec(`INSERT INTO observations (project_id, trace_id, id, type, level)
		      VALUES ('p1', 'old', 'span1', 'span', 'DEFAULT')`)
		exec(`INSERT INTO scores (project_id, id, trace_id, name, data_type, value, timestamp, created_at)
		      VALUES ('p1', 's1', 'old', 'quality', 'numeric', 1.0, 0, 0)`)
		exec(`INSERT INTO raw_batches (project_id, received_at, body) VALUES ('p1', 0, X'00')`)
	}()

	before := time.Now().Add(-time.Second).UnixNano()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	after := time.Now().Add(time.Second).UnixNano()

	// Nothing was cascaded away by the rebuild.
	for table, want := range map[string]int64{
		"projects": 1, "api_keys": 1, "traces": 2, "observations": 1,
		"scores": 1, "raw_batches": 1, "payloads": 1,
	} {
		var got int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %d after the upgrade, want %d", table, got, want)
		}
	}

	// retention_days is NULL for every existing row: the 0001 default of 30
	// never acted, and turning it on retroactively would delete data the
	// operator never asked us to (spec 005 #2).
	project, err := s.ProjectByName("app")
	if err != nil || project == nil {
		t.Fatalf("project after upgrade: %+v %v", project, err)
	}
	if project.RetentionDays != nil {
		t.Errorf("retention_days = %d after the upgrade, want NULL (keep forever)", *project.RetentionDays)
	}
	if project.RawRetentionDays != nil || project.DeletedAt != nil {
		t.Errorf("new columns are not empty on an upgraded row: %+v", project)
	}

	// The backfill: the past trace keeps its own timestamp, the one that
	// claimed 2200 is clamped to the migration time rather than being made
	// immortal.
	arrivals := map[string]int64{}
	rows, err := s.db.Query(`SELECT id, ingested_at FROM traces`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			arrived int64
		)
		if err := rows.Scan(&id, &arrived); err != nil {
			t.Fatal(err)
		}
		arrivals[id] = arrived
	}
	if want := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(); arrivals["old"] != want {
		t.Errorf("ingested_at of the past trace = %d, want its own timestamp %d", arrivals["old"], want)
	}
	if arrivals["new"] < before || arrivals["new"] > after {
		t.Errorf("ingested_at of the future trace = %d, want the migration time between %d and %d",
			arrivals["new"], before, after)
	}
}

// TestMigration0005KeepsTheCascades: the rebuild recreates the tables that
// every ON DELETE CASCADE points at. If the references came back dangling, a
// project delete would leave its data behind — silently, until a read joined.
func TestMigration0005KeepsTheCascades(t *testing.T) {
	path := openAtSchema0004(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	project, err := s.CreateProject("app", KeyPair{PublicKey: "tp-pk-1", Secret: "tp-sk-1"})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := s.NewWriter(WriterOptions{CommitWindow: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Submit(t.Context(), batchFor(project.ID, hexTrace(1), hexSpan(1))); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traces", "observations", "api_keys"} {
		var got int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != 0 {
			t.Errorf("%s = %d after deleting the project, want the cascade to have taken it", table, got)
		}
	}
}

// TestArrivalHasNoDefault is the reason `traces` is rebuilt rather than
// altered (spec 005 Decision 16): `ALTER TABLE ADD COLUMN` cannot add a NOT
// NULL column without leaving a default, and a default here would date a
// forgotten insert to 1970 and feed it to the next sweep.
func TestArrivalHasNoDefault(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.CreateProject("app", KeyPair{PublicKey: "tp-pk-1", Secret: "tp-sk-1"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.db.Exec(`INSERT INTO traces (project_id, id) SELECT id, 'x' FROM projects`)
	if err == nil {
		t.Fatal("a trace was stored without saying when it arrived")
	}
	if !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("insert failed with %v, want a NOT NULL constraint failure", err)
	}
}
