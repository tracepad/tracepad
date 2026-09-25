package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// `GET /api/v1/stats/scores` (spec 025 #6, #7): the same read seam the
// statistics use, answering a different shape.

// scoreTrendBucket is one bucket of one series, with every shape a data type
// can add to it: a numeric name fills the mean and the extremes, a boolean one
// the rate, a categorical one the distribution.
type scoreTrendBucket struct {
	Key        string           `json:"key"`
	Count      int64            `json:"count"`
	Mean       *float64         `json:"mean"`
	Min        *float64         `json:"min"`
	Max        *float64         `json:"max"`
	Rate       *float64         `json:"rate"`
	Categories map[string]int64 `json:"categories"`
}

type scoreTrendSeries struct {
	Name     string             `json:"name"`
	DataType string             `json:"data_type"`
	Buckets  []scoreTrendBucket `json:"buckets"`
}

// scoreTrendBody is the answer, decoded.
type scoreTrendBody struct {
	GroupBy string             `json:"group_by"`
	Targets string             `json:"targets"`
	Omitted int                `json:"omitted"`
	Series  []scoreTrendSeries `json:"series"`
}

func (b scoreTrendBody) series(t *testing.T, name string) scoreTrendSeries {
	t.Helper()
	for _, one := range b.Series {
		if one.Name == name {
			return one
		}
	}
	t.Fatalf("no series %q in %+v", name, b.Series)
	panic("unreachable")
}

func (h *harness) scoreTrends(t *testing.T, path string) scoreTrendBody {
	t.Helper()
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	return decodeJSON[scoreTrendBody](t, rec)
}

// seedScoredHour puts one trace with one generation in an hour and grades it
// three ways — numeric, boolean and categorical — plus one score on the
// observation, which is the only kind a model grouping can count.
func (h *harness) seedScoredHour(t *testing.T, hour int64, n int, environment, release, model_ string,
	value float64, category string) {
	t.Helper()
	start := hour*int64(time.Second) + int64(n)*int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Environment: environment, Release: release}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: model_,
		StartTime: start, EndTime: start + 100*ms,
	})
	body := []map[string]any{
		{"id": scoreHex(n*10 + 1), "trace_id": trace.ID, "name": "hallucination", "value": value},
		{"id": scoreHex(n*10 + 2), "trace_id": trace.ID, "name": "thumbs",
			"data_type": "boolean", "value": float64(n % 2)},
		{"id": scoreHex(n*10 + 3), "trace_id": trace.ID, "name": "verdict",
			"data_type": "categorical", "string_value": category},
		{"id": scoreHex(n*10 + 4), "trace_id": trace.ID, "observation_id": spanHex(n),
			"name": "faithfulness", "value": value},
	}
	rec := h.send(t, "POST", "/api/v1/scores", body)
	expectStatus(t, rec, 201)
}

func scoreHex(n int) string { return fmt.Sprintf("%032x", 5000+n) }

// seedGradedTrace puts one trace with one generation in an hour, for the tests
// that then grade it themselves.
func (h *harness) seedGradedTrace(t *testing.T, hour int64, n int) string {
	t.Helper()
	start := hour*int64(time.Second) + int64(n)*int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Environment: "production", Release: "2.5.0"}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: "claude-sonnet-5",
		StartTime: start, EndTime: start + 100*ms,
	})
	return trace.ID
}

// postScores files a batch in one request, which is how a client that grades in
// bulk writes.
func (h *harness) postScores(t *testing.T, body []map[string]any) {
	t.Helper()
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", body), 201)
}

// derefFloat makes a failure message readable: `%v` of a pointer is an address,
// which says nothing about the number that was wrong.
func derefFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// The seam, checked the way the spec asks: a range entirely behind the
// watermark answers with the raw rows gone.
func TestScoreTrendsBehindTheWatermarkOutliveTheRawRows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// The traces arrive in the hour they ran, so the retention pass below
	// takes them whatever the date the suite runs on.
	h.arrival = statsHour * int64(time.Second)
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	before := h.scoreTrends(t, "/api/v1/stats/scores?group_by=hour")
	hallucination := before.series(t, "hallucination")
	if len(hallucination.Buckets) != 1 || hallucination.Buckets[0].Count != 2 {
		t.Fatalf("hallucination = %+v, want one hour of two scores", hallucination.Buckets)
	}

	// The raw rows go the way retention takes them; the rollup stands.
	if err := h.setRetention(h.project.ID, 1); err != nil {
		t.Fatal(err)
	}
	sweeper := h.store.NewSweeper(h.writer, store.SweepOptions{
		Now: func() time.Time { return time.Unix(statsHour, 0).Add(30 * 24 * time.Hour) },
	})
	if err := sweeper.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remaining := h.countTraces(t); remaining != 0 {
		t.Fatalf("%d traces survived the sweep; the test proves nothing", remaining)
	}

	after := h.scoreTrends(t, "/api/v1/stats/scores?group_by=hour")
	got := after.series(t, "hallucination")
	if len(got.Buckets) != 1 || got.Buckets[0].Count != 2 ||
		got.Buckets[0].Key != hallucination.Buckets[0].Key {
		t.Errorf("buckets = %+v after the raw rows went, want %+v",
			got.Buckets, hallucination.Buckets)
	}
	if mean := got.Buckets[0].Mean; mean == nil || *mean < 0.39 || *mean > 0.41 {
		t.Errorf("mean = %v, want the stored sum over the stored count", mean)
	}
}

// A range straddling the watermark is the two halves added, each score counted
// once. Double-counting at the seam would be a curve that jumps.
//
// The first hour holds two scores of different values on purpose: a day whose
// hours hold one score each cannot tell a mean from a mean of means, and the
// mean of means is the wrong number this row format exists to make impossible
// (Decision 2).
func TestScoreTrendsStraddlingTheWatermarkCountEachScoreOnce(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 4, "production", "2.5.0", "claude-sonnet-5", 0.0, "pass")
	h.seedScoredHour(t, statsHour+3600, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	h.seedScoredHour(t, statsHour+2*3600, 3, "production", "2.5.0", "claude-sonnet-5", 1.0, "pass")

	// A pass whose clock leaves the third hour open, so it is the live tail.
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600+600, 0))

	body := h.scoreTrends(t, "/api/v1/stats/scores?group_by=hour")
	got := body.series(t, "hallucination")
	if len(got.Buckets) != 3 {
		t.Fatalf("buckets = %+v, want the three hours", got.Buckets)
	}
	counts := map[string]int64{}
	var total int64
	for _, bucket := range got.Buckets {
		counts[bucket.Key] = bucket.Count
		total += bucket.Count
	}
	if total != 4 {
		t.Errorf("the range counts %d scores, want the four seeded", total)
	}
	if counts[got.Buckets[0].Key] != 2 {
		t.Errorf("the first hour counts %d, want its two scores", counts[got.Buckets[0].Key])
	}

	// A day is the merge of its hours: the same four scores, one bucket, and a
	// mean that is Σsum / Σcount. A mean of means would answer 0.567 here.
	daily := h.scoreTrends(t, "/api/v1/stats/scores?group_by=day").series(t, "hallucination")
	if len(daily.Buckets) != 1 || daily.Buckets[0].Count != 4 {
		t.Fatalf("day buckets = %+v, want one of four", daily.Buckets)
	}
	mean := daily.Buckets[0].Mean
	if mean == nil || *mean < 0.449 || *mean > 0.451 {
		t.Errorf("day mean = %v, want (0.2 + 0 + 0.6 + 1.0) / 4", derefFloat(mean))
	}
	if min, max := daily.Buckets[0].Min, daily.Buckets[0].Max; min == nil || *min != 0 || max == nil || *max != 1 {
		t.Errorf("day extremes = %v..%v, want 0..1", derefFloat(min), derefFloat(max))
	}
}

// The three summaries, one per data type (Decision 2, Decision 6).
func TestScoreTrendsSummarizeByDataType(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	h.seedScoredHour(t, statsHour, 3, "production", "2.5.0", "claude-sonnet-5", 1.0, "pass")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	body := h.scoreTrends(t, "/api/v1/stats/scores?group_by=day")
	if body.Targets != "any" {
		t.Errorf("targets = %q, want any: every score counts once outside a model grouping", body.Targets)
	}

	// Boolean: `n % 2` over traces 1, 2 and 3, so two of the three are true.
	thumbs := body.series(t, "thumbs")
	if thumbs.DataType != "boolean" {
		t.Fatalf("thumbs is %q, want boolean", thumbs.DataType)
	}
	if rate := thumbs.Buckets[0].Rate; rate == nil || *rate < 0.666 || *rate > 0.667 {
		t.Errorf("rate = %v, want two true in three", derefFloat(rate))
	}
	if thumbs.Buckets[0].Mean != nil {
		t.Error("a boolean bucket carries a mean; the rate is what it means")
	}

	// Categorical: the distribution, and the shares sum to the count.
	verdict := body.series(t, "verdict")
	categories := verdict.Buckets[0].Categories
	if categories["pass"] != 2 || categories["fail"] != 1 {
		t.Errorf("categories = %v, want two passes and a fail", categories)
	}
	var counted int64
	for _, n := range categories {
		counted += n
	}
	if counted != verdict.Buckets[0].Count {
		t.Errorf("the categories sum to %d, want the bucket's %d", counted, verdict.Buckets[0].Count)
	}
}

// Grouping by model counts only the scores that name an observation, and says
// so (Decision 6): a trace-level score has no model to sit under.
func TestScoreTrendsByModelCountOnlyObservationScores(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 2, "production", "2.5.0", "gpt-4o-mini", 0.6, "fail")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	body := h.scoreTrends(t, "/api/v1/stats/scores?group_by=model")
	if body.Targets != "observation" {
		t.Errorf("targets = %q, want observation", body.Targets)
	}
	if len(body.Series) != 1 || body.Series[0].Name != "faithfulness" {
		t.Fatalf("series = %+v, want only the observation-level name", body.Series)
	}
	keys := map[string]int64{}
	for _, bucket := range body.Series[0].Buckets {
		keys[bucket.Key] = bucket.Count
	}
	if keys["claude-sonnet-5"] != 1 || keys["gpt-4o-mini"] != 1 || len(keys) != 2 {
		t.Errorf("model buckets = %v, want one per model and no empty key", keys)
	}
}

// `name` narrows to one series; the environment box and the range narrow the
// numbers, on both sides of the seam.
func TestScoreTrendsNarrowByNameEnvironmentAndRange(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 2, "staging", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	h.seedScoredHour(t, statsHour+3600, 3, "production", "2.6.0", "claude-sonnet-5", 1.0, "pass")
	h.rollTheCorpus(t, time.Unix(statsHour+4*3600, 0))

	named := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	if len(named.Series) != 1 || named.Series[0].Name != "hallucination" {
		t.Fatalf("series = %+v, want the one asked for", named.Series)
	}
	if named.Series[0].Buckets[0].Count != 3 {
		t.Errorf("count = %d, want the three scores of that name", named.Series[0].Buckets[0].Count)
	}

	filtered := h.scoreTrends(t,
		"/api/v1/stats/scores?name=hallucination&environment=production&group_by=day")
	if filtered.Series[0].Buckets[0].Count != 2 {
		t.Errorf("count = %d in production, want the two production scores",
			filtered.Series[0].Buckets[0].Count)
	}

	from := time.Unix(statsHour+3600, 0).UTC().Format(time.RFC3339)
	windowed := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day&from="+from)
	if windowed.Series[0].Buckets[0].Count != 1 {
		t.Errorf("count = %d from the second hour, want the one score in it",
			windowed.Series[0].Buckets[0].Count)
	}

	// The release breakdown is the question the screen's third table asks.
	releases := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=release")
	keys := map[string]int64{}
	for _, bucket := range releases.Series[0].Buckets {
		keys[bucket.Key] = bucket.Count
	}
	if keys["2.5.0"] != 2 || keys["2.6.0"] != 1 {
		t.Errorf("release buckets = %v, want two at 2.5.0 and one at 2.6.0", keys)
	}
}

// A score that names no trace, and a text score, are not in any series — on
// either side of the seam (Decision 1).
func TestScoreTrendsLeaveOutSessionAndTextScores(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"id": scoreHex(900), "session_id": "sess-1", "name": "csat", "value": 1.0},
		{"id": scoreHex(901), "trace_id": traceHex(1), "name": "rationale",
			"data_type": "text", "string_value": "it repeated itself"},
	})
	expectStatus(t, rec, 201)

	// Live first: nothing has been rolled yet, so this is the raw path.
	live := h.scoreTrends(t, "/api/v1/stats/scores?group_by=day")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	rolled := h.scoreTrends(t, "/api/v1/stats/scores?group_by=day")

	for _, body := range []scoreTrendBody{live, rolled} {
		for _, series := range body.Series {
			if series.Name == "csat" || series.Name == "rationale" {
				t.Errorf("%q is a series, and it has no trace hour to sit in", series.Name)
			}
		}
	}
	if len(live.Series) != len(rolled.Series) {
		t.Errorf("live has %d series and the rollup %d; the seam is visible",
			len(live.Series), len(rolled.Series))
	}
}

// One name graded two ways is two series, told apart by the type (edge cases).
func TestOneNameGradedTwoWaysIsTwoSeries(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"id": scoreHex(910), "trace_id": traceHex(1), "name": "hallucination",
			"data_type": "categorical", "string_value": "some"},
	})
	expectStatus(t, rec, 201)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	body := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	types := map[string]bool{}
	for _, series := range body.Series {
		types[series.DataType] = true
	}
	if len(body.Series) != 2 || !types["numeric"] || !types["categorical"] {
		t.Errorf("series = %+v, want one per data type", body.Series)
	}
}

// A window the corpus does not reach answers with no series rather than with
// fabricated zeroes; the screen's empty state is what renders it.
func TestScoreTrendsAnswerAnEmptyWindowWithNoSeries(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	from := time.Unix(statsHour+30*24*3600, 0).UTC().Format(time.RFC3339)
	body := h.scoreTrends(t, "/api/v1/stats/scores?from="+from)
	if len(body.Series) != 0 {
		t.Errorf("series = %+v over an empty window, want none", body.Series)
	}
}

// The first of the two ceilings of Decision 24: a score name needs no config,
// so the number of them is unbounded client input. What is kept is what was
// busiest; what it is *written* in is name order, so a reader scanning an
// alphabetical list does not have to re-find their place because a name grew.
func TestScoreTrendsKeepTheBusiestNames(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := h.seedGradedTrace(t, statsHour, 1)
	// The i-th name is filed i+1 times, so the busiest names are the last
	// ones alphabetically and "kept by size" cannot pass as "kept by name".
	var body []map[string]any
	for i := range 5 {
		for range i + 1 {
			body = append(body, map[string]any{
				"id": scoreHex(1000 + len(body)), "trace_id": trace,
				"name": fmt.Sprintf("name-%02d", i), "value": 1.0})
		}
	}
	h.postScores(t, body)

	whole := h.scoreTrends(t, "/api/v1/stats/scores")
	if len(whole.Series) != 5 || whole.Omitted != 0 {
		t.Fatalf("the whole answer is %d series, omitted %d; want 5 and 0",
			len(whole.Series), whole.Omitted)
	}

	capped := h.scoreTrends(t, "/api/v1/stats/scores?limit=3")
	if capped.Omitted != 2 {
		t.Errorf("omitted = %d, want the 2 names the limit left out", capped.Omitted)
	}
	got := make([]string, 0, len(capped.Series))
	for _, one := range capped.Series {
		got = append(got, one.Name)
	}
	want := []string{"name-02", "name-03", "name-04"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("series = %v, want the three busiest in name order %v", got, want)
	}
}

// The second ceiling: a categorical `string_value` that is really free text
// would otherwise grow a bucket a value at a time. The rarest fold into one
// `other` key rather than disappearing, so the distribution still adds up to
// the `count` a reader divides by.
func TestScoreTrendsFoldTheRarestCategories(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := h.seedGradedTrace(t, statsHour, 1)
	var body []map[string]any
	for i := range 25 {
		for range i + 1 {
			body = append(body, map[string]any{
				"id": scoreHex(1000 + len(body)), "trace_id": trace,
				"name": "verdict", "data_type": "categorical",
				"string_value": fmt.Sprintf("v%02d", i)})
		}
	}
	h.postScores(t, body)

	buckets := h.scoreTrends(t, "/api/v1/stats/scores").series(t, "verdict").Buckets
	if len(buckets) != 1 {
		t.Fatalf("buckets = %+v, want the one day the corpus is in", buckets)
	}
	bucket := buckets[0]
	if len(bucket.Categories) != 21 {
		t.Errorf("categories = %d, want the twenty busiest and one `other`", len(bucket.Categories))
	}
	// v00..v04 were filed 1, 2, 3, 4 and 5 times.
	if bucket.Categories["other"] != 15 {
		t.Errorf("other = %d, want the 15 scores the five rarest values carried",
			bucket.Categories["other"])
	}
	var total int64
	for _, count := range bucket.Categories {
		total += count
	}
	if total != bucket.Count {
		t.Errorf("the categories sum to %d over a bucket of %d: a folded value was lost",
			total, bucket.Count)
	}
}

// Spec 003 #21 and #23, on the new endpoint: an unknown parameter and a
// parameter given without a value are both refusals.
func TestScoreTrendsRefuseUnknownAndEmptyParameters(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for _, path := range []string{
		"/api/v1/stats/scores?user_id=someone",
		"/api/v1/stats/scores?name=",
		"/api/v1/stats/scores?group_by=",
		"/api/v1/stats/scores?group_by=user",
		"/api/v1/stats/scores?from=yesterday",
		"/api/v1/stats/scores?limit=",
		"/api/v1/stats/scores?limit=0",
		"/api/v1/stats/scores?limit=501",
		"/api/v1/stats/scores?limit=all",
	} {
		t.Run(path, func(t *testing.T) {
			expectStatus(t, h.get(t, path), 400)
		})
	}
}

// Deleting a score corrects the rollup before it answers (Decision 4): the row
// is gone, so `created_at > last_pass` can never find it, and nothing else
// would ever dirty that hour.
func TestDeletingAScoreCorrectsTheRollupBeforeItAnswers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	before := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	if before.Series[0].Buckets[0].Count != 2 {
		t.Fatalf("count = %d before the retraction, want 2", before.Series[0].Buckets[0].Count)
	}

	rec := h.call(t, "DELETE", "/api/v1/scores/"+scoreHex(11), nil)
	expectStatus(t, rec, 200)

	after := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	bucket := after.Series[0].Buckets[0]
	if bucket.Count != 1 {
		t.Fatalf("count = %d immediately after the retraction, want 1", bucket.Count)
	}
	if mean := bucket.Mean; mean == nil || *mean < 0.599 || *mean > 0.601 {
		t.Errorf("mean = %v, want the surviving 0.6 alone", mean)
	}
}

// The other half of Decision 4: a score in the live tail needs no re-roll, and
// asking for one would be a write on a read path.
func TestDeletingAScoreInTheTailNeedsNoReRoll(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.seedScoredHour(t, statsHour+2*3600, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
	// The third hour stays open, so the second trace's scores are the tail's.
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600+600, 0))

	rec := h.call(t, "DELETE", "/api/v1/scores/"+scoreHex(21), nil)
	expectStatus(t, rec, 200)

	body := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	if body.Series[0].Buckets[0].Count != 1 {
		t.Errorf("count = %d, want the one surviving score",
			body.Series[0].Buckets[0].Count)
	}
}

// `GET /api/v1/system` counts the third table, so an operator watching disk
// has a number rather than a guess (spec 013 #8).
func TestSystemCountsTheScoreRollup(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	counts := decodeJSON[struct {
		Database struct {
			Rows map[string]int64 `json:"rows"`
		} `json:"database"`
	}](t, rec)
	if counts.Database.Rows["scores_hourly"] == 0 {
		t.Errorf("scores_hourly is not counted: %v", counts.Database.Rows)
	}
}

// The router table, the endpoint map and the OpenAPI document are one list
// (spec 004 Decision 25); this is the map's half for the new route.
func TestTheEndpointMapNamesTheScoreTrends(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.get(t, "/api/v1")
	expectStatus(t, rec, 200)
	var index struct {
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range index.Endpoints {
		if endpoint.Method == "GET" && endpoint.Path == "/api/v1/stats/scores" {
			return
		}
	}
	t.Errorf("the endpoint map does not name /api/v1/stats/scores: %+v", index.Endpoints)
}

// A re-POST that moves a score to another trace, or off one, leaves a row in
// the hour it came from that the dirty query can never find: the write is
// found by `created_at`, and it dirties the hour of the trace the score names
// *now* (spec 025 #20). Both shapes, and both would double-count for ever.
func TestMovingAScoreCorrectsTheHourItLeaves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		moved map[string]any
	}{
		{
			name: "re-pointed at a trace in another hour",
			moved: map[string]any{"id": scoreHex(11), "trace_id": traceHex(2),
				"name": "hallucination", "value": 0.9},
		},
		{
			// The API takes a score that names only a session (spec 003 #4),
			// and such a score is on no timeline at all.
			name: "re-pointed off its trace onto a session",
			moved: map[string]any{"id": scoreHex(11), "session_id": "sess-moved",
				"name": "hallucination", "value": 0.9},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})
			h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
			h.seedScoredHour(t, statsHour+3600, 2, "production", "2.5.0", "claude-sonnet-5", 0.6, "fail")
			h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

			before := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=hour")
			if len(before.series(t, "hallucination").Buckets) != 2 {
				t.Fatalf("buckets = %+v before the move, want the two hours",
					before.series(t, "hallucination").Buckets)
			}

			rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{tc.moved})
			expectStatus(t, rec, 201)

			after := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=hour")
			hours := map[string]int64{}
			var total int64
			for _, bucket := range after.series(t, "hallucination").Buckets {
				hours[bucket.Key] = bucket.Count
				total += bucket.Count
			}
			// The score is in one place or in none — never in both.
			if total != 1 {
				t.Errorf("the range counts %d scores after the move, want 1: %v", total, hours)
			}
			if hours[before.series(t, "hallucination").Buckets[0].Key] != 0 {
				t.Errorf("the hour it left still counts it: %v", hours)
			}
		})
	}
}

// A correction that leaves the target alone must not pay for the rule above:
// the hour cannot move, because what decides it is the trace's timestamp.
func TestCorrectingAScoreInPlaceLeavesItsHourAlone(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedScoredHour(t, statsHour, 1, "production", "2.5.0", "claude-sonnet-5", 0.2, "pass")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	rec := h.send(t, "POST", "/api/v1/scores", []map[string]any{
		{"id": scoreHex(11), "trace_id": traceHex(1), "name": "hallucination", "value": 0.9},
	})
	expectStatus(t, rec, 201)

	// The pass has not run again, so this is the live half correcting nothing
	// and the rolled half still holding the old value — which is the lag the
	// docs publish, not a hole.
	body := h.scoreTrends(t, "/api/v1/stats/scores?name=hallucination&group_by=day")
	if body.Series[0].Buckets[0].Count != 1 {
		t.Errorf("count = %d after an in-place correction, want the one score",
			body.Series[0].Buckets[0].Count)
	}
}
