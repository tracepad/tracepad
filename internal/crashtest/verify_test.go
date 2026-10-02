package crashtest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// openDB opens a database file for reading what the server left. A snapshot
// is opened read-write only because SQLite must be able to recover its WAL;
// nothing here writes.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// violations collects what a check found; a check never stops at the first,
// because the pattern of what broke is half of the diagnosis.
type violations []string

func (v *violations) add(format string, args ...any) {
	*v = append(*v, fmt.Sprintf(format, args...))
}

func (v violations) cap() violations {
	const limit = 25
	if len(v) > limit {
		return append(v[:limit:limit], fmt.Sprintf("… and %d more", len(v)-limit))
	}
	return v
}

// rows runs a query that lists offenders and adds one violation per row.
func (v *violations) rows(db *sql.DB, what, query string, args ...any) {
	r, err := db.Query(query, args...)
	if err != nil {
		v.add("%s: the check itself failed: %v", what, err)
		return
	}
	defer r.Close()
	cols, _ := r.Columns()
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.Scan(ptrs...); err != nil {
			v.add("%s: scan: %v", what, err)
			return
		}
		parts := make([]string, len(cols))
		for i, c := range cols {
			parts[i] = fmt.Sprintf("%s=%v", c, vals[i])
		}
		v.add("%s: %s", what, strings.Join(parts, " "))
	}
}

// structural is what must hold of the file at *any* commit boundary, whatever
// the process was doing when it died: the file is sound, and no transaction
// is half of itself. It reads the database as it stands, mid-erasure and
// mid-sweep included, because both go in whole chunks.
func structural(db *sql.DB) violations {
	var v violations

	var verdict string
	rowsOK := func(pragma string) {
		r, err := db.Query(pragma)
		if err != nil {
			v.add("%s: %v", pragma, err)
			return
		}
		defer r.Close()
		for r.Next() {
			if err := r.Scan(&verdict); err != nil {
				v.add("%s: %v", pragma, err)
				return
			}
			if verdict != "ok" {
				v.add("%s: %s", pragma, verdict)
			}
		}
	}
	rowsOK(`PRAGMA integrity_check`)

	v.rows(db, "foreign_key_check", `PRAGMA foreign_key_check`)

	// A kill in the middle of the first start leaves a database migrated as
	// far as it got — each migration is a transaction of its own — and the
	// next start finishes it. Nothing below can be asked of such a file.
	if missing := unappliedMigrations(db); len(missing) > 0 {
		return v
	}

	// A trace's aggregates are a function of its observations, refreshed in
	// the transaction that wrote them (spec 002 #7, #22).
	v.rows(db, "trace aggregates disagree with the observations",
		`SELECT t.project_id, t.id, t.observation_count AS stored, c.n AS actual,
		        t.error_count AS stored_errors, c.errors AS actual_errors
		   FROM traces t
		   JOIN (SELECT project_id, trace_id, COUNT(*) AS n,
		                SUM(level = 'ERROR') AS errors
		           FROM observations GROUP BY project_id, trace_id) c
		     ON c.project_id = t.project_id AND c.trace_id = t.id
		  WHERE t.observation_count != c.n OR t.error_count != c.errors`)
	v.rows(db, "a trace with no observation",
		`SELECT t.project_id, t.id, t.observation_count FROM traces t
		  WHERE NOT EXISTS (SELECT 1 FROM observations o
		                     WHERE o.project_id = t.project_id AND o.trace_id = t.id)`)
	v.rows(db, "an observation with no trace",
		`SELECT o.project_id, o.trace_id, COUNT(*) AS observations FROM observations o
		  WHERE NOT EXISTS (SELECT 1 FROM traces t
		                     WHERE t.project_id = o.project_id AND t.id = o.trace_id)
		  GROUP BY o.project_id, o.trace_id`)

	// Everything deleted with a trace goes with it, in one transaction.
	v.rows(db, "a score whose trace is gone",
		`SELECT s.project_id, s.id, s.trace_id FROM scores s
		  WHERE s.trace_id IS NOT NULL
		    AND NOT EXISTS (SELECT 1 FROM traces t
		                     WHERE t.project_id = s.project_id AND t.id = s.trace_id)`)

	// The search index is written and deleted in the same transaction as the
	// rows it indexes (spec 011): an entry for a trace that is not there, or
	// an entry without its FTS row, or an FTS row without its entry, is a
	// half of one.
	v.rows(db, "a search entry whose trace is gone",
		`SELECT e.project_id, e.trace_id, COUNT(*) AS entries FROM search_entries e
		  WHERE NOT EXISTS (SELECT 1 FROM traces t
		                     WHERE t.project_id = e.project_id AND t.id = e.trace_id)
		  GROUP BY e.project_id, e.trace_id`)
	v.rows(db, "a search entry with no FTS row",
		`SELECT e.id, e.trace_id FROM search_entries e
		  WHERE e.id NOT IN (SELECT id FROM search_fts_docsize)`)
	v.rows(db, "an FTS row with no search entry",
		`SELECT d.id FROM search_fts_docsize d
		  WHERE d.id NOT IN (SELECT id FROM search_entries)`)

	return v
}

// migrationFiles are the names the binary under test carries, read from the
// source tree this package sits in.
func migrationFiles() []string {
	names, _ := filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	for i, name := range names {
		names[i] = filepath.Base(name)
	}
	sort.Strings(names)
	return names
}

// unappliedMigrations are the migrations the file has not recorded.
func unappliedMigrations(db *sql.DB) []string {
	applied := map[string]bool{}
	if r, err := db.Query(`SELECT filename FROM schema_migrations`); err == nil {
		for r.Next() {
			var name string
			if r.Scan(&name) == nil {
				applied[name] = true
			}
		}
		r.Close()
	}
	var missing []string
	for _, name := range migrationFiles() {
		if !applied[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// settled is what must hold once the server has run for a while after the
// kill and ingest has stopped: the derived tables have caught up with the
// rows they summarize. Rolled hours of `main` — nothing retains it — must
// count what a scan counts.
func settled(db *sql.DB, mainID string) violations {
	var v violations
	if missing := unappliedMigrations(db); len(missing) > 0 {
		v.add("the restart has not finished the migrations: %v", missing)
		return v
	}
	var until sql.NullInt64
	err := db.QueryRow(`SELECT rolled_until FROM stats_rollup WHERE project_id = ?`, mainID).Scan(&until)
	if err != nil && err != sql.ErrNoRows {
		v.add("stats_rollup: %v", err)
		return v
	}
	if !until.Valid {
		return v
	}

	type cell struct{ traces, failed int64 }
	read := func(query string) map[int64]cell {
		out := map[int64]cell{}
		r, err := db.Query(query, mainID, until.Int64)
		if err != nil {
			v.add("rollup comparison: %v", err)
			return out
		}
		defer r.Close()
		for r.Next() {
			var hour int64
			var c cell
			if err := r.Scan(&hour, &c.traces, &c.failed); err != nil {
				v.add("rollup comparison: %v", err)
				return out
			}
			out[hour] = c
		}
		return out
	}
	scanned := read(`SELECT (timestamp / 1000000000 / 3600) * 3600 AS hour,
	                        COUNT(*), COALESCE(SUM(error_count > 0), 0)
	                   FROM traces
	                  WHERE project_id = ? AND (timestamp / 1000000000 / 3600) * 3600 < ?
	                  GROUP BY hour`)
	rolled := read(`SELECT hour, SUM(count), SUM(error_count) FROM stats_hourly
	                 WHERE project_id = ? AND model = '' AND hour < ?
	                 GROUP BY hour`)

	hours := map[int64]bool{}
	for h := range scanned {
		hours[h] = true
	}
	for h := range rolled {
		hours[h] = true
	}
	ordered := make([]int64, 0, len(hours))
	for h := range hours {
		ordered = append(ordered, h)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, h := range ordered {
		if scanned[h] != rolled[h] {
			v.add("stats_hourly of hour %d counts %+v, a scan of traces counts %+v (rolled until %d)",
				h, rolled[h], scanned[h], until.Int64)
		}
	}
	return v
}

// durable asserts the promises: an answer the server gave before it was
// killed is on disk when it is back. What was acknowledged as deleted is gone;
// what was acknowledged as written is there unless a request of the test's own
// — a delete sent, an erasure begun — may have taken it since.
//
// withErasures adds the part that only holds after the restart has finished
// the erasures it resumed.
func durable(db *sql.DB, led *ledger, withErasures bool) violations {
	led.mu.Lock()
	defer led.mu.Unlock()
	var v violations

	have := map[string]map[string]bool{} // trace -> span ids
	users := map[string]string{}
	r, err := db.Query(`SELECT o.trace_id, o.id FROM observations o
	                     JOIN projects p ON p.id = o.project_id WHERE p.name = 'main'`)
	if err != nil {
		v.add("read the observations: %v", err)
		return v
	}
	for r.Next() {
		var trace, span string
		if err := r.Scan(&trace, &span); err != nil {
			r.Close()
			v.add("read the observations: %v", err)
			return v
		}
		if have[trace] == nil {
			have[trace] = map[string]bool{}
		}
		have[trace][span] = true
	}
	r.Close()
	r, err = db.Query(`SELECT t.id, COALESCE(t.user_id, '') FROM traces t
	                    JOIN projects p ON p.id = t.project_id WHERE p.name = 'main'`)
	if err != nil {
		v.add("read the traces: %v", err)
		return v
	}
	for r.Next() {
		var id, user string
		if err := r.Scan(&id, &user); err != nil {
			r.Close()
			v.add("read the traces: %v", err)
			return v
		}
		users[id] = user
	}
	r.Close()
	scores := map[string]bool{}
	r, err = db.Query(`SELECT s.id FROM scores s JOIN projects p ON p.id = s.project_id WHERE p.name = 'main'`)
	if err != nil {
		v.add("read the scores: %v", err)
		return v
	}
	for r.Next() {
		var id string
		if err := r.Scan(&id); err != nil {
			r.Close()
			v.add("read the scores: %v", err)
			return v
		}
		scores[id] = true
	}
	r.Close()

	for id, rec := range led.traces {
		switch {
		case led.deleteAcked[id]:
			if _, ok := users[id]; ok || len(have[id]) > 0 {
				v.add("trace %s was deleted (200) and is still there: row=%v observations=%d", id, ok, len(have[id]))
			}
		case led.deleteSent[id] || led.claimed[rec.user]:
			// Taken, or perhaps taken: what may be said of it is in the
			// structural check, and for an erasure below.
		default:
			got, ok := users[id]
			if !ok {
				v.add("trace %s was acknowledged (%d spans) and has no row", id, len(rec.spans))
				continue
			}
			if got != rec.user {
				v.add("trace %s has user %q, was acknowledged with %q", id, got, rec.user)
			}
			for span := range rec.spans {
				if !have[id][span] {
					v.add("trace %s: span %s was acknowledged and is not there", id, span)
				}
			}
		}
	}
	for score, trace := range led.scores {
		rec := led.traces[trace]
		if led.deleteSent[trace] || (rec != nil && led.claimed[rec.user]) {
			continue
		}
		if !scores[score] {
			v.add("score %s on trace %s was acknowledged and is not there", score, trace)
		}
	}
	for id := range led.deleteAcked {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM scores WHERE trace_id = ?`, id).Scan(&n)
		if n != 0 {
			v.add("trace %s was deleted (200) and %d of its scores remain", id, n)
		}
		_ = db.QueryRow(`SELECT COUNT(*) FROM search_entries WHERE trace_id = ?`, id).Scan(&n)
		if n != 0 {
			v.add("trace %s was deleted (200) and %d of its search entries remain", id, n)
		}
	}

	if !withErasures {
		return v
	}
	// An erasure either never reached the database — and the user's traces
	// are all there — or was committed, and, run to its end, took every one.
	// What may not be is some of each. An erasure that was answered 202 is
	// the second kind for certain.
	for user := range led.claimed {
		var present, total int
		for id, rec := range led.traces {
			if rec.user != user || led.deleteAcked[id] || led.deleteSent[id] {
				continue
			}
			total++
			if _, ok := users[id]; ok {
				present++
			}
		}
		if present > 0 && present != total {
			v.add("user %s: %d of %d traces remain after an erasure that has run to its end", user, present, total)
		}
		if led.erasureID[user] != "" && present > 0 {
			v.add("user %s: the erasure %s was accepted and %d traces remain", user, led.erasureID[user], present)
		}
		if present == 0 {
			var rest int
			_ = db.QueryRow(`SELECT COUNT(*) FROM traces WHERE user_id = ?`, user).Scan(&rest)
			if rest != 0 {
				v.add("user %s: %d trace rows still carry the id after the erasure took every acknowledged one", user, rest)
			}
		}
	}
	return v
}
