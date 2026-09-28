package server

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The two user endpoints and the `user_id` filter on the statistics
// (spec 023 #5, #6). The listing answers from the rollup, so every test here
// drives a pass first; the single user merges the live tail, so one of them
// deliberately does not.

type userRow struct {
	UserID     string   `json:"user_id"`
	Traces     int      `json:"traces"`
	ErrorCount int      `json:"error_count"`
	TotalCost  *float64 `json:"total_cost"`
	Sessions   int      `json:"sessions"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
}

type userPage struct {
	Users       []userRow `json:"users"`
	NextCursor  *string   `json:"next_cursor"`
	PrevCursor  *string   `json:"prev_cursor"`
	Total       *int      `json:"total"`
	TotalCapped *bool     `json:"total_capped"`
}

type userBody struct {
	userRow
	LatencyMs struct {
		P50 *int64 `json:"p50"`
		P95 *int64 `json:"p95"`
	} `json:"latency_ms"`
}

// userStatsBody is the statistics answer with the one field a `user_id`
// timeline adds (spec 023 #6).
type userStatsBody struct {
	GroupBy string `json:"group_by"`
	Unit    string `json:"unit"`
	Buckets []struct {
		Key        string   `json:"key"`
		Count      int      `json:"count"`
		ErrorCount int      `json:"error_count"`
		TotalCost  *float64 `json:"total_cost"`
		Sessions   *int     `json:"sessions"`
	} `json:"buckets"`
}

// seedUserHour puts one trace of one user in an hour, in a session of its own
// unless one is named.
func (h *harness) seedUserHour(t *testing.T, n int, user, session string, hour, offset int64,
	environment string, errored bool, cost *float64) {
	t.Helper()
	start := (hour+offset)*int64(time.Second) + 0
	trace := &model.Trace{ID: traceHex(n), Environment: environment,
		UserID: user, SessionID: session}
	observation := &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: "claude-sonnet-5",
		StartTime: start, EndTime: start + 100*ms,
	}
	if errored {
		observation.Level = model.LevelError
	}
	if cost != nil {
		observation.CostDetails = map[string]any{"total": *cost}
	}
	h.seed(t, trace, observation)
}

func price(v float64) *float64 { return &v }

// userCorpus: three users with different traffic, cost and failures, all in
// one hour so that one pass rolls the lot.
func userCorpus(t *testing.T, h *harness) {
	t.Helper()
	h.seedUserHour(t, 1, "alice", "s-a", statsHour, 10, "production", false, price(0.30))
	h.seedUserHour(t, 2, "alice", "s-a", statsHour, 20, "production", true, price(0.20))
	h.seedUserHour(t, 3, "alice", "s-b", statsHour, 30, "staging", false, nil)
	h.seedUserHour(t, 4, "bob", "s-c", statsHour, 40, "production", false, price(0.10))
	h.seedUserHour(t, 5, "bob", "s-d", statsHour, 50, "production", true, price(0.05))
	h.seedUserHour(t, 6, "carol", "s-e", statsHour, 60, "production", false, nil)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
}

func (h *harness) users(t *testing.T, path string) userPage {
	t.Helper()
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	return decodeJSON[userPage](t, rec)
}

func names(page userPage) []string {
	out := make([]string, 0, len(page.Users))
	for _, row := range page.Users {
		out = append(out, row.UserID)
	}
	return out
}

func TestUserListingSorts(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	for _, tc := range []struct {
		sort string
		want []string
	}{
		// alice ran three, bob two, carol one.
		{"traces", []string{"alice", "bob", "carol"}},
		// alice cost 0.50, bob 0.15, carol nothing at all — and a user
		// with no costed trace sorts last (edge cases).
		{"cost", []string{"alice", "bob", "carol"}},
		// alice and bob failed once each, carol never; the tie is broken
		// by the id.
		{"errors", []string{"alice", "bob", "carol"}},
		// carol's trace is the newest, but they all fall in one hour and
		// the rollup's grain is the hour — so the tie-break decides, and
		// it is the id.
		{"last_seen", []string{"alice", "bob", "carol"}},
	} {
		t.Run(tc.sort, func(t *testing.T) {
			page := h.users(t, "/api/v1/users?sort="+tc.sort)
			got := names(page)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("order = %v, want %v", got, tc.want)
			}
		})
	}

	// The default is `last_seen`, spelled or not.
	if a, b := names(h.users(t, "/api/v1/users")), names(h.users(t, "/api/v1/users?sort=last_seen")); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("the default order %v is not last_seen's %v", a, b)
	}
}

func TestUserListingRowsAreTheRollup(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	page := h.users(t, "/api/v1/users?sort=traces")
	alice := page.Users[0]
	if alice.Traces != 3 || alice.ErrorCount != 1 || alice.Sessions != 2 {
		t.Errorf("alice = %d traces, %d errors, %d sessions; want 3, 1, 2",
			alice.Traces, alice.ErrorCount, alice.Sessions)
	}
	if alice.TotalCost == nil || *alice.TotalCost < 0.4999 || *alice.TotalCost > 0.5001 {
		t.Errorf("alice cost = %v, want 0.5", alice.TotalCost)
	}
	if alice.FirstSeen == "" || alice.LastSeen == "" {
		t.Errorf("alice has no window: %q .. %q", alice.FirstSeen, alice.LastSeen)
	}
	// A user with no costed trace has no cost at all, which is not zero.
	carol := page.Users[2]
	if carol.TotalCost != nil {
		t.Errorf("carol cost = %v, want absent", carol.TotalCost)
	}
}

func TestUserListingPrefixAndCount(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	page := h.users(t, "/api/v1/users?prefix=a")
	if fmt.Sprint(names(page)) != "[alice]" {
		t.Errorf("prefix a = %v, want [alice]", names(page))
	}
	if page := h.users(t, "/api/v1/users?prefix=A"); len(page.Users) != 0 {
		t.Errorf("prefix A matched %v; the prefix is case-sensitive", names(page))
	}

	counted := h.users(t, "/api/v1/users?count=1")
	if counted.Total == nil || *counted.Total != 3 {
		t.Errorf("total = %v, want 3", counted.Total)
	}
	if counted.TotalCapped == nil || *counted.TotalCapped {
		t.Errorf("total_capped = %v, want false", counted.TotalCapped)
	}
}

func TestUserListingPagesBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	first := h.users(t, "/api/v1/users?sort=traces&limit=1")
	if fmt.Sprint(names(first)) != "[alice]" || first.NextCursor == nil {
		t.Fatalf("first page = %v, next = %v", names(first), first.NextCursor)
	}
	if first.PrevCursor != nil {
		t.Errorf("the first page has a previous one: %v", first.PrevCursor)
	}
	second := h.users(t, "/api/v1/users?sort=traces&limit=1&cursor="+url.QueryEscape(*first.NextCursor))
	if fmt.Sprint(names(second)) != "[bob]" {
		t.Fatalf("second page = %v, want [bob]", names(second))
	}
	third := h.users(t, "/api/v1/users?sort=traces&limit=1&cursor="+url.QueryEscape(*second.NextCursor))
	if fmt.Sprint(names(third)) != "[carol]" || third.NextCursor != nil {
		t.Fatalf("third page = %v, next = %v", names(third), third.NextCursor)
	}

	// And back up from the last page.
	back := h.users(t, "/api/v1/users?sort=traces&limit=1&direction=prev&cursor="+
		url.QueryEscape(*third.PrevCursor))
	if fmt.Sprint(names(back)) != "[bob]" {
		t.Errorf("the page above carol is %v, want [bob]", names(back))
	}
	// With no cursor, `prev` is the far end.
	end := h.users(t, "/api/v1/users?sort=traces&limit=1&direction=prev")
	if fmt.Sprint(names(end)) != "[carol]" {
		t.Errorf("the far end is %v, want [carol]", names(end))
	}
}

func TestUserListingRefusesWhatItDoesNotKnow(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	for _, path := range []string{
		"/api/v1/users?sort=oldest",
		"/api/v1/users?sort=",
		"/api/v1/users?prefix=",
		"/api/v1/users?name=alice",
		"/api/v1/users?cursor=not-a-cursor",
		"/api/v1/users/alice?window=7d",
	} {
		rec := h.get(t, path)
		expectStatus(t, rec, 400)
	}
}

// TestGetUserMergesTheLiveTail is Decision 4's promise: the listing trails the
// raw rows and the page does not.
func TestGetUserMergesTheLiveTail(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	rolled := decodeJSON[userBody](t, h.get(t, "/api/v1/users/alice"))
	if rolled.Traces != 3 {
		t.Fatalf("alice = %d traces before the tail, want 3", rolled.Traces)
	}
	if rolled.LatencyMs.P50 == nil {
		t.Errorf("no p50 over the rolled histogram")
	}

	// One more trace, well past the watermark and in a session of its own,
	// with no pass in between.
	h.seedUserHour(t, 7, "alice", "s-late", statsHour+3*3600, 10, "production", false, price(0.01))

	after := decodeJSON[userBody](t, h.get(t, "/api/v1/users/alice"))
	if after.Traces != 4 {
		t.Errorf("alice = %d traces with the tail, want 4", after.Traces)
	}
	if after.Sessions != 3 {
		t.Errorf("alice = %d sessions with the tail, want 3", after.Sessions)
	}
	if after.LastSeen <= rolled.LastSeen {
		t.Errorf("last_seen did not move with the tail: %q then %q", rolled.LastSeen, after.LastSeen)
	}
	if after.TotalCost == nil || *after.TotalCost < 0.5099 || *after.TotalCost > 0.5101 {
		t.Errorf("cost = %v with the tail, want 0.51", after.TotalCost)
	}

	// A user the listing has never heard of, seen only in the tail: exact
	// here, absent there (edge cases).
	h.seedUserHour(t, 8, "dave", "s-new", statsHour+3*3600, 20, "production", false, nil)
	dave := decodeJSON[userBody](t, h.get(t, "/api/v1/users/dave"))
	if dave.Traces != 1 || dave.Sessions != 1 {
		t.Errorf("dave = %d traces, %d sessions; want 1 and 1", dave.Traces, dave.Sessions)
	}
	for _, listed := range names(h.users(t, "/api/v1/users")) {
		if listed == "dave" {
			t.Error("dave is in the listing before a pass has rolled him")
		}
	}
}

func TestGetUnseenUserIs404(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)
	expectStatus(t, h.get(t, "/api/v1/users/nobody"), 404)
}

// TestStatsByUserEqualsThatUsersCorpus: the filter is a filter — the buckets
// are what the same range says about a corpus of that user alone, on both
// halves of the seam (spec 023 #6).
func TestStatsByUserEqualsThatUsersCorpus(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)
	// A second hour, deliberately left unrolled so the range straddles the
	// watermark.
	h.seedUserHour(t, 7, "alice", "s-late", statsHour+3*3600, 10, "production", true, price(0.01))

	filtered := h.userStats(t, "/api/v1/stats?group_by=hour&user_id=alice")
	var count, errors int
	for _, bucket := range filtered.Buckets {
		count += bucket.Count
		errors += bucket.ErrorCount
	}
	if count != 4 || errors != 2 {
		t.Errorf("alice's timeline = %d traces, %d errors; want 4 and 2", count, errors)
	}
	if len(filtered.Buckets) != 2 {
		t.Fatalf("buckets = %d, want the rolled hour and the live one", len(filtered.Buckets))
	}
	// Two sessions started in the rolled hour, one in the live one — which
	// is the seam counting each session once, on the side it began.
	if filtered.Buckets[0].Sessions == nil || *filtered.Buckets[0].Sessions != 2 {
		t.Errorf("rolled hour started %v sessions, want 2", filtered.Buckets[0].Sessions)
	}
	if filtered.Buckets[1].Sessions == nil || *filtered.Buckets[1].Sessions != 1 {
		t.Errorf("live hour started %v sessions, want 1", filtered.Buckets[1].Sessions)
	}

	// The unfiltered answer is the whole corpus, and carries no `sessions`
	// at all: `stats_hourly` has no such number.
	whole := h.userStats(t, "/api/v1/stats?group_by=hour")
	var all int
	for _, bucket := range whole.Buckets {
		all += bucket.Count
		if bucket.Sessions != nil {
			t.Errorf("an unfiltered bucket carries sessions = %v", bucket.Sessions)
		}
	}
	if all != 7 {
		t.Errorf("the whole corpus is %d traces, want 7", all)
	}

	// The other groupings are the same buckets restricted, with no
	// `sessions` — it has no meaning outside a timeline.
	byEnvironment := h.userStats(t, "/api/v1/stats?group_by=environment&user_id=alice")
	if len(byEnvironment.Buckets) != 2 {
		t.Errorf("alice ran in %d environments, want production and staging", len(byEnvironment.Buckets))
	}
	for _, bucket := range byEnvironment.Buckets {
		if bucket.Sessions != nil {
			t.Errorf("a categorical bucket carries sessions = %v", bucket.Sessions)
		}
	}
	byModel := h.userStats(t, "/api/v1/stats?group_by=model&user_id=alice")
	if byModel.Unit != "observation" || len(byModel.Buckets) != 1 {
		t.Errorf("alice's models = %+v (unit %q)", byModel.Buckets, byModel.Unit)
	}
}

// TestStatsByUserFiltersSessionsByEnvironment: `environment` is a filter, and a
// filter that only one half of the seam applies makes the series step at the
// watermark. The rolled half files `sessions_started` on the environment cell
// of the trace that began the session, and the live half has to agree (found
// in review of PR #42).
func TestStatsByUserFiltersSessionsByEnvironment(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// Two sessions in the rolled hour, one production and one staging; two
	// more in the live hour, split the same way.
	h.seedUserHour(t, 1, "alice", "s-prod-old", statsHour, 10, "production", false, nil)
	h.seedUserHour(t, 2, "alice", "s-stage-old", statsHour, 20, "staging", false, nil)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	h.seedUserHour(t, 3, "alice", "s-prod-new", statsHour+3*3600, 10, "production", false, nil)
	h.seedUserHour(t, 4, "alice", "s-stage-new", statsHour+3*3600, 20, "staging", false, nil)

	filtered := h.userStats(t,
		"/api/v1/stats?group_by=hour&user_id=alice&environment=production")
	if len(filtered.Buckets) != 2 {
		t.Fatalf("buckets = %d, want the rolled hour and the live one", len(filtered.Buckets))
	}
	for i, bucket := range filtered.Buckets {
		half := "rolled"
		if i == 1 {
			half = "live"
		}
		if bucket.Sessions == nil || *bucket.Sessions != 1 {
			t.Errorf("the %s half counts %d production session starts, want 1",
				half, deref(bucket.Sessions))
		}
		// And the count beside it is the one trace of that environment: a
		// bucket that carries a session but no trace is the fabricated row
		// the endpoint promises never to answer with.
		if bucket.Count != 1 {
			t.Errorf("the %s half counts %d traces, want 1", half, bucket.Count)
		}
	}

	// An environment nothing started in produces no buckets at all, rather
	// than buckets holding a session and no traces.
	empty := h.userStats(t,
		"/api/v1/stats?group_by=hour&user_id=alice&environment=development")
	if len(empty.Buckets) != 0 {
		t.Errorf("buckets = %+v, want none", empty.Buckets)
	}
}

// deref reads an optional count for a failure message; a nil one is the
// absence the assertion above has already reported.
func deref(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func (h *harness) userStats(t *testing.T, path string) userStatsBody {
	t.Helper()
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	return decodeJSON[userStatsBody](t, rec)
}

// TestErasureRemovesTheUserFromTheRollup (spec 023 #10): the endpoint's
// contract is that the user's data is gone when the 200 arrives, and a
// per-user count is data about the user.
func TestErasureRemovesTheUserFromTheRollup(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	rec := h.call(t, "DELETE",
		"/api/v1/projects/"+h.project.ID+"/users/alice/data?confirm=alice&wait=30", nil)
	expectStatus(t, rec, 200)

	expectStatus(t, h.get(t, "/api/v1/users/alice"), 404)
	for _, listed := range names(h.users(t, "/api/v1/users")) {
		if listed == "alice" {
			t.Error("alice is still listed after her data was erased")
		}
	}
	// And nobody else moved.
	bob := decodeJSON[userBody](t, h.get(t, "/api/v1/users/bob"))
	if bob.Traces != 2 {
		t.Errorf("bob = %d traces after alice's erasure, want 2", bob.Traces)
	}
}

// TestSystemCountsTheUserTables (spec 023, Data contract): an operator watching
// disk sees the table that multiplies by the user count.
func TestSystemCountsTheUserTables(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Database struct {
			Rows map[string]int64 `json:"rows"`
		} `json:"database"`
	}](t, rec)
	if body.Database.Rows["users"] != 3 {
		t.Errorf("users = %d, want 3", body.Database.Rows["users"])
	}
	if body.Database.Rows["users_hourly"] == 0 {
		t.Error("users_hourly is not counted")
	}
}
