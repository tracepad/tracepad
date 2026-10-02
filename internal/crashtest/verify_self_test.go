package crashtest

import (
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/storetest"
)

// A check that cannot fail is worse than none. These run without a binary,
// so the gate keeps them: each damages a healthy file in the way a torn
// transaction would, and asks the check to see it.

func TestChecksSeeWhatTheyAreFor(t *testing.T) {
	path := storetest.Path(t)
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hour := time.Now().Add(-3*time.Hour).Unix() / 3600 * 3600

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO projects (id, name, created_at) VALUES ('p1', 'main', 1)`)
	exec(`INSERT INTO traces (project_id, id, user_id, timestamp, ingested_at, updated_at,
	                          observation_count, error_count)
	      VALUES ('p1', 't1', 'u1', ?, 1, 1, 2, 1)`, hour*1_000_000_000+5)
	exec(`INSERT INTO observations (project_id, trace_id, id, type, level) VALUES
	      ('p1', 't1', 's1', 'span', 'DEFAULT'), ('p1', 't1', 's2', 'span', 'ERROR')`)
	exec(`INSERT INTO search_entries (id, project_id, trace_id, observation_id, field)
	      VALUES (1, 'p1', 't1', 's1', 'input')`)
	exec(`INSERT INTO search_fts (rowid, body) VALUES (1, 'alpha')`)
	exec(`INSERT INTO scores (project_id, id, trace_id, name, data_type, value, timestamp, created_at)
	      VALUES ('p1', 'sc1', 't1', 'q', 'numeric', 1, 1, 1)`)
	exec(`INSERT INTO stats_rollup (project_id, rolled_until, last_pass) VALUES ('p1', ?, 1)`, hour+3600)
	exec(`INSERT INTO stats_hourly (project_id, hour, environment, release, model, count, error_count, latency)
	      VALUES ('p1', ?, 'default', '', '', 1, 1, '[]')`, hour)

	led := newLedger()
	led.traces["t1"] = &ackedTrace{user: "u1", spans: map[string]bool{"s1": true, "s2": true}}
	led.scores["sc1"] = "t1"

	expect := func(name string, got violations, want string) {
		t.Helper()
		if want == "" {
			if len(got) > 0 {
				t.Fatalf("%s: a healthy file was reported: %v", name, got)
			}
			return
		}
		for _, v := range got {
			if strings.Contains(v, want) {
				return
			}
		}
		t.Fatalf("%s: wanted a violation naming %q, got %v", name, want, got)
	}

	expect("healthy structure", structural(db), "")
	expect("healthy promises", durable(db, led, true), "")
	expect("healthy rollup", settled(db, "p1"), "")

	// A trace whose aggregates were not refreshed with its observations.
	exec(`UPDATE traces SET observation_count = 1 WHERE id = 't1'`)
	expect("stale count", structural(db), "trace aggregates disagree")
	exec(`UPDATE traces SET observation_count = 2 WHERE id = 't1'`)

	// An observation committed without the trace it belongs to.
	exec(`INSERT INTO observations (project_id, trace_id, id, type) VALUES ('p1', 'ghost', 'g1', 'span')`)
	expect("observation without trace", structural(db), "an observation with no trace")
	exec(`DELETE FROM observations WHERE trace_id = 'ghost'`)

	// A deletion that took the trace and left its score and index entry.
	exec(`PRAGMA foreign_keys = OFF`)
	exec(`INSERT INTO scores (project_id, id, trace_id, name, data_type, value, timestamp, created_at)
	      VALUES ('p1', 'sc2', 'gone', 'q', 'numeric', 1, 1, 1)`)
	expect("score without trace", structural(db), "a score whose trace is gone")
	exec(`DELETE FROM scores WHERE id = 'sc2'`)
	exec(`INSERT INTO search_entries (id, project_id, trace_id, observation_id, field)
	      VALUES (2, 'p1', 'gone', NULL, 'name')`)
	expect("entry without trace", structural(db), "a search entry whose trace is gone")
	expect("entry without fts row", structural(db), "a search entry with no FTS row")
	exec(`DELETE FROM search_entries WHERE id = 2`)
	exec(`INSERT INTO search_fts (rowid, body) VALUES (3, 'orphan')`)
	expect("fts row without entry", structural(db), "an FTS row with no search entry")
	exec(`DELETE FROM search_fts WHERE rowid = 3`)
	expect("structure restored", structural(db), "")

	// A promise broken: an acknowledged span missing, a deleted trace there.
	exec(`DELETE FROM observations WHERE id = 's2'`)
	exec(`UPDATE traces SET observation_count = 1, error_count = 0 WHERE id = 't1'`)
	expect("lost span", durable(db, led, false), "span s2 was acknowledged and is not there")
	exec(`INSERT INTO observations (project_id, trace_id, id, type, level) VALUES ('p1', 't1', 's2', 'span', 'ERROR')`)
	exec(`UPDATE traces SET observation_count = 2, error_count = 1 WHERE id = 't1'`)
	exec(`DELETE FROM scores WHERE id = 'sc1'`)
	expect("lost score", durable(db, led, false), "score sc1")
	led.deleteAcked["t1"] = true
	expect("undeleted trace", durable(db, led, false), "was deleted (200) and is still there")
	led.deleteAcked = map[string]bool{}

	// An erasure answered 202 that left a trace, and one that left half.
	led.claimed["u1"] = true
	led.erasureID["u1"] = "e1"
	expect("kept after an accepted erasure", durable(db, led, true), "the erasure e1 was accepted")

	// A hour the aggregator did not roll the last trace into.
	exec(`UPDATE stats_hourly SET count = 0`)
	expect("stale rollup", settled(db, "p1"), "stats_hourly of hour")

	// A migration the restart has not finished.
	exec(`DELETE FROM schema_migrations WHERE filename = (SELECT MAX(filename) FROM schema_migrations)`)
	expect("unfinished migrations", settled(db, "p1"), "has not finished the migrations")
}
