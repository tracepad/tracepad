package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// The per-user rollup at the store layer (spec 023). The properties are spec
// 013's, one dimension over: an hour recomputed from the raw rows equals a
// live per-user scan of it, a re-roll changes nothing, and the summary table
// is exactly the sums of the hourly rows under it.

// userSeed writes one trace of one user, with one generation in it.
type userSeed struct {
	n           int
	user        string
	session     string
	environment string
	model       string
	errored     bool
	cost        *float64
	latencyMs   int64
	// offsetSeconds places the trace inside its hour.
	hour          int64
	offsetSeconds int64
}

func seedUserTrace(t *testing.T, s *Store, projectID string, seed userSeed) {
	t.Helper()
	start := (seed.hour + seed.offsetSeconds) * 1e9
	trace := &model.Trace{
		ID: hexTrace(seed.n), Name: "run", Environment: seed.environment,
		UserID: seed.user, SessionID: seed.session,
	}
	observation := &model.Observation{
		TraceID: trace.ID, ID: hexSpan(seed.n), Type: model.TypeGeneration, Name: "call",
		Level:     model.LevelDefault,
		StartTime: start, EndTime: start + seed.latencyMs*1e6,
		Model: seed.model,
	}
	if seed.errored {
		observation.Level = model.LevelError
	}
	if seed.cost != nil {
		observation.CostDetails = map[string]any{"total": *seed.cost}
	}
	seedTrace(t, s, projectID, trace, observation)
}

func money(v float64) *float64 { return &v }

// usersFixture: two users in one hour, one of them across two environments,
// one session apiece, and a trace that names no user at all.
func usersFixture(t *testing.T, s *Store, projectID string) {
	t.Helper()
	seedUserTrace(t, s, projectID, userSeed{n: 1, user: "alice", session: "s-a",
		environment: "production", model: "claude-sonnet-5", cost: money(0.01), latencyMs: 250,
		hour: rollupHour, offsetSeconds: 10})
	seedUserTrace(t, s, projectID, userSeed{n: 2, user: "alice", session: "s-a",
		environment: "production", model: "claude-sonnet-5", errored: true, cost: money(0.02),
		latencyMs: 900, hour: rollupHour, offsetSeconds: 20})
	seedUserTrace(t, s, projectID, userSeed{n: 3, user: "alice", session: "s-b",
		environment: "staging", model: "gpt-4o-mini", latencyMs: 120,
		hour: rollupHour, offsetSeconds: 30})
	seedUserTrace(t, s, projectID, userSeed{n: 4, user: "bob", session: "s-c",
		environment: "production", model: "claude-sonnet-5", cost: money(0.005), latencyMs: 40,
		hour: rollupHour, offsetSeconds: 40})
	// A trace with no user id: it belongs to no row here (spec 023 #1).
	seedUserTrace(t, s, projectID, userSeed{n: 5, user: "", session: "s-d",
		environment: "production", model: "claude-sonnet-5", latencyMs: 60,
		hour: rollupHour, offsetSeconds: 50})
}

func userRows(t *testing.T, s *Store, projectID, userID string, hour int64) map[string]UserStatsRow {
	t.Helper()
	rows := map[string]UserStatsRow{}
	err := s.UsersRollupRows(projectID, userID, hour, hour+SecondsPerHour, "",
		func(row UserStatsRow) {
			rows[row.Environment+"|"+row.Release+"|"+row.Model] = row
		})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestUserHourEqualsALivePerUserScan is the property the read seam of
// `/stats?user_id=` rests on: the rolled tuples and their numbers are what a
// scan of the raw rows for that user says.
func TestUserHourEqualsALivePerUserScan(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	from, to := rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9
	for _, user := range []string{"alice", "bob"} {
		live := map[string]*StatsRow{}
		err := s.StatsSamples(project.ID, StatsFilter{
			From: &from, To: &to, UserID: user, GroupBy: GroupByEnvironment,
		}, func(sample StatsSample) {
			row := live[sample.Key]
			if row == nil {
				row = &StatsRow{}
				live[sample.Key] = row
			}
			row.Count++
			if sample.Errored {
				row.ErrorCount++
			}
			if sample.Cost != nil {
				total := *sample.Cost
				if row.TotalCost != nil {
					total += *row.TotalCost
				}
				row.TotalCost = &total
			}
			if sample.LatencyMs != nil {
				row.Latency.Add(*sample.LatencyMs)
			}
		})
		if err != nil {
			t.Fatal(err)
		}

		rolled := map[string]*StatsRow{}
		for _, row := range userRows(t, s, project.ID, user, rollupHour) {
			if row.Model != "" {
				continue
			}
			cell := rolled[row.Environment]
			if cell == nil {
				cell = &StatsRow{}
				rolled[row.Environment] = cell
			}
			cell.Count += row.Count
			cell.ErrorCount += row.ErrorCount
			if row.TotalCost != nil {
				total := *row.TotalCost
				if cell.TotalCost != nil {
					total += *cell.TotalCost
				}
				cell.TotalCost = &total
			}
			cell.Latency.Merge(row.Latency)
		}

		if len(rolled) != len(live) {
			t.Fatalf("%s: rolled %d environments, live scan says %d", user, len(rolled), len(live))
		}
		for key, want := range live {
			got := rolled[key]
			if got == nil {
				t.Fatalf("%s: no rolled row for %q", user, key)
			}
			if got.Count != want.Count || got.ErrorCount != want.ErrorCount {
				t.Errorf("%s/%s: rolled %d/%d, live %d/%d",
					user, key, got.Count, got.ErrorCount, want.Count, want.ErrorCount)
			}
			if (got.TotalCost == nil) != (want.TotalCost == nil) {
				t.Errorf("%s/%s: cost presence differs", user, key)
			}
			if got.Latency.Count() != want.Latency.Count() {
				t.Errorf("%s/%s: %d latencies rolled, %d live",
					user, key, got.Latency.Count(), want.Latency.Count())
			}
		}
	}
}

// TestAnonymousTracesProduceNoUserRows: the listing is of users, and a trace
// that named none belongs to nobody (spec 023 #1).
func TestAnonymousTracesProduceNoUserRows(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	var total int64
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(count), 0) FROM users_hourly WHERE project_id = ? AND model = ''`,
		project.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	// Four of the five fixtures carry a user id.
	if total != 4 {
		t.Errorf("rolled %d traces per user, want the 4 that named one", total)
	}
	users, err := s.Users(project.ID, UserFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.UserID == "" {
			t.Fatal("the empty user id has a summary row")
		}
	}
}

// TestUserSummaryEqualsItsHourlyRows: the summary is a recompute over the
// hourly rows and nothing else (spec 023 #3).
func TestUserSummaryEqualsItsHourlyRows(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	// A second hour for alice, so first_seen and last_seen have to move.
	seedUserTrace(t, s, project.ID, userSeed{n: 6, user: "alice", session: "s-e",
		environment: "production", model: "claude-sonnet-5", cost: money(0.03), latencyMs: 70,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 5})
	roll(t, s, project.ID, rollupHour)
	roll(t, s, project.ID, rollupHour+SecondsPerHour)

	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice == nil {
		t.Fatal("alice has no summary")
	}
	if alice.Traces != 4 || alice.ErrorCount != 1 {
		t.Errorf("alice: %d traces, %d errors; want 4 and 1", alice.Traces, alice.ErrorCount)
	}
	// Three sessions started: s-a, s-b in the first hour, s-e in the second.
	if alice.Sessions != 3 {
		t.Errorf("alice: %d sessions, want 3", alice.Sessions)
	}
	if alice.FirstSeen != rollupHour || alice.LastSeen != rollupHour+SecondsPerHour {
		t.Errorf("alice: window %d..%d, want %d..%d",
			alice.FirstSeen, alice.LastSeen, rollupHour, rollupHour+SecondsPerHour)
	}
	if alice.TotalCost == nil || *alice.TotalCost < 0.0599 || *alice.TotalCost > 0.0601 {
		t.Errorf("alice: cost %v, want 0.06", alice.TotalCost)
	}
	// The latency is not in the summary table: it is merged out of the hours
	// themselves, which is what the user page reads.
	rolled, err := s.UserRollup(project.ID, "alice", rollupHour+2*SecondsPerHour)
	if err != nil || rolled == nil {
		t.Fatalf("no rolled hours for alice: %v", err)
	}
	if rolled.Latency.Count() != 4 {
		t.Errorf("alice: %d latencies, want 4", rolled.Latency.Count())
	}
	if rolled.Traces != alice.Traces || rolled.Sessions != alice.Sessions {
		t.Errorf("the summary (%d/%d) and the hours (%d/%d) disagree",
			alice.Traces, alice.Sessions, rolled.Traces, rolled.Sessions)
	}
}

// TestTheUserPageDoesNotCountAnHourTwice: the erasure path re-rolls the hours
// it emptied in the same request (spec 013 #7), and one of those can be the
// hour in progress — above the watermark, where the live tail also reads. The
// rolled half of the user page is bounded by the watermark for exactly that
// reason.
func TestTheUserPageDoesNotCountAnHourTwice(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	// A watermark that stops before the fixture's hour, as it does for the
	// hour still in progress.
	roll(t, s, project.ID, rollupHour)

	rolled, err := s.UserRollup(project.ID, "alice", rollupHour)
	if err != nil {
		t.Fatal(err)
	}
	if rolled != nil {
		t.Errorf("an hour at or past the watermark was read from the rollup: %+v", rolled.UserRow)
	}
	// And below it, the same hour is the rollup's.
	behind, err := s.UserRollup(project.ID, "alice", rollupHour+SecondsPerHour)
	if err != nil || behind == nil {
		t.Fatalf("the hour behind the watermark is not in the rollup: %v", err)
	}
	if behind.Traces != 3 {
		t.Errorf("behind the watermark: %d traces, want 3", behind.Traces)
	}
}

// TestSessionIsCountedOnceInItsFirstHour: a session spanning two hours is one
// session, in the hour it started (spec 023 #1).
func TestSessionIsCountedOnceInItsFirstHour(t *testing.T) {
	s, project := readStore(t)
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "alice", session: "long",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour, offsetSeconds: 30})
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "alice", session: "long",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour)
	roll(t, s, project.ID, rollupHour+SecondsPerHour)

	first := userRows(t, s, project.ID, "alice", rollupHour)["production||"]
	second := userRows(t, s, project.ID, "alice", rollupHour+SecondsPerHour)["production||"]
	if first.SessionsStarted != 1 {
		t.Errorf("first hour started %d sessions, want 1", first.SessionsStarted)
	}
	if second.SessionsStarted != 0 {
		t.Errorf("second hour started %d sessions, want 0", second.SessionsStarted)
	}
	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil || alice == nil {
		t.Fatalf("no summary: %v", err)
	}
	if alice.Sessions != 1 {
		t.Errorf("summary counts %d sessions, want 1", alice.Sessions)
	}
}

// TestALaterEarlierTraceMovesTheSessionStart is Decision 2's reason for
// dirtying the other hours of a changed trace's session: the start moves, and
// re-rolling both hours has to correct both.
func TestALaterEarlierTraceMovesTheSessionStart(t *testing.T) {
	s, project := readStore(t)
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "alice", session: "moved",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour+SecondsPerHour)
	if got := userRows(t, s, project.ID, "alice", rollupHour+SecondsPerHour)["production||"]; got.SessionsStarted != 1 {
		t.Fatalf("the only hour started %d sessions, want 1", got.SessionsStarted)
	}

	// A trace of the same session arrives for an earlier hour.
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "alice", session: "moved",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour)
	roll(t, s, project.ID, rollupHour+SecondsPerHour)

	if got := userRows(t, s, project.ID, "alice", rollupHour)["production||"]; got.SessionsStarted != 1 {
		t.Errorf("the earlier hour started %d sessions, want 1", got.SessionsStarted)
	}
	if got := userRows(t, s, project.ID, "alice", rollupHour+SecondsPerHour)["production||"]; got.SessionsStarted != 0 {
		t.Errorf("the later hour still starts %d sessions, want 0", got.SessionsStarted)
	}
	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil || alice == nil {
		t.Fatalf("no summary: %v", err)
	}
	if alice.Sessions != 1 {
		t.Errorf("the session is counted %d times, want once", alice.Sessions)
	}
}

// TestTheDirtySetReachesTheSessionsOtherHours: the aggregator finds the hour a
// session start moved *out of* (spec 023 #2), which nothing else would dirty.
func TestTheDirtySetReachesTheSessionsOtherHours(t *testing.T) {
	s, project := readStore(t)
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "alice", session: "moved",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour+SecondsPerHour)

	// Everything written so far is behind the cutoff; then one trace of the
	// same session lands in an earlier hour.
	var cutoff int64
	if err := s.db.QueryRow(`SELECT MAX(updated_at) FROM traces WHERE project_id = ?`,
		project.ID).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "alice", session: "moved",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour, offsetSeconds: 30})

	dirty, err := s.dirtyHours(project.ID, cutoff, rollupHour+2*SecondsPerHour)
	if err != nil {
		t.Fatal(err)
	}
	var sawLater bool
	for _, hour := range dirty {
		if hour == rollupHour+SecondsPerHour {
			sawLater = true
		}
	}
	if !sawLater {
		// Without this the later hour keeps a `sessions_started` that is no
		// longer true, and nothing would ever revisit it.
		t.Errorf("dirty hours %v do not include the hour the session start left", dirty)
	}
}

// TestUserRollIsIdempotent: re-delivery then a re-roll leaves one set of rows,
// which is why the job is a delete and an insert (spec 013 #3).
func TestUserRollIsIdempotent(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	before, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// The same spans again, as a retry would send them.
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	after, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.Traces != after.Traces || before.Sessions != after.Sessions {
		t.Errorf("re-delivery changed the summary: %d/%d then %d/%d",
			before.Traces, before.Sessions, after.Traces, after.Sessions)
	}
}

// TestAFrozenHourKeepsItsUserRows: the freeze is one rule in one place, and it
// covers both tables because they are written by one job (spec 013 #11).
func TestAFrozenHourKeepsItsUserRows(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	// Every trace of the hour goes, as the sweep would take them.
	if _, err := s.db.Exec(`DELETE FROM traces WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}

	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// "Now" is two days past the hour, so it is outside the window.
	now := (rollupHour + 2*24*3600) * int64(1e9)
	job := &statsRoll{ProjectID: project.ID, Hour: rollupHour, Now: now}
	if err := writer.Submit(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !job.Frozen {
		t.Fatal("the hour was not frozen")
	}
	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice == nil || alice.Traces != 3 {
		t.Errorf("a frozen hour lost its user rows: %v", alice)
	}
}

// TestTheSweepOfTheLastRowForgetsTheUser (spec 023 #3, edge cases): the summary
// covers the retained history, and a user with nothing retained is gone.
func TestTheSweepOfTheLastRowForgetsTheUser(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	seedUserTrace(t, s, project.ID, userSeed{n: 6, user: "alice", session: "s-e",
		environment: "production", model: "m", latencyMs: 70,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 5})
	roll(t, s, project.ID, rollupHour)
	roll(t, s, project.ID, rollupHour+SecondsPerHour)

	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	// Sweep the first hour: bob was only ever in it, alice was not.
	sweep := &usersRollupSweep{ProjectID: project.ID, Before: rollupHour + SecondsPerHour}
	if err := writer.Submit(context.Background(), sweep); err != nil {
		t.Fatal(err)
	}
	bob, err := s.UserSummaryRow(project.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob != nil {
		t.Errorf("bob survived the sweep of his only hour: %v", bob)
	}
	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice == nil {
		t.Fatal("alice went with the sweep of an hour she is not alone in")
	}
	if alice.Traces != 1 || alice.FirstSeen != rollupHour+SecondsPerHour {
		t.Errorf("alice: %d traces from %d; want 1 from %d",
			alice.Traces, alice.FirstSeen, rollupHour+SecondsPerHour)
	}
}

// TestAPassSummarizesEveryUserItRolled: the aggregator defers the summary to
// the end of the pass (spec 023 #3), so a pass over several hours has to leave
// the same numbers a per-hour recompute would.
func TestAPassSummarizesEveryUserItRolled(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	seedUserTrace(t, s, project.ID, userSeed{n: 6, user: "alice", session: "s-e",
		environment: "production", model: "m", cost: money(0.03), latencyMs: 70,
		hour: rollupHour + SecondsPerHour, offsetSeconds: 5})

	passAt(t, s, time.Unix(rollupHour+4*SecondsPerHour, 0))

	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil || alice == nil {
		t.Fatalf("alice has no summary after a pass: %v", err)
	}
	if alice.Traces != 4 || alice.Sessions != 3 {
		t.Errorf("alice = %d traces, %d sessions; want 4 and 3", alice.Traces, alice.Sessions)
	}
	if alice.FirstSeen != rollupHour || alice.LastSeen != rollupHour+SecondsPerHour {
		t.Errorf("alice: window %d..%d, want %d..%d",
			alice.FirstSeen, alice.LastSeen, rollupHour, rollupHour+SecondsPerHour)
	}
	bob, err := s.UserSummaryRow(project.ID, "bob")
	if err != nil || bob == nil || bob.Traces != 1 {
		t.Errorf("bob = %v, err = %v; want one trace", bob, err)
	}
}

// TestAnAlreadyRolledProjectIsBackfilled is what migration 0013's last line
// exists for: on an upgrade the watermark is already past the whole history,
// so nothing is a forward roll and nothing is dirty — the two new tables would
// stay empty for ever on a project that has stopped receiving traffic.
func TestAnAlreadyRolledProjectIsBackfilled(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	passAt(t, s, time.Unix(rollupHour+4*SecondsPerHour, 0))

	// The state an upgrade finds: `stats_hourly` rolled, the watermark past
	// it, and the per-user tables empty.
	for _, table := range []string{"users_hourly", "users"} {
		if _, err := s.db.Exec(`DELETE FROM ` + table); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.RolledUntil == 0 {
		t.Fatal("the fixture did not roll; the test proves nothing")
	}

	// The migration's one line.
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, time.Unix(rollupHour+4*SecondsPerHour, 0))

	users, err := s.Users(project.ID, UserFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("the backfill produced %d users, want alice and bob", len(users))
	}
	// And the watermark did not move backwards, which is what keeps
	// `/api/v1/stats` answering from the rollup throughout (spec 013 #12).
	after, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RolledUntil < before.RolledUntil {
		t.Errorf("rolled_until went from %d to %d", before.RolledUntil, after.RolledUntil)
	}
}

// TestUserListingSortsAndPages covers the four sorts, the tie-break, and a
// keyset page in both directions at a limit of one.
func TestUserListingSortsAndPages(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	for _, sortBy := range UserSorts {
		page, err := s.Users(project.ID, UserFilter{Sort: sortBy, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 2 {
			t.Fatalf("%s: %d users, want 2", sortBy, len(page))
		}
		// Walk the listing one row at a time and check the whole of it
		// comes back in the same order.
		var walked []string
		var cursor *UserCursor
		for {
			rows, err := s.Users(project.ID, UserFilter{Sort: sortBy, Limit: 1, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				break
			}
			walked = append(walked, rows[0].UserID)
			cursor = &UserCursor{Key: UserCursorKey(sortBy, rows[0]), UserID: rows[0].UserID}
		}
		if len(walked) != 2 || walked[0] != page[0].UserID || walked[1] != page[1].UserID {
			t.Errorf("%s: walked %v, whole page is %s, %s",
				sortBy, walked, page[0].UserID, page[1].UserID)
		}

		// And backwards from the last row: the page before it is the first.
		back, err := s.Users(project.ID, UserFilter{
			Sort: sortBy, Limit: 1, Backward: true,
			After: &UserCursor{Key: UserCursorKey(sortBy, page[1]), UserID: page[1].UserID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(back) != 1 || back[0].UserID != page[0].UserID {
			t.Errorf("%s: the page before the last is %v, want %s", sortBy, back, page[0].UserID)
		}
	}
}

// TestUncostedUsersSortLast is the edge case named in the spec: a NULL cost is
// last under `sort=cost`, and the keyset walks through it rather than stopping
// at it.
func TestUncostedUsersSortLast(t *testing.T) {
	s, project := readStore(t)
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "costed", session: "s1",
		environment: "production", model: "m", cost: money(0.5), latencyMs: 10,
		hour: rollupHour, offsetSeconds: 10})
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "free-a", session: "s2",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour, offsetSeconds: 20})
	seedUserTrace(t, s, project.ID, userSeed{n: 3, user: "free-b", session: "s3",
		environment: "production", model: "m", latencyMs: 10,
		hour: rollupHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour)

	var walked []string
	var cursor *UserCursor
	for range 4 {
		rows, err := s.Users(project.ID, UserFilter{Sort: UsersByCost, Limit: 1, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		walked = append(walked, rows[0].UserID)
		cursor = &UserCursor{Key: UserCursorKey(UsersByCost, rows[0]), UserID: rows[0].UserID}
	}
	want := []string{"costed", "free-a", "free-b"}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Errorf("cost order %v, want %v", walked, want)
	}
}

// TestUserPrefixIsCaseSensitive: a prefix, not a search, and not a LIKE
// (spec 023 #5).
func TestUserPrefixIsCaseSensitive(t *testing.T) {
	s, project := readStore(t)
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "acme:1", session: "s1",
		environment: "production", model: "m", latencyMs: 10, hour: rollupHour, offsetSeconds: 10})
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "acme:2", session: "s2",
		environment: "production", model: "m", latencyMs: 10, hour: rollupHour, offsetSeconds: 20})
	seedUserTrace(t, s, project.ID, userSeed{n: 3, user: "ACME:3", session: "s3",
		environment: "production", model: "m", latencyMs: 10, hour: rollupHour, offsetSeconds: 30})
	roll(t, s, project.ID, rollupHour)

	rows, err := s.Users(project.ID, UserFilter{Prefix: "acme:", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("prefix acme: matched %d, want the 2 lower-case ones", len(rows))
	}
	count, err := s.CountUsers(project.ID, UserFilter{Prefix: "acme:"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count %d, want 2", count)
	}
}

// TestUserSortsRideTheirIndexes is the plan check spec 023's Testing asks for
// by name: each sort seeks its own index rather than sorting the table.
func TestUserSortsRideTheirIndexes(t *testing.T) {
	s, project := readStore(t)
	for sortBy, index := range map[string]string{
		UsersByLastSeen: "idx_users_last_seen",
		UsersByTraces:   "idx_users_traces",
		UsersByCost:     "idx_users_cost",
		UsersByErrors:   "idx_users_errors",
	} {
		query, args := userQuery(project.ID, UserFilter{
			Sort: sortBy, Limit: 50,
			After: &UserCursor{Key: "1", UserID: "u"},
		})
		plan, err := s.explainQueryPlan(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, index) {
			t.Errorf("%s does not ride %s:\n%s", sortBy, index, joined)
		}
		if strings.Contains(joined, "USE TEMP B-TREE FOR ORDER BY") {
			t.Errorf("%s sorts rather than seeks:\n%s", sortBy, joined)
		}
	}
}

// TestSessionStartSeeksTheSessionIndex is the plan check the session-start
// predicate needs by name (spec 003 #25's method), and it exists because the
// shipped form did the opposite: the planner took the `OR` apart into a
// MULTI-INDEX OR over `idx_traces_timestamp` and walked every older trace in
// the project, per candidate trace — 13.35 s against 0.01 s for one hour at 20k
// traces (found in review of PR #42).
//
// Nothing in this store runs `ANALYZE`, so there is no `sqlite_stat1` to make
// the planner prefer the selective term on its own. The `+` is what settles it,
// and this test is what keeps it there.
func TestSessionStartSeeksTheSessionIndex(t *testing.T) {
	s, project := readStore(t)
	for _, tc := range []struct {
		name  string
		query string
		args  []any
	}{
		{"the hour's session starts",
			`SELECT t.user_id, COUNT(*) FROM traces t
			 WHERE ` + sessionStartCondition + `
			   AND t.timestamp >= ? AND t.timestamp < ?
			 GROUP BY t.user_id`,
			[]any{project.ID, int64(0), int64(1)}},
		{"one user's session starts",
			`SELECT t.timestamp FROM traces t
			 WHERE ` + sessionStartCondition + `
			   AND t.user_id = ? AND t.timestamp >= ? AND t.timestamp < ?`,
			[]any{project.ID, "u", int64(0), int64(1)}},
		{"the hours a changed session touched",
			dirtySessionHoursQuery,
			[]any{SecondsPerHour, SecondsPerHour, project.ID, project.ID, int64(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := s.explainQueryPlan(tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, "idx_traces_session") {
				t.Errorf("the session index is not used:\n%s", joined)
			}
			// The shape the `+` exists to prevent: an index chosen for the
			// comparison rather than for the session.
			if strings.Contains(joined, "MULTI-INDEX OR") {
				t.Errorf("the planner split the comparison into an index scan:\n%s", joined)
			}
		})
	}
}

// TestTheLiveTailIsExact: the user page's other half (spec 023 #4).
func TestTheLiveTailIsExact(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)

	tail, err := s.UserTail(project.ID, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tail.Traces != 3 || tail.ErrorCount != 1 || tail.Sessions != 2 {
		t.Errorf("tail: %d traces, %d errors, %d sessions; want 3, 1, 2",
			tail.Traces, tail.ErrorCount, tail.Sessions)
	}
	if tail.LastSeen != (rollupHour+30)*1e9 {
		t.Errorf("tail last seen %d, want %d", tail.LastSeen, (rollupHour+30)*1e9)
	}
	// Nothing was ever filed under this id.
	nobody, err := s.UserTail(project.ID, "nobody", 0)
	if err != nil {
		t.Fatal(err)
	}
	if nobody.Traces != 0 {
		t.Errorf("an unseen id has %d traces", nobody.Traces)
	}
}

// TestErasureTakesThePerUserRows (spec 023 #10): they are *about* the user, so
// they go outright rather than being re-rolled.
func TestErasureTakesThePerUserRows(t *testing.T) {
	s, project := readStore(t)
	usersFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	erase := &UserDataErase{ProjectID: project.ID, UserID: "alice", Confirm: "alice", Limit: 100}
	if err := writer.Submit(context.Background(), erase); err != nil {
		t.Fatal(err)
	}

	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice != nil {
		t.Errorf("alice still has a summary after erasure: %v", alice)
	}
	if rows := userRows(t, s, project.ID, "alice", rollupHour); len(rows) != 0 {
		t.Errorf("alice still has %d rolled rows", len(rows))
	}
	// Bob is untouched: an erasure is about one user.
	bob, err := s.UserSummaryRow(project.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob == nil || bob.Traces != 1 {
		t.Errorf("bob was caught by alice's erasure: %v", bob)
	}
}
