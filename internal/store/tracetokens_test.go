package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Tokens where people look (spec 049): five classes on the trace, maintained
// on write, backfilled by migration 0033, rolled into both per-user tables,
// and summed per class everywhere — never one class into another.

// generation is one observation of a trace in the fixture hour, with usage.
func generation(trace string, n int, modelName string, usage map[string]any) *model.Observation {
	start := rollupHour*1e9 + int64(n)*1e9
	return &model.Observation{
		TraceID: trace, ID: hexSpan(n), Type: model.TypeGeneration, Name: "call",
		Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
		Model: modelName, Usage: usage,
	}
}

func traceTokens(t *testing.T, s *Store, projectID, id string) Tokens {
	t.Helper()
	row, err := s.Trace(t.Context(), projectID, id)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatalf("trace %s is not there", id)
	}
	return row.Tokens
}

// Every key of every class lands in its class and nowhere else; the first key
// present decides; a key in no list lands in none; a count outside 0..10^9 is
// no count (spec 049 #1, spec 043 #4).
func TestEveryKeyLandsInItsClass(t *testing.T) {
	s, project := readStore(t)
	n := 0
	for class, keys := range tokenClasses {
		for _, key := range keys {
			n++
			t.Run(key, func(t *testing.T) {
				id := hexTrace(n)
				seedTrace(t, s, project.ID, &model.Trace{ID: id, Environment: "production"},
					generation(id, n, "m", map[string]any{key: 42}))
				got := traceTokens(t, s, project.ID, id)
				for i, field := range got.fields() {
					switch {
					case i == class && (*field == nil || **field != 42):
						t.Errorf("%q: class %s = %v, want 42", key, tokenColumnNames[i], describeTokens(got))
					case i != class && *field != nil:
						t.Errorf("%q also landed in %s: %s", key, tokenColumnNames[i], describeTokens(got))
					}
				}
			})
		}
	}

	for _, tc := range []struct {
		name  string
		usage map[string]any
		want  Tokens
	}{
		// The first key of the list that is present decides, whatever
		// order the object carries them in.
		{"the first present wins", map[string]any{
			"output_reasoning_tokens": 9, "reasoning_tokens": 4,
			"cache_creation_tokens": 7, "cache_creation_input_tokens": 3,
		}, Tokens{Reasoning: count(4), CacheWrite: count(3)}},
		// A key in no list is in no class: `total` is not a class, and
		// neither is a spelling nobody listed.
		{"an unknown key", map[string]any{"total_tokens": 500, "audio_tokens": 20, "reasoning": 3}, Tokens{}},
		// Out of range is no count — and the class is not read from the
		// next spelling along.
		{"out of range", map[string]any{
			"reasoning_tokens": -1, "output_reasoning_tokens": 5,
			"cache_creation_input_tokens": 2e9,
		}, Tokens{}},
		// A dotted key is one key, not a path into an object.
		{"a nested object is not a dotted key", map[string]any{
			"cache_read": map[string]any{"input_tokens": 11},
		}, Tokens{}},
	} {
		n++
		t.Run(tc.name, func(t *testing.T) {
			id := hexTrace(n)
			seedTrace(t, s, project.ID, &model.Trace{ID: id, Environment: "production"},
				generation(id, n, "m", tc.usage))
			expectTokens(t, tc.name, traceTokens(t, s, project.ID, id), tc.want)
		})
	}
}

// No class is ever added to another (spec 049 #1, #3): an observation with 100
// output and 40 reasoning bills 100, whichever convention its provider follows,
// and the statistics' output series carries 100 too.
func TestReasoningIsNeverAddedToOutput(t *testing.T) {
	s, project := readStore(t)
	seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(1), Environment: "production"},
		generation(hexTrace(1), 1, "m", map[string]any{
			"input_tokens": 10, "output_tokens": 100, "reasoning_tokens": 40,
			"cache_read_input_tokens": 5, "cache_creation_input_tokens": 6,
		}))
	got := traceTokens(t, s, project.ID, hexTrace(1))
	expectTokens(t, "the trace", got, Tokens{count(10), count(100), count(5), count(40), count(6)})
	if billed := got.Billed(); billed == nil || *billed != 110 {
		t.Errorf("billed = %v, want input + output = 110", billed)
	}

	roll(t, s, project.ID, rollupHour)
	for key, row := range rolledRows(t, s, project.ID, rollupHour) {
		expectTokens(t, "rolled "+key, row.Tokens, Tokens{count(10), count(100), count(5), count(40), count(6)})
	}
}

func TestBilledReadsAMissingClassAsZero(t *testing.T) {
	for _, tc := range []struct {
		tokens Tokens
		want   *int64
	}{
		{Tokens{}, nil},
		{Tokens{CacheRead: count(9), Reasoning: count(9)}, nil},
		{Tokens{Input: count(7)}, count(7)},
		{Tokens{Output: count(8)}, count(8)},
		{Tokens{Input: count(7), Output: count(8)}, count(15)},
	} {
		if got := tc.tokens.Billed(); !tokensEqual(got, tc.want) {
			t.Errorf("Billed(%s) = %v, want %v", describeTokens(tc.tokens), got, tc.want)
		}
	}
}

// The trace's columns on write (spec 049 #2): recomputed in each ingest
// transaction over every observation the trace has so far, so a trace
// delivered in three batches reads the sum of its counted observations after
// each; usage on an observation that names no model counts nowhere (spec 031
// #12); a trace with no counted usage reads NULL.
func TestTheTraceSumsItsTokensOnEveryBatch(t *testing.T) {
	s, project := readStore(t)
	id := hexTrace(1)
	trace := &model.Trace{ID: id, Environment: "production"}

	seedTrace(t, s, project.ID, trace)
	expectTokens(t, "no observations yet", traceTokens(t, s, project.ID, id), Tokens{})

	seedTrace(t, s, project.ID, trace,
		generation(id, 1, "m", map[string]any{"input_tokens": 100, "output_tokens": 10}),
		generation(id, 2, "", map[string]any{"input_tokens": 1000, "output_tokens": 1000}))
	expectTokens(t, "after the first batch", traceTokens(t, s, project.ID, id),
		Tokens{Input: count(100), Output: count(10)})

	seedTrace(t, s, project.ID, trace,
		generation(id, 3, "m", map[string]any{"prompt_tokens": 50, "reasoning_tokens": 7}))
	expectTokens(t, "after the second batch", traceTokens(t, s, project.ID, id),
		Tokens{Input: count(150), Output: count(10), Reasoning: count(7)})

	// A re-delivered span is the same span (spec 002 #5): it replaces its
	// own counts rather than adding to them.
	seedTrace(t, s, project.ID, trace,
		generation(id, 1, "m", map[string]any{"input_tokens": 200, "output_tokens": 20}),
		generation(id, 4, "m", map[string]any{"cache_write_tokens": 3}))
	expectTokens(t, "after the third batch", traceTokens(t, s, project.ID, id),
		Tokens{Input: count(250), Output: count(20), Reasoning: count(7), CacheWrite: count(3)})

	other := hexTrace(2)
	seedTrace(t, s, project.ID, &model.Trace{ID: other, Environment: "production"},
		generation(other, 5, "m", nil), generation(other, 6, "", map[string]any{"input_tokens": 5}))
	expectTokens(t, "a trace with no counted usage", traceTokens(t, s, project.ID, other), Tokens{})
}

// tokenMonth seeds a synthetic month for the agreement test: three users and
// the anonymous, two environments, every class, some observations without a
// model, some traces without usage, spread across 30 days.
func tokenMonth(t *testing.T, s *Store, projectID string, lastHour int64) (from int64) {
	t.Helper()
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	users := []string{"alice", "bob", "carol", ""}
	spellings := [][]string{
		{"input_tokens", "output_tokens", "cache_read_input_tokens", "reasoning_tokens", "cache_creation_input_tokens"},
		{"prompt_tokens", "completion_tokens", "input_cached_tokens", "output_reasoning_tokens", "input_cache_creation"},
		{"input", "output", "cache_read_tokens", "reasoning.output_tokens", "cache_creation_tokens"},
	}
	from = lastHour - 30*24*SecondsPerHour
	for n := 1; n <= 240; n++ {
		hour := from + int64(n)*3*SecondsPerHour
		start := hour*1e9 + int64(n%3600)*1e9
		id := hexTrace(n)
		trace := &model.Trace{ID: id, Environment: []string{"production", "staging"}[n%2],
			UserID: users[n%len(users)], SessionID: fmt.Sprintf("s%d", n%7)}
		var observations []*model.Observation
		for k := range 3 {
			o := &model.Observation{TraceID: id, ID: hexSpan(n*10 + k), Type: model.TypeGeneration,
				Level: model.LevelDefault, StartTime: start + int64(k)*1e6, EndTime: start + int64(k+1)*1e6,
				Model: []string{"a", "b", ""}[(n+k)%3]}
			if n%5 != 0 {
				keys := spellings[(n+k)%len(spellings)]
				usage := map[string]any{}
				for c, key := range keys {
					if (n+k+c)%4 != 0 {
						usage[key] = n*10 + k + c
					}
				}
				o.Usage = usage
			}
			observations = append(observations, o)
		}
		if err := writer.Submit(t.Context(), &IngestBatch{ProjectID: projectID,
			Traces: []*model.Trace{trace}, Observations: observations}); err != nil {
			t.Fatal(err)
		}
	}
	return from
}

// Agreement (spec 049 #2, #5): per class, the trace columns summed over a
// range equal the statistics' trace-unit cells, and per user the per-user
// rows and the user page — on both sides of the watermark.
func TestTraceColumnsAgreeWithEveryRollup(t *testing.T) {
	s, project := readStore(t)
	lastHour := rollupHour
	from := tokenMonth(t, s, project.ID, lastHour)
	// A pass that rolls half the month: the other half is the live tail.
	middle := from + 15*24*SecondsPerHour
	passAt(t, s, time.Unix(middle+SecondsPerHour, 0))
	state, err := s.RollupState(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil <= from || state.RolledUntil >= lastHour {
		t.Fatalf("rolled_until = %d, want it inside the month so both halves answer", state.RolledUntil)
	}

	columns := func(where string, args ...any) Tokens {
		t.Helper()
		var sums tokenScan
		if err := s.db.QueryRow(`SELECT `+columnTokenSums()+` FROM traces WHERE project_id = ? AND `+where,
			append([]any{project.ID}, args...)...).Scan(sums.targets()...); err != nil {
			t.Fatal(err)
		}
		return sums.tokens()
	}

	// The rolled half: `stats_hourly`'s trace-unit cells.
	var rolled Tokens
	if err := s.StatsRollupRows(t.Context(), project.ID, from, state.RolledUntil, nil, func(row StatsRow) {
		if row.Model == "" {
			rolled.Add(row.Tokens)
		}
	}); err != nil {
		t.Fatal(err)
	}
	expectTokens(t, "stats_hourly below the watermark", rolled,
		columns(`timestamp < ?`, state.RolledUntil*1e9))
	if rolled.Reasoning == nil || rolled.CacheWrite == nil {
		t.Fatal("the rolled half carries no reasoning or cache write, so the comparison proves nothing")
	}

	// The live half: the statistics' live scan, folded the way the server
	// folds it.
	var live Tokens
	liveFrom, liveTo := state.RolledUntil*1e9, (lastHour+SecondsPerHour)*1e9
	filter := StatsFilter{From: &liveFrom, To: &liveTo, GroupBy: GroupByTotal}
	if err := s.StatsTokens(t.Context(), project.ID, filter, func(sum StatsTokenSum) { live.Add(sum.Tokens) }); err != nil {
		t.Fatal(err)
	}
	expectTokens(t, "the live scan above the watermark", live, columns(`timestamp >= ?`, liveFrom))

	for _, user := range []string{"alice", "bob", "carol"} {
		var hourly Tokens
		if err := s.UsersRollupRows(t.Context(), project.ID, user, from, state.RolledUntil, nil, func(row UserStatsRow) {
			if row.Model == "" {
				hourly.Add(row.Tokens)
			}
		}); err != nil {
			t.Fatal(err)
		}
		expectTokens(t, user+"'s users_hourly", hourly,
			columns(`user_id = ? AND timestamp < ?`, user, state.RolledUntil*1e9))

		// The per-model rows carry the same total as the trace-unit ones.
		var byModel Tokens
		if err := s.UsersRollupRows(t.Context(), project.ID, user, from, state.RolledUntil, nil, func(row UserStatsRow) {
			if row.Model != "" {
				byModel.Add(row.Tokens)
			}
		}); err != nil {
			t.Fatal(err)
		}
		expectTokens(t, user+"'s model rows", byModel, hourly)

		summary, err := s.UserSummaryRow(t.Context(), project.ID, user)
		if err != nil {
			t.Fatal(err)
		}
		expectTokens(t, user+"'s summary", summary.Tokens, hourly)

		rollup, err := s.UserRollup(t.Context(), project.ID, user, state.RolledUntil)
		if err != nil {
			t.Fatal(err)
		}
		tail, err := s.UserTail(t.Context(), project.ID, user, state.RolledUntil*1e9)
		if err != nil {
			t.Fatal(err)
		}
		whole := rollup.Tokens
		whole.Add(tail.Tokens)
		expectTokens(t, user+"'s page, both halves", whole, columns(`user_id = ?`, user))
	}
}

// min_tokens (spec 049 #6): input plus output at least N; a trace that
// carried neither has no number and never matches, not even `min_tokens=0`.
func TestMinTokensBoundsTheListing(t *testing.T) {
	s, project := readStore(t)
	seed := func(n int, usage map[string]any) {
		seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(n), Environment: "production"},
			generation(hexTrace(n), n, "m", usage))
	}
	seed(1, map[string]any{"input_tokens": 60, "output_tokens": 40})       // 100
	seed(2, map[string]any{"input_tokens": 99})                            // 99
	seed(3, map[string]any{"output_tokens": 101, "reasoning_tokens": 500}) // 101
	seed(4, map[string]any{"reasoning_tokens": 5000})                      // none
	seed(5, nil)                                                           // none

	for _, tc := range []struct {
		min  int64
		want []int
	}{
		{0, []int{1, 2, 3}},
		{99, []int{1, 2, 3}},
		{100, []int{1, 3}},
		{101, []int{3}},
		{102, nil},
	} {
		min := tc.min
		rows, err := s.Traces(t.Context(), project.ID, TraceFilter{MinTokens: &min, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range rows {
			got = append(got, row.ID)
		}
		var want []string
		for i := len(tc.want) - 1; i >= 0; i-- {
			want = append(want, hexTrace(tc.want[i]))
		}
		if !slices.Equal(got, want) {
			t.Errorf("min_tokens=%d: %v, want %v", min, got, want)
		}
		counted, err := s.CountTraces(t.Context(), project.ID, TraceFilter{MinTokens: &min}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if counted != len(tc.want) {
			t.Errorf("min_tokens=%d: count %d, want %d", min, counted, len(tc.want))
		}
	}
}

// Sessions sum per class over their traces (spec 049 #6), in the listing and
// the detail alike, and a session none of whose traces carried a class has
// none.
func TestSessionsSumTokensPerClass(t *testing.T) {
	s, project := readStore(t)
	seed := func(n int, session string, usage map[string]any) {
		seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(n), Environment: "production", SessionID: session},
			generation(hexTrace(n), n, "m", usage))
	}
	seed(1, "s1", map[string]any{"input_tokens": 10, "output_tokens": 1, "reasoning_tokens": 4})
	seed(2, "s1", map[string]any{"input_tokens": 20, "cache_creation_tokens": 8})
	seed(3, "s1", nil)
	seed(4, "s2", nil)

	want := map[string]Tokens{
		"s1": {Input: count(30), Output: count(1), Reasoning: count(4), CacheWrite: count(8)},
		"s2": {},
	}
	rows, err := s.Sessions(t.Context(), project.ID, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d sessions, want 2", len(rows))
	}
	for _, row := range rows {
		expectTokens(t, "listed "+row.ID, row.Tokens, want[row.ID])
		detail, err := s.Session(t.Context(), project.ID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		expectTokens(t, "detail "+row.ID, detail.Tokens, want[row.ID])
	}
}

// sort=tokens (spec 049 #6): descending by input plus output, a user with no
// tokens read as 0, ties broken by id; the keyset walks the whole listing in
// both directions one row at a time.
func TestUsersSortByTokens(t *testing.T) {
	s, project := readStore(t)
	seed := func(n int, user string, usage map[string]any) {
		seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(n), Environment: "production", UserID: user},
			generation(hexTrace(n), n, "m", usage))
	}
	seed(1, "heavy", map[string]any{"input_tokens": 900, "output_tokens": 100})
	seed(2, "tie-b", map[string]any{"input_tokens": 50})
	seed(3, "tie-a", map[string]any{"output_tokens": 50, "reasoning_tokens": 10000})
	seed(4, "cached", map[string]any{"cache_read_input_tokens": 1e6})
	seed(5, "none", nil)
	roll(t, s, project.ID, rollupHour)

	want := []string{"heavy", "tie-a", "tie-b", "cached", "none"}
	for _, limit := range []int{1, 2, 10} {
		var walked []string
		var cursor *UserCursor
		for range 10 {
			rows, err := s.Users(t.Context(), project.ID, UserFilter{Sort: UsersByTokens, Limit: limit, After: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				walked = append(walked, row.UserID)
			}
			last := rows[len(rows)-1]
			cursor = &UserCursor{Key: UserCursorKey(UsersByTokens, last), UserID: last.UserID}
		}
		if !slices.Equal(walked, want) {
			t.Errorf("limit %d: tokens order %v, want %v", limit, walked, want)
		}
	}

	// Backwards from the last row, one at a time, is the same order read
	// the other way.
	var back []string
	cursor := &UserCursor{Key: "0", UserID: "none"}
	for range 10 {
		rows, err := s.Users(t.Context(), project.ID, UserFilter{Sort: UsersByTokens, Limit: 1, Backward: true, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		back = append(back, rows[0].UserID)
		cursor = &UserCursor{Key: UserCursorKey(UsersByTokens, rows[0]), UserID: rows[0].UserID}
	}
	if reversed := slices.Clone(want[:4]); true {
		slices.Reverse(reversed)
		if !slices.Equal(back, reversed) {
			t.Errorf("backwards: %v, want %v", back, reversed)
		}
	}

	heavy, err := s.UserSummaryRow(t.Context(), project.ID, "heavy")
	if err != nil {
		t.Fatal(err)
	}
	expectTokens(t, "heavy's summary", heavy.Tokens, Tokens{Input: count(900), Output: count(100)})
	if key := UserCursorKey(UsersByTokens, &UserRow{}); key != "0" {
		t.Errorf("a user with no tokens keys as %q, want 0", key)
	}
}

// Erasing a user and deleting traces re-roll the statistics without their
// tokens, the new classes with the old (spec 049, Testing).
func TestErasureAndDeletionReRollTheTokens(t *testing.T) {
	s, project := readStore(t)
	seed := func(n int, user string, usage map[string]any) {
		seedTrace(t, s, project.ID, &model.Trace{ID: hexTrace(n), Environment: "production", UserID: user},
			generation(hexTrace(n), n, "m", usage))
	}
	seed(1, "alice", map[string]any{"input_tokens": 100, "reasoning_tokens": 10, "cache_write_tokens": 1})
	seed(2, "bob", map[string]any{"input_tokens": 20, "reasoning_tokens": 2, "cache_write_tokens": 3})
	seed(3, "carol", map[string]any{"input_tokens": 7, "reasoning_tokens": 5})
	roll(t, s, project.ID, rollupHour)
	advance(t, s, project.ID, rollupHour)

	cell := func() Tokens {
		return rolledRows(t, s, project.ID, rollupHour)["production||"].Tokens
	}
	expectTokens(t, "before", cell(), Tokens{Input: count(127), Reasoning: count(17), CacheWrite: count(4)})

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Submit(context.Background(),
		&UserDataErase{ProjectID: project.ID, UserID: "alice", Confirm: "alice", Limit: 100}); err != nil {
		t.Fatal(err)
	}
	expectTokens(t, "after alice's erasure", cell(), Tokens{Input: count(27), Reasoning: count(7), CacheWrite: count(3)})
	if alice, err := s.UserSummaryRow(t.Context(), project.ID, "alice"); err != nil || alice != nil {
		t.Errorf("alice's summary after erasure: %v, %v", alice, err)
	}

	if err := writer.Submit(context.Background(),
		&TraceDelete{ProjectID: project.ID, IDs: []string{hexTrace(2)}, Confirm: hexTrace(2)}); err != nil {
		t.Fatal(err)
	}
	expectTokens(t, "after bob's trace went", cell(), Tokens{Input: count(7), Reasoning: count(5)})
	bob, err := s.UserSummaryRow(t.Context(), project.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob != nil {
		t.Errorf("bob's summary outlived his only trace: %+v", bob)
	}
}

// Migration 0033 carries the sums `refreshAggregates` assigns, as the store
// builds them: a backfill that summed anything else would count what ingest
// does not.
func TestMigration0033SpellsTheTraceSums(t *testing.T) {
	body, err := migrationFS.ReadFile("migrations/0033_tokens_everywhere.sql")
	if err != nil {
		t.Fatal(err)
	}
	space := regexp.MustCompile(`\s+`)
	normal := func(s string) string {
		s = space.ReplaceAllString(s, " ")
		return strings.ReplaceAll(strings.ReplaceAll(s, "( ", "("), " )", ")")
	}
	if !strings.Contains(normal(string(body)), normal(traceTokenSums())) {
		t.Errorf("the migration does not carry traceTokenSums as the store builds it:\n%s", normal(traceTokenSums()))
	}
	if !strings.Contains(normal(string(body)), normal(userTokensKey)) {
		t.Errorf("idx_users_tokens is not built on userTokensKey %s", userTokensKey)
	}
}

// The backfill (spec 049 #4): a store the previous release left — traces with
// observations, rolled hours, a frozen hour whose raw rows are gone — reads
// the trace columns right after the upgrade, the rollups after one pass, and
// NULL in the frozen hour for ever. What it reads is what a fresh ingest of
// the same spans reads.
func TestMigration0033BackfillsTracesAndRollups(t *testing.T) {
	frozenHour := rollupHour - 30*24*SecondsPerHour
	seedAll := func(t *testing.T, s *Store, projectID string) {
		t.Helper()
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(1), Environment: "production", UserID: "u1"},
			generation(hexTrace(1), 1, "m", map[string]any{"input_tokens": 100, "output_tokens": 10, "reasoning_tokens": 4}),
			generation(hexTrace(1), 2, "m", map[string]any{"prompt_tokens": 1, "cache_creation_tokens": 9}),
			generation(hexTrace(1), 3, "", map[string]any{"input_tokens": 1000}))
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(2), Environment: "production", UserID: "u2"},
			generation(hexTrace(2), 4, "m", map[string]any{"input": 5, "output_reasoning_tokens": 2e9}))
		seedTrace(t, s, projectID, &model.Trace{ID: hexTrace(3), Environment: "staging"},
			generation(hexTrace(3), 5, "m", nil))
	}

	// The reference: the same spans on this version.
	reference, project := readStore(t)
	seedAll(t, reference, project.ID)
	passAt(t, reference, afterTheHour())

	// The store the previous release left: the same rows, rolled, clean —
	// every trace stamped before the pass, so only the migration can make
	// an hour dirty — and a frozen hour past the retention window.
	path := freshDB(t)
	func() {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		p, err := s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
		if err != nil {
			t.Fatal(err)
		}
		project = p
		seedAll(t, s, project.ID)
		stampTraces(t, s, project.ID, rollupHour*1e9)
		passAt(t, s, afterTheHour())
		for _, statement := range []string{
			fmt.Sprintf(`INSERT INTO stats_hourly (project_id, hour, environment, release, model, count, error_count,
			                                       total_cost, latency, input_tokens, output_tokens, cache_read_tokens)
			             VALUES ('%s', %d, 'production', '', '', 3, 0, NULL, '[]', 30, 3, NULL)`, project.ID, frozenHour),
			fmt.Sprintf(`INSERT INTO users_hourly (project_id, hour, user_id, environment, release, model, count,
			                                       error_count, total_cost, latency, sessions_started)
			             VALUES ('%s', %d, 'u9', 'production', '', '', 3, 0, NULL, '[]', 0)`, project.ID, frozenHour),
			`UPDATE projects SET retention_days = 7`,
		} {
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}()
	// The seeding above ran this binary's migrations, 0033 included; take
	// its columns back off so the upgrade starts from the previous shape.
	downgrade(t, path)

	start := time.Now()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	t.Logf("migration 0033 over the fixture took %v", time.Since(start).Round(time.Millisecond))

	// The trace columns are there before any pass.
	traceQuery := `SELECT id, ` + tokenColumnList("") + ` FROM traces ORDER BY id`
	if got, want := dumpRows(t, s.db, traceQuery), dumpRows(t, reference.db, traceQuery); !slices.Equal(got, want) {
		t.Errorf("trace columns after the upgrade\n got: %v\nwant: %v", got, want)
	}
	state, err := s.RollupState(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastPass != 0 || state.RolledUntil == 0 {
		t.Errorf("rollup state after 0033 = %+v, want last_pass 0 and the watermark kept", state)
	}

	// One pass, at the reference's clock, and the rollups say what they say
	// on a fresh ingest.
	passAt(t, s, afterTheHour())
	for _, query := range []string{
		`SELECT hour, environment, release, model, count, ` + tokenColumnList("") +
			` FROM stats_hourly WHERE hour = ` + fmt.Sprint(rollupHour) + ` ORDER BY 1, 2, 3, 4`,
		`SELECT hour, user_id, environment, release, model, count, ` + tokenColumnList("") +
			` FROM users_hourly WHERE hour = ` + fmt.Sprint(rollupHour) + ` ORDER BY 1, 2, 3, 4, 5`,
		`SELECT user_id, traces, ` + tokenColumnList("") + ` FROM users WHERE user_id != 'u9' ORDER BY user_id`,
	} {
		if got, want := dumpRows(t, s.db, query), dumpRows(t, reference.db, query); !slices.Equal(got, want) {
			t.Errorf("%s\n got: %v\nwant: %v", query, got, want)
		}
	}

	// The frozen hour keeps what it had and NULL for what it never had.
	frozen := dumpRows(t, s.db, fmt.Sprintf(`SELECT quote(input_tokens), quote(output_tokens),
	        quote(reasoning_tokens), quote(cache_write_tokens) FROM stats_hourly WHERE hour = %d`, frozenHour))
	if want := []string{"30|3|NULL|NULL"}; !slices.Equal(frozen, want) {
		t.Errorf("the frozen hour = %v, want %v", frozen, want)
	}
	frozenUser := dumpRows(t, s.db, fmt.Sprintf(`SELECT count, quote(input_tokens), quote(reasoning_tokens)
	        FROM users_hourly WHERE hour = %d`, frozenHour))
	if want := []string{"3|NULL|NULL"}; !slices.Equal(frozenUser, want) {
		t.Errorf("the frozen user hour = %v, want %v", frozenUser, want)
	}
}

// downgrade takes a database back to the schema before 0033: the columns and
// the index it adds come off, and the migration is no longer recorded.
func downgrade(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{`DROP INDEX idx_users_tokens`}
	for _, table := range []string{"traces", "users_hourly", "users"} {
		for _, column := range tokenColumnNames {
			statements = append(statements, `ALTER TABLE `+table+` DROP COLUMN `+column)
		}
	}
	statements = append(statements,
		`ALTER TABLE stats_hourly DROP COLUMN reasoning_tokens`,
		`ALTER TABLE stats_hourly DROP COLUMN cache_write_tokens`,
		`DELETE FROM schema_migrations WHERE filename = '0033_tokens_everywhere.sql'`)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// UsageTokens over a decoded usage — what the CLI's tree calls — says what the
// stored sums say, which reach it through `tracepad_token` and a decode of the
// stored text. Over one table of usages, one observation each, the two agree,
// or the tree would print a number no listing agrees with.
func TestUsageTokensAgreesWithTheSQL(t *testing.T) {
	s, project := readStore(t)
	usages := []map[string]any{
		{"input_tokens": 12, "output_tokens": 3},
		{"prompt_tokens": 7.9, "completion_tokens": 0},
		{"input_tokens": nil, "prompt_tokens": 5},
		{"input_tokens": "5", "prompt_tokens": 5},
		{"input_tokens": -1, "output_tokens": 1e9, "reasoning_tokens": 1e9 + 1},
		{"input": 4, "output": 2, "input_cache_read": 9, "reasoning.output_tokens": 8, "cache_creation.input_tokens": 1},
		{"cache_read": map[string]any{"input_tokens": 11}},
		{"total": 40, "total_tokens": 40},
		{"input_tokens": true, "output_tokens": []any{1}},
	}
	for i, usage := range usages {
		id := hexTrace(i + 1)
		// Through JSON, as the CLI decodes a tree: numbers are float64.
		seedTrace(t, s, project.ID, &model.Trace{ID: id, Environment: "production"},
			generation(id, i+1, "m", usage))
		observations, err := s.Observations(t.Context(), project.ID, id, SkipIO)
		if err != nil {
			t.Fatal(err)
		}
		stored := observations[0].Usage
		expectTokens(t, fmt.Sprintf("usage %v", usage), UsageTokens(stored), traceTokens(t, s, project.ID, id))
	}
}
