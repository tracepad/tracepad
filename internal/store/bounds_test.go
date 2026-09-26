package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Spec 043, the store's half of PR 1: lookups that do not queue behind the
// writer, the database's conditions told apart from the batch's own failures,
// one counting rule for costs and token counts, sums that saturate, a rollup
// pass that one hour cannot stop, and the migration that repairs what is
// already stored.

// A new pool connection used to run `PRAGMA auto_vacuum=INCREMENTAL`, which
// writes the header and so wants the write lock: behind a long commit it
// waited out busy_timeout and failed, and the guard answered that failure
// `401` (spec 043 #1). Held here by a transaction on a connection of its own —
// the lock the writer holds while it commits — with every idle connection
// reserved, so that the lookup has to open a new one.
func TestAKeyLookupDoesNotWaitForTheWriter(t *testing.T) {
	s, _ := readStore(t)
	ctx := context.Background()

	holder, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer holder.ExecContext(ctx, `ROLLBACK`)

	var reserved []*sql.Conn
	defer func() {
		for _, conn := range reserved {
			conn.Close()
		}
	}()
	for s.db.Stats().Idle > 0 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		reserved = append(reserved, conn)
	}
	opened := s.db.Stats().OpenConnections

	start := time.Now()
	project, err := s.ProjectBySecret(ctx, "tp-sk-test")
	took := time.Since(start)
	if err != nil || project == nil {
		t.Fatalf("lookup = %v, %v after %v, want the project while the writer holds its lock",
			project, err, took.Round(time.Millisecond))
	}
	if took > time.Second {
		t.Errorf("the lookup took %v behind the writer's lock, want well under a second", took)
	}
	if s.db.Stats().OpenConnections <= opened {
		t.Error("the lookup reused a connection, so the test proves nothing about a new one")
	}
}

// A fresh file takes its vacuum mode on the connection that creates its first
// table, so no open ever has to rewrite it (spec 043 #1, spec 005 #5). Before,
// the DSN set it on every connection; without either, the first open would
// VACUUM a database it had just created.
func TestAFreshFileIsIncrementalFromItsFirstTable(t *testing.T) {
	var logged bytes.Buffer
	previous := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = previous })

	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != incrementalVacuumMode {
		t.Errorf("auto_vacuum = %d on a fresh file, want %d", mode, incrementalVacuumMode)
	}
	if strings.Contains(logged.String(), "rewriting the database") {
		t.Errorf("a fresh file was rewritten to change its vacuum mode:\n%s", logged.String())
	}
}

// codedError is the driver's error as this package sees it: a message and the
// one method `Condition` reads.
type codedError struct{ code int }

func (e codedError) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e codedError) Code() int     { return e.code }

// The database's conditions are the ones that pass; everything else is the
// batch's own (spec 043 #2). Extended codes fold into their primary one, and
// the error is found however deeply it is wrapped.
func TestConditionTellsTheDatabaseFromTheBatch(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{codedError{13}, "SQLITE_FULL"},
		{fmt.Errorf("commit write transaction: %w", codedError{5 | 2<<8}), "SQLITE_BUSY"}, // BUSY_SNAPSHOT
		{fmt.Errorf("store payload: %w", codedError{10 | 4<<8}), "SQLITE_IOERR"},          // IOERR_WRITE
		{codedError{6}, "SQLITE_LOCKED"},
		{codedError{7}, "SQLITE_NOMEM"},
		{codedError{14}, "SQLITE_CANTOPEN"},
		{codedError{19 | 8<<8}, ""}, // CONSTRAINT_UNIQUE: the batch's own
		{codedError{1}, ""},
		{errors.New("encode json column"), ""},
		{nil, ""},
	} {
		name, ok := Condition(tc.err)
		if name != tc.want || ok != (tc.want != "") {
			t.Errorf("Condition(%v) = %q, %v, want %q", tc.err, name, ok, tc.want)
		}
	}
}

// Sums in Go hold at the limit instead of wrapping or reaching an infinity
// (spec 043 #6).
func TestSumsSaturate(t *testing.T) {
	big := int64(math.MaxInt64)
	var sum *int64
	addCount(&sum, &big)
	addCount(&sum, &big)
	if sum == nil || *sum != math.MaxInt64 {
		t.Errorf("MaxInt64 + MaxInt64 = %v, want it held at MaxInt64", sum)
	}
	small := int64(math.MinInt64)
	sum = nil
	addCount(&sum, &small)
	addCount(&sum, &small)
	if *sum != math.MinInt64 {
		t.Errorf("MinInt64 + MinInt64 = %d, want it held at MinInt64", *sum)
	}
	three, four := int64(3), int64(4)
	sum = nil
	addCount(&sum, &three)
	addCount(&sum, &four)
	if *sum != 7 {
		t.Errorf("3 + 4 = %d", *sum)
	}

	value := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	sumOf := func(costs ...float64) *float64 {
		var sum *float64
		for _, cost := range costs {
			sum = AddCost(sum, cost)
		}
		return sum
	}
	for _, tc := range []struct {
		costs []float64
		want  any
	}{
		{[]float64{math.MaxFloat64, math.MaxFloat64}, math.MaxFloat64},
		{[]float64{-math.MaxFloat64, -math.MaxFloat64}, -math.MaxFloat64},
		{[]float64{0.25, 0.5}, 0.75},
		// Not costs: they add nothing, and nothing else is no data —
		// not a NaN, and not the zero two opposite infinities made.
		{[]float64{math.NaN()}, nil},
		{[]float64{math.Inf(1), math.Inf(-1)}, nil},
		{[]float64{0.25, math.NaN(), math.Inf(1)}, 0.25},
		{nil, nil},
	} {
		if got := value(sumOf(tc.costs...)); got != tc.want {
			t.Errorf("AddCost over %v = %v, want %v", tc.costs, got, tc.want)
		}
	}
	nan := math.NaN()
	if got := value(AddCost(&nan, 1)); got != 1.0 {
		t.Errorf("AddCost(NaN, 1) = %v, want a stored NaN to count as nothing", got)
	}
}

// The counting rule (spec 043 #4), read by every reader: a cost counts when
// `total` is a number within ±10^12, a token count when it is a number from 0
// to 10^9. Everything else is kept as sent and counted as no data — and a
// string `total`, which used to fail the scan and with it the hour's roll,
// rolls like any other.
func TestTheCountingRule(t *testing.T) {
	s, project := readStore(t)
	base := rollupHour * 1e9
	seed := func(n int, cost map[string]any, usage map[string]any) {
		start := base + int64(n)*1e9
		trace := &model.Trace{ID: hexTrace(n), Environment: "production", UserID: "u1"}
		seedTrace(t, s, project.ID, trace, &model.Observation{
			TraceID: trace.ID, ID: hexSpan(n), Type: model.TypeGeneration,
			Level: model.LevelDefault, Model: "m",
			StartTime: start, EndTime: start + 1e6,
			CostDetails: cost, Usage: usage,
		})
	}
	seed(1, map[string]any{"total": 1.5e308}, map[string]any{"input_tokens": 1e300, "output_tokens": 5})
	seed(2, map[string]any{"total": "abc"}, map[string]any{"input_tokens": -3, "output_tokens": 5})
	seed(3, map[string]any{"total": 2e12}, map[string]any{"input_tokens": 1e9, "output_tokens": 1e9 + 1})
	seed(4, map[string]any{"total": 0.25}, map[string]any{"input_tokens": 10})

	totals := map[string]*float64{}
	for n := 1; n <= 4; n++ {
		row, err := s.Trace(project.ID, hexTrace(n))
		if err != nil {
			t.Fatal(err)
		}
		totals[hexTrace(n)] = row.TotalCost
	}
	for n := 1; n <= 3; n++ {
		if cost := totals[hexTrace(n)]; cost != nil {
			t.Errorf("trace %d total_cost = %v, want no data", n, *cost)
		}
	}
	if cost := totals[hexTrace(4)]; cost == nil || *cost != 0.25 {
		t.Errorf("trace 4 total_cost = %v, want 0.25", cost)
	}
	// Kept as sent, not dropped (spec 002 #11).
	observations, err := s.Observations(project.ID, hexTrace(2), WithIO)
	if err != nil || len(observations) != 1 || observations[0].CostDetails["total"] != "abc" {
		t.Fatalf("observation = %+v, %v, want cost_details kept as sent", observations, err)
	}

	passAt(t, s, afterTheHour())
	rows := rolledRows(t, s, project.ID, rollupHour)
	want := map[string]struct {
		cost          float64
		input, output int64
	}{
		"production||":  {0.25, 1e9 + 10, 10},
		"production||m": {0.25, 1e9 + 10, 10},
	}
	if len(rows) != len(want) {
		t.Fatalf("rolled %v, want %v", keysOf(rows), keysOf(want))
	}
	for key, w := range want {
		row := rows[key]
		if row.TotalCost == nil || *row.TotalCost != w.cost {
			t.Errorf("%s cost = %v, want %v", key, row.TotalCost, w.cost)
		}
		if row.Tokens.Input == nil || *row.Tokens.Input != w.input {
			t.Errorf("%s input = %v, want %d: 1e300 and -3 are not counts, 1e9 is", key, row.Tokens.Input, w.input)
		}
		if row.Tokens.Output == nil || *row.Tokens.Output != w.output {
			t.Errorf("%s output = %v, want %d: 1e9+1 is not a count", key, row.Tokens.Output, w.output)
		}
	}
	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil <= rollupHour {
		t.Errorf("rolled_until = %d, want past the hour a string cost sits in", state.RolledUntil)
	}
}

// countingFailure fails the roll of every hour with one error and counts the
// attempts.
type countingFailure struct {
	inner    jobSubmitter
	err      error
	attempts int
}

func (f *countingFailure) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*statsRoll); ok {
		f.attempts++
		return f.err
	}
	return f.inner.Submit(ctx, job)
}

// A failure that is not the hour's own — the writer's queue full, the database
// in trouble, the pass out of time — ends the project's pass at the first hour
// it meets, as a shutdown always did: every hour after would meet it too, and
// going on logged an error for each and rolled none (spec 043 #24).
func TestAFailureThatIsNotTheHoursStopsThePass(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the writer is busy", ErrWriterBusy},
		{"the disk is full", fmt.Errorf("begin write transaction: %w", codedError{13})},
		{"the pass ran out of time", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, project := readStore(t)
			for n := range 5 {
				start := (rollupHour+int64(n)*SecondsPerHour)*1e9 + 1e9
				trace := &model.Trace{ID: hexTrace(n + 1), Environment: "production"}
				seedTrace(t, s, project.ID, trace, &model.Observation{
					TraceID: trace.ID, ID: hexSpan(n + 1), Type: model.TypeSpan,
					Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
				})
			}
			at := time.Unix(rollupHour+10*SecondsPerHour, 0)
			// Rolled once, then every hour made dirty again: both loops
			// meet the failure.
			passAt(t, s, at)
			if _, err := s.db.Exec(`UPDATE traces SET updated_at = ?`, time.Now().Add(time.Hour).UnixNano()); err != nil {
				t.Fatal(err)
			}
			before, err := s.RollupState(project.ID)
			if err != nil {
				t.Fatal(err)
			}

			writer, err := s.NewWriter(quickWrites)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			failing := &countingFailure{inner: writer, err: tc.err}
			aggregator := s.NewAggregator(failing, RollupOptions{
				Interval: DefaultRollupInterval, Now: func() time.Time { return at },
			})
			if err := aggregator.Pass(context.Background()); err == nil {
				t.Error("the pass reported no failure")
			}
			if failing.attempts != 1 {
				t.Errorf("the pass tried %d hours, want it to stop at the first", failing.attempts)
			}
			after, err := s.RollupState(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Errorf("rollup state moved from %+v to %+v on a pass that rolled nothing", before, after)
			}
		})
	}
}

// A dirty hour that fails holds `last_pass` for a bounded number of passes,
// not for ever (spec 043 #24): held for good, every later pass re-rolled all
// that had changed since, for one hour that cannot be rolled.
func TestAHourThatAlwaysFailsHoldsLastPassABoundedTime(t *testing.T) {
	s, project := readStore(t)
	hour := rollupHour
	seed := func(n int) {
		start := hour*1e9 + int64(n)*1e9
		trace := &model.Trace{ID: hexTrace(n), Environment: "production"}
		seedTrace(t, s, project.ID, trace, &model.Observation{
			TraceID: trace.ID, ID: hexSpan(n), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
		})
	}
	seed(1)
	passAt(t, s, time.Unix(hour+2*SecondsPerHour, 0))
	seed(2) // a late span: the hour is dirty

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	seam := &failingHours{inner: writer, hours: map[int64]bool{hour: true}}
	clock := time.Now().Add(time.Hour)
	aggregator := s.NewAggregator(seam, RollupOptions{
		Interval: DefaultRollupInterval, Now: func() time.Time { return clock },
	})
	lastPass := func() int64 {
		state, err := s.RollupState(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		return state.LastPass
	}
	held := lastPass()
	for attempt := 1; attempt <= maxHourAttempts; attempt++ {
		clock = clock.Add(time.Minute)
		if err := aggregator.Pass(context.Background()); err == nil {
			t.Fatalf("pass %d reported no failure", attempt)
		}
		moved := lastPass() != held
		if want := attempt == maxHourAttempts; moved != want {
			t.Fatalf("after failed pass %d last_pass moved = %v, want %v", attempt, moved, want)
		}
	}
	// Given up: the next pass no longer finds the hour, and succeeds.
	clock = clock.Add(time.Minute)
	if err := aggregator.Pass(context.Background()); err != nil {
		t.Errorf("the pass after giving up = %v, want the hour no longer retried", err)
	}

	// A span that lands in the hour again makes it dirty again, with its
	// attempts back.
	seed(3)
	// Stamped on the pass's clock, which runs ahead of the wall.
	if _, err := s.db.Exec(`UPDATE traces SET updated_at = ? WHERE id = ?`, clock.UnixNano(), hexTrace(3)); err != nil {
		t.Fatal(err)
	}
	delete(seam.hours, hour)
	clock = clock.Add(time.Minute)
	if err := aggregator.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rolledRows(t, s, project.ID, hour)["production||"].Count; got != 3 {
		t.Errorf("the hour holds %d traces once it can be rolled, want 3", got)
	}
}

// failingHours is the aggregator's writer with a seam: the roll of the hours
// it names fails, everything else goes through.
type failingHours struct {
	inner jobSubmitter
	hours map[int64]bool
}

func (f *failingHours) Submit(ctx context.Context, job WriteJob) error {
	if roll, ok := job.(*statsRoll); ok && f.hours[roll.Hour] {
		return fmt.Errorf("injected failure for hour %d", roll.Hour)
	}
	return f.inner.Submit(ctx, job)
}

// One failing hour costs that hour and nothing else (spec 043 #8). Before, the
// project's pass returned at the first failure, before the watermark moved,
// so one poisoned hour stopped its statistics for good.
func TestOneFailingHourDoesNotStopThePass(t *testing.T) {
	hours := []int64{rollupHour, rollupHour + SecondsPerHour, rollupHour + 2*SecondsPerHour}
	seedHours := func(t *testing.T, s *Store, projectID string, first int) {
		for i, hour := range hours {
			start := hour*1e9 + int64(first)*1e9
			trace := &model.Trace{ID: hexTrace(first + i), Environment: "production"}
			seedTrace(t, s, projectID, trace, &model.Observation{
				TraceID: trace.ID, ID: hexSpan(first + i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
			})
		}
	}
	pass := func(t *testing.T, s *Store, failing map[int64]bool, at time.Time) error {
		t.Helper()
		writer, err := s.NewWriter(quickWrites)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		aggregator := s.NewAggregator(&failingHours{inner: writer, hours: failing}, RollupOptions{
			Interval: DefaultRollupInterval, Now: func() time.Time { return at },
		})
		return aggregator.Pass(context.Background())
	}
	count := func(t *testing.T, s *Store, projectID string, hour int64) int64 {
		t.Helper()
		return rolledRows(t, s, projectID, hour)["production||"].Count
	}

	t.Run("the forward roll stops at the failing hour, not before", func(t *testing.T) {
		s, project := readStore(t)
		seedHours(t, s, project.ID, 1)
		at := time.Unix(hours[2]+2*SecondsPerHour, 0)

		if err := pass(t, s, map[int64]bool{hours[1]: true}, at); err == nil {
			t.Error("the pass reported no failure, want the failed hour in its error")
		}
		if got := count(t, s, project.ID, hours[0]); got != 1 {
			t.Errorf("the hour before the failure holds %d traces, want it rolled", got)
		}
		state, err := s.RollupState(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.RolledUntil != hours[1] {
			t.Errorf("rolled_until = %d, want the failing hour %d: past what was rolled, never past the failure",
				state.RolledUntil, hours[1])
		}

		if err := pass(t, s, nil, at); err != nil {
			t.Fatal(err)
		}
		for _, hour := range hours {
			if got := count(t, s, project.ID, hour); got != 1 {
				t.Errorf("hour %d holds %d traces after the retry, want 1", hour, got)
			}
		}
	})

	t.Run("the dirty loop rolls the other hours and keeps last_pass", func(t *testing.T) {
		s, project := readStore(t)
		seedHours(t, s, project.ID, 1)
		if err := pass(t, s, nil, time.Unix(hours[2]+2*SecondsPerHour, 0)); err != nil {
			t.Fatal(err)
		}
		before, err := s.RollupState(project.ID)
		if err != nil {
			t.Fatal(err)
		}

		// A late trace in the first and the last hour dirties both; the
		// first, the one the loop used to stop at, fails. The clock is
		// past the late traces' stamps, so a moved `last_pass` would hide
		// them from every later pass.
		seedHours(t, s, project.ID, 11)
		later := time.Now().Add(time.Hour)
		if err := pass(t, s, map[int64]bool{hours[0]: true}, later); err == nil {
			t.Error("the pass reported no failure, want the failed hour in its error")
		}
		if got := count(t, s, project.ID, hours[2]); got != 2 {
			t.Errorf("the last hour holds %d traces, want the late one rolled past the failure", got)
		}
		if got := count(t, s, project.ID, hours[0]); got != 1 {
			t.Errorf("the failing hour holds %d traces, want it untouched", got)
		}
		after, err := s.RollupState(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.LastPass != before.LastPass {
			t.Errorf("last_pass moved from %d to %d over a failed dirty hour", before.LastPass, after.LastPass)
		}

		if err := pass(t, s, nil, later); err != nil {
			t.Fatal(err)
		}
		if got := count(t, s, project.ID, hours[0]); got != 2 {
			t.Errorf("the failed hour holds %d traces after the retry, want the late one rolled", got)
		}
	})
}

// Migration 0023 spells out the counting rule the store builds (spec 043 #9):
// the same cost expression, and every token key with the same range. Two
// copies of one rule are held together here, or the repair would count what
// the store does not.
func TestMigration0023SpellsTheCountingRule(t *testing.T) {
	body, err := migrationFS.ReadFile("migrations/0023_repair_numbers.sql")
	if err != nil {
		t.Fatal(err)
	}
	space := regexp.MustCompile(`\s+`)
	normal := func(s string) string {
		s = space.ReplaceAllString(s, " ")
		return strings.ReplaceAll(strings.ReplaceAll(s, "( ", "("), " )", ")")
	}
	migration := normal(string(body))
	if !strings.Contains(migration, normal(costExpr("o.cost_details"))) {
		t.Errorf("the migration does not carry costExpr as the store builds it:\n%s", normal(costExpr("o.cost_details")))
	}
	for _, keys := range tokenClasses {
		for _, key := range keys {
			want := normal(`json_extract(o.usage, '$.` + key + `') NOT BETWEEN 0 AND ` + maxCountedTokens)
			if !strings.Contains(migration, want) {
				t.Errorf("the migration does not stamp traces whose %q is out of range", key)
			}
		}
	}
}

// The repair (spec 043 #9): a database whose aggregates an earlier version
// poisoned — an infinite trace total, a string total summed to zero, infinite
// and negative sums in the rollup, a frozen cell nothing can recompute — comes
// out of migration 0023 and one pass with the numbers a fresh ingest of the
// same spans produces, and the frozen cell NULL.
func TestMigration0023RepairsPoisonedNumbers(t *testing.T) {
	at := afterTheHour()
	frozenHour := rollupHour - 30*24*SecondsPerHour
	seedAll := func(t *testing.T, s *Store, projectID string) {
		t.Helper()
		base := rollupHour * 1e9
		generation := func(trace string, n int, cost, usage map[string]any) *model.Observation {
			start := base + int64(n)*1e9
			return &model.Observation{
				TraceID: trace, ID: hexSpan(n), Type: model.TypeGeneration,
				Level: model.LevelDefault, Model: "m",
				StartTime: start, EndTime: start + 1e6, CostDetails: cost, Usage: usage,
			}
		}
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(1), Environment: "production", UserID: "u1"},
			generation(hexTrace(1), 1, map[string]any{"total": 1.5e308}, nil),
			generation(hexTrace(1), 2, map[string]any{"total": 1.5e308}, nil))
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(2), Environment: "production", UserID: "u1"},
			generation(hexTrace(2), 3, map[string]any{"total": "abc"}, nil))
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(3), Environment: "production", UserID: "u2"},
			generation(hexTrace(3), 4, nil, map[string]any{"input_tokens": 1e300, "output_tokens": 7}))
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(4), Environment: "production", UserID: "u2"},
			generation(hexTrace(4), 5, map[string]any{"total": 0.5}, map[string]any{"input_tokens": 10}))
	}

	// What the same spans make on this version.
	reference, project := readStore(t)
	seedAll(t, reference, project.ID)
	passAt(t, reference, at)

	// The same spans, rolled, and then poisoned the way an earlier version
	// left them — and not dirty, as if every pass since had seen them.
	path := freshDB(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	poisoned, err := s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	seedAll(t, s, poisoned.ID)
	passAt(t, s, at)
	for _, statement := range []string{
		`UPDATE traces SET updated_at = 1`,
		`UPDATE traces SET total_cost = 9e999 WHERE id = '` + hexTrace(1) + `'`,
		`UPDATE traces SET total_cost = 0.0 WHERE id = '` + hexTrace(2) + `'`,
		`UPDATE stats_hourly SET total_cost = 9e999, input_tokens = -8`,
		`UPDATE users_hourly SET total_cost = 9e999`,
		`UPDATE users SET total_cost = 9e999`,
		fmt.Sprintf(`INSERT INTO stats_hourly (project_id, hour, environment, release, model, count, error_count,
		                                       total_cost, latency, input_tokens, output_tokens, cache_read_tokens)
		             VALUES ('%s', %d, 'production', '', '', 3, 0, 9e999, '[]', -3, 5, 9223372036854775807)`, poisoned.ID, frozenHour),
		`DELETE FROM schema_migrations WHERE filename = '0023_repair_numbers.sql'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	s.Close()

	start := time.Now()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	t.Logf("migration 0023 over the fixture took %v", time.Since(start).Round(time.Millisecond))
	passAt(t, s, at)

	for _, query := range []string{
		`SELECT id, quote(total_cost) FROM traces ORDER BY id`,
		`SELECT hour, environment, release, model, count, error_count, quote(total_cost), latency,
		        quote(input_tokens), quote(output_tokens), quote(cache_read_tokens)
		   FROM stats_hourly WHERE hour = ` + fmt.Sprint(rollupHour) + ` ORDER BY 1, 2, 3, 4`,
		`SELECT hour, user_id, environment, release, model, count, error_count, quote(total_cost), latency
		   FROM users_hourly ORDER BY 1, 2, 3, 4, 5`,
		`SELECT user_id, traces, error_count, quote(total_cost), sessions, first_seen, last_seen
		   FROM users ORDER BY user_id`,
	} {
		got, want := dumpRows(t, s.db, query), dumpRows(t, reference.db, query)
		if !slices.Equal(got, want) {
			t.Errorf("%s\n got: %v\nwant: %v", space(query), got, want)
		}
	}
	frozen := dumpRows(t, s.db, fmt.Sprintf(
		`SELECT quote(total_cost), quote(input_tokens), quote(output_tokens), quote(cache_read_tokens)
		   FROM stats_hourly WHERE hour = %d`, frozenHour))
	// The cache-read sum is what `1e300` cast to an int64 left: positive, and
	// more than three traces can count.
	if !slices.Equal(frozen, []string{"NULL|NULL|5|NULL"}) {
		t.Errorf("the frozen cell = %v, want its impossible sums NULL and its sound one kept", frozen)
	}
}

func space(query string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(query, " ")
}

// dumpRows reads every row of a query as text, columns joined by `|`.
func dumpRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		cells := make([]string, len(values))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			cells[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(cells, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
