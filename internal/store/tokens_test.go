package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// Tokens in the statistics (spec 031 #1–#5): three sums per cell, read off
// each observation's `usage` under one closed list of spellings, into both
// units, on both sides of the seam.

// tokensFixture seeds one hour whose observations carry usage under every
// spelling of Decision 1, plus the shapes the sums must not be fooled by.
func tokensFixture(t *testing.T, s *Store, projectID string) {
	t.Helper()
	base := rollupHour * 1e9

	observation := func(n int, trace, name string, usage map[string]any) *model.Observation {
		start := base + int64(n)*int64(1e9)
		return &model.Observation{
			TraceID: trace, ID: hexSpan(n), Type: model.TypeGeneration, Name: "call",
			Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
			Model: name, Usage: usage,
		}
	}
	seed := func(n int, environment, release string, observations ...*model.Observation) {
		seedTrace(t, s, projectID, &model.Trace{
			ID: hexTrace(n), Name: "run", Environment: environment, Release: release,
		}, observations...)
	}

	// The three spellings, one per observation, and a trace whose two
	// observations sum into one trace-unit cell. The second of them names
	// two spellings of one number, and the first in the list wins.
	seed(1, "production", "2026.8.30",
		observation(1, hexTrace(1), "claude-sonnet-5", map[string]any{
			"input_tokens": 100, "output_tokens": 10, "cache_read_input_tokens": 5}),
		observation(7, hexTrace(1), "claude-sonnet-5", map[string]any{
			"input_tokens": 1, "prompt_tokens": 999, "output_tokens": 1}))
	seed(2, "production", "2026.8.30",
		observation(2, hexTrace(2), "gpt-4o-mini", map[string]any{
			"prompt_tokens": 200, "completion_tokens": 20, "input_cached_tokens": 7}))
	seed(3, "staging", "",
		observation(3, hexTrace(3), "claude-sonnet-5", map[string]any{
			"input": 300, "output": 30, "cache_read_tokens": 9}))
	// No usage at all: contributes nothing, and nothing is not zero.
	seed(4, "staging", "",
		observation(4, hexTrace(4), "claude-sonnet-5", nil))
	// A count that is not a number is not a count — and the first key
	// present decides, so the class is NULL rather than read from the next
	// spelling along (spec 031 #1).
	seed(5, "staging", "",
		observation(5, hexTrace(5), "gpt-4o-mini", map[string]any{
			"input_tokens": "lots", "prompt_tokens": 5, "output_tokens": 40}))
	// Usage on an observation that names no model: not in the model unit,
	// and the rollup's observation cells are the rows the trace-unit sum
	// is made of (spec 031 #5), so it is in neither.
	seed(6, "staging", "",
		observation(6, hexTrace(6), "", map[string]any{"input_tokens": 1000}))
	// An environment in which nothing carried a count: every sum NULL.
	seed(8, "dev", "",
		observation(8, hexTrace(8), "claude-sonnet-5", nil))
}

func count(n int64) *int64 { return &n }

func tokensEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func describeTokens(t Tokens) string {
	var parts []string
	for _, n := range []*int64{t.Input, t.Output, t.CacheRead} {
		if n == nil {
			parts = append(parts, "NULL")
		} else {
			parts = append(parts, fmt.Sprint(*n))
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func expectTokens(t *testing.T, what string, got, want Tokens) {
	t.Helper()
	if !tokensEqual(got.Input, want.Input) || !tokensEqual(got.Output, want.Output) ||
		!tokensEqual(got.CacheRead, want.CacheRead) {
		t.Errorf("%s: tokens %s, want %s", what, describeTokens(got), describeTokens(want))
	}
}

// One hour rolled: each spelling read under its class, the first key present
// winning, a trace-unit cell the sum over its traces' observations, and a
// cell with no count staying NULL rather than zero.
func TestTokensRollIntoBothCells(t *testing.T) {
	s, project := readStore(t)
	tokensFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	rows := rolledRows(t, s, project.ID, rollupHour)
	for key, want := range map[string]Tokens{
		// The model cells.
		"production|2026.8.30|claude-sonnet-5": {count(101), count(11), count(5)},
		"production|2026.8.30|gpt-4o-mini":     {count(200), count(20), count(7)},
		"staging||claude-sonnet-5":             {count(300), count(30), count(9)},
		"staging||gpt-4o-mini":                 {nil, count(40), nil},
		"dev||claude-sonnet-5":                 {nil, nil, nil},
		// The trace-unit cells: the same observations, summed over the
		// traces of the environment and release.
		"production|2026.8.30|": {count(301), count(31), count(12)},
		"staging||":             {count(300), count(70), count(9)},
		"dev||":                 {nil, nil, nil},
	} {
		row, ok := rows[key]
		if !ok {
			t.Errorf("row %q is missing from the rollup: %v", key, keysOf(rows))
			continue
		}
		expectTokens(t, "row "+key, row.Tokens, want)
	}
}

// The live scan over the same rows says what the rollup says, per bucket, for
// every grouping — the property the seam rests on (spec 013 #5), one column
// further.
func TestLiveTokensEqualTheRolledOnes(t *testing.T) {
	s, project := readStore(t)
	tokensFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	from, to := rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9
	for _, tc := range []struct {
		groupBy string
		key     func(StatsRow) (string, bool)
	}{
		{GroupByHour, func(r StatsRow) (string, bool) { return "2026-08-26T10:00:00Z", r.Model == "" }},
		{GroupByDay, func(r StatsRow) (string, bool) { return "2026-08-26", r.Model == "" }},
		{GroupByEnvironment, func(r StatsRow) (string, bool) { return r.Environment, r.Model == "" }},
		{GroupByRelease, func(r StatsRow) (string, bool) { return r.Release, r.Model == "" }},
		{GroupByModel, func(r StatsRow) (string, bool) { return r.Model, r.Model != "" }},
		// The whole window under the empty key (spec 034 #3): `GROUP BY ''`
		// is one group, and the aggregate says so.
		{GroupByTotal, func(r StatsRow) (string, bool) { return "", r.Model == "" }},
	} {
		t.Run(tc.groupBy, func(t *testing.T) {
			filter := StatsFilter{From: &from, To: &to, GroupBy: tc.groupBy}
			live := map[string]*Tokens{}
			at := func(key string) *Tokens {
				sum := live[key]
				if sum == nil {
					sum = &Tokens{}
					live[key] = sum
				}
				return sum
			}
			// The two halves of the live answer, folded the way the
			// server folds them.
			if err := s.StatsSamples(t.Context(), project.ID, filter, func(sample StatsSample) {
				at(sample.Key).Add(sample.Tokens)
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.StatsTokens(t.Context(), project.ID, filter, func(sum StatsTokenSum) {
				at(sum.Key).Add(sum.Tokens)
			}); err != nil {
				t.Fatal(err)
			}

			rolled := map[string]*Tokens{}
			for _, row := range rolledRows(t, s, project.ID, rollupHour) {
				key, ok := tc.key(row)
				if !ok {
					continue
				}
				sum := rolled[key]
				if sum == nil {
					sum = &Tokens{}
					rolled[key] = sum
				}
				sum.Add(row.Tokens)
			}

			if len(rolled) != len(live) {
				t.Fatalf("rolled %d buckets, the live scan has %d: %v vs %v",
					len(rolled), len(live), keysOf(rolled), keysOf(live))
			}
			counted := false
			for key, want := range live {
				counted = counted || want.Input != nil
				got := rolled[key]
				if got == nil {
					t.Errorf("bucket %q is missing from the rollup", key)
					continue
				}
				expectTokens(t, "bucket "+key, *got, *want)
			}
			if !counted {
				t.Error("no live bucket carries an input count, so the comparison proves nothing")
			}
		})
	}
}

// The trace-unit aggregate is bounded by the hour's traces and joined to
// their observations; driving off `observations` instead would read every
// observation the project holds on every chart.
func TestTheTokensAggregateSeeksTheTraceIndex(t *testing.T) {
	s, project := readStore(t)
	tokensFixture(t, s, project.ID)

	from, to := rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9
	query, args := statsTokensQuery(project.ID, StatsFilter{From: &from, To: &to, GroupBy: GroupByDay})
	plan, err := s.explainQueryPlan(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	for _, index := range []string{"idx_traces_timestamp", "idx_observations_trace"} {
		if !strings.Contains(joined, index) {
			t.Errorf("the tokens aggregate does not use %s:\n%s", index, joined)
		}
	}
}

// The backfill of Decision 3, exercised where it can be: a database whose
// hours are rolled and whose token columns are NULL is exactly the position
// an upgrade leaves, and `last_pass = 0` is the one line that repairs it.
func TestResettingLastPassBackfillsTheTokens(t *testing.T) {
	s, project := readStore(t)
	tokensFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)
	forgetTokens(t, s, project.ID)

	// A pass over a clean watermark leaves the hour alone: nothing is dirty.
	passAt(t, s, base.Add(2*DefaultRollupInterval))
	if row := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]; row.Tokens.Input != nil {
		t.Fatalf("a pass over a clean watermark rewrote the hour; it is not dirty")
	}

	// The migration's own line.
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, base.Add(4*DefaultRollupInterval))

	row := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
	expectTokens(t, "the backfilled hour", row.Tokens, Tokens{count(301), count(31), count(12)})
}

// The known limit of Decision 3: an hour past the retention window whose
// observations are gone is frozen, and keeps NULL tokens for ever — there is
// nothing left to sum, and the rows it has are not demolished either.
func TestAFrozenHourKeepsNullTokens(t *testing.T) {
	s, project := readStore(t)
	tokensFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	passAt(t, s, afterTheHour())
	forgetTokens(t, s, project.ID)
	before := rolledRows(t, s, project.ID, rollupHour)

	// The sweep's work, done here directly, and a window the hour is past.
	for _, statement := range []string{
		`DELETE FROM observations WHERE project_id = ?`,
		`DELETE FROM traces WHERE project_id = ?`,
		`UPDATE projects SET retention_days = 1 WHERE id = ?`,
	} {
		if _, err := s.db.Exec(statement, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	after := rolledRows(t, s, project.ID, rollupHour)
	if len(after) != len(before) {
		t.Fatalf("the frozen hour was demolished: %d rows, was %d", len(after), len(before))
	}
	for key, row := range after {
		expectTokens(t, "frozen row "+key, row.Tokens, Tokens{})
	}
}

// forgetTokens puts the rollup in the state migration 0018 finds: every hour
// rolled, nothing dirty, and the three columns NULL.
func forgetTokens(t *testing.T, s *Store, projectID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE stats_hourly SET input_tokens = NULL, output_tokens = NULL, cache_read_tokens = NULL
		 WHERE project_id = ?`, projectID); err != nil {
		t.Fatal(err)
	}
	if rows := rolledRows(t, s, projectID, rollupHour); len(rows) == 0 {
		t.Fatal("the statistics were not rolled, so the test proves nothing")
	}
}

// Migration 0018 itself, over a database the previous release left: the
// three columns arrive nullable and `last_pass` is reset so the next pass
// re-rolls every retained hour (Decision 3).
func TestMigration0018ResetsThePassCutoff(t *testing.T) {
	path := openAtSchema(t, "0017_accounts.sql")
	func() {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range []string{
			`INSERT INTO projects (id, name) VALUES ('p1', 'app')`,
			`INSERT INTO stats_rollup (project_id, rolled_until, last_pass) VALUES ('p1', 1787742000, 1787742000000000000)`,
			`INSERT INTO stats_hourly (project_id, hour, environment, release, model, count, error_count, total_cost, latency)
			 VALUES ('p1', 1787738400, 'production', '', '', 3, 0, NULL, '[]')`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()

	state, err := s.RollupState(t.Context(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastPass != 0 {
		t.Errorf("last_pass = %d after 0018, want 0: the next pass must find every hour dirty", state.LastPass)
	}
	if state.RolledUntil != 1787742000 {
		t.Errorf("rolled_until = %d after 0018, want it untouched", state.RolledUntil)
	}
	var rows int
	err = s.StatsRollupRows(t.Context(), "p1", rollupHour, rollupHour+SecondsPerHour, nil, func(row StatsRow) {
		rows++
		expectTokens(t, "the pre-0018 row", row.Tokens, Tokens{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("read %d rows after the upgrade, want the one that was there", rows)
	}
}

// openAtSchema builds a database as a previous release left it: every
// migration up to and including `last` applied and recorded, nothing after.
func openAtSchema(t *testing.T, last string) string {
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
	names, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range names {
		name := entry.Name()
		if name > last {
			break
		}
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
