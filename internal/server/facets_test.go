package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The facets endpoint and the many-valued filters (spec 027).

// facetsBody is the answer's shape, read loosely enough that a missing key is
// an empty list rather than a decode failure.
type facetsBody struct {
	From        string       `json:"from"`
	To          string       `json:"to"`
	Environment []facetValue `json:"environment"`
	Release     []facetValue `json:"release"`
	Name        []facetValue `json:"name"`
	Omitted     struct {
		Environment int `json:"environment"`
		Release     int `json:"release"`
		Name        int `json:"name"`
	} `json:"omitted"`
}

type facetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func (h *harness) facets(t *testing.T, query string) facetsBody {
	t.Helper()
	rec := h.get(t, "/api/v1/facets"+query)
	expectStatus(t, rec, 200)
	return decodeJSON[facetsBody](t, rec)
}

// facetRange is a query string covering the seeded hour, so that a test says
// which window it is asking about rather than leaning on the default.
func facetRange(hour int64) string {
	from := time.Unix(hour, 0).UTC().Format(time.RFC3339)
	to := time.Unix(hour+3600, 0).UTC().Format(time.RFC3339)
	return "?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to)
}

// seedFacetHour puts one trace in the hour, with a name, an environment and a
// release of the caller's choosing.
func (h *harness) seedFacetHour(t *testing.T, hour int64, n int, name, environment, release string) {
	t.Helper()
	start := hour*int64(time.Second) + int64(n)*int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Name: name,
		Environment: environment, Release: release}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeSpan,
		Level: model.LevelDefault, StartTime: start, EndTime: start + 10*ms,
	})
}

// seedFacetCorpus is the shape the panel is for: one busy environment, two
// thin ones, two names and two releases, plus a trace that names no release.
func (h *harness) seedFacetCorpus(t *testing.T, hour int64) {
	t.Helper()
	h.seedFacetHour(t, hour, 1, "chat", "production", "2026.8.30")
	h.seedFacetHour(t, hour, 2, "chat", "production", "2026.8.30")
	h.seedFacetHour(t, hour, 3, "chat", "production", "2026.8.31")
	h.seedFacetHour(t, hour, 4, "summarize", "staging", "")
	h.seedFacetHour(t, hour, 5, "summarize", "prod", "")
}

// values flattens a list to `value:count` strings, in the order the answer
// carried them — which is what the sort is about.
func values(list []facetValue) []string {
	out := make([]string, 0, len(list))
	for _, one := range list {
		out = append(out, fmt.Sprintf("%s:%d", one.Value, one.Count))
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The answer, from the live scan alone: a project nobody has rolled has a
// watermark of zero, and every query is the tail (spec 013 #5).
func TestFacetsFromTheLiveTail(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)

	body := h.facets(t, facetRange(statsHour))
	// Count descending, then value ascending — so the environment the
	// project actually uses is first and the two typos are told apart by
	// their names rather than by chance.
	if got := values(body.Environment); !equal(got, []string{"production:3", "prod:1", "staging:1"}) {
		t.Errorf("environments = %v", got)
	}
	if got := values(body.Name); !equal(got, []string{"chat:3", "summarize:2"}) {
		t.Errorf("names = %v", got)
	}
	// The empty release is not a value to pick: two traces named none.
	if got := values(body.Release); !equal(got, []string{"2026.8.30:2", "2026.8.31:1"}) {
		t.Errorf("releases = %v", got)
	}
	if body.Omitted.Environment != 0 || body.Omitted.Release != 0 || body.Omitted.Name != 0 {
		t.Errorf("omitted = %+v, want nothing left out", body.Omitted)
	}
	if body.From == "" || body.To == "" {
		t.Error("the answer does not say which range it is for")
	}
}

// A range entirely behind the watermark answers from the rollup, which is what
// makes the list survive the raw rows — asserted the way spec 013's seam test
// asserts it, by emptying the raw tables first.
func TestFacetsBehindTheWatermarkOutliveTheRawRows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	before := h.facets(t, facetRange(statsHour))
	if len(before.Environment) != 3 {
		t.Fatalf("the rollup answered %v; the pass did not run", values(before.Environment))
	}

	if err := h.setRetention(h.project.ID, 1); err != nil {
		t.Fatal(err)
	}
	sweeper := h.store.NewSweeper(h.writer, store.SweepOptions{
		Now: func() time.Time { return time.Unix(statsHour, 0).Add(30 * 24 * time.Hour) },
	})
	if err := sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if remaining := h.countTraces(t); remaining != 0 {
		t.Fatalf("%d traces survived the sweep; the test proves nothing", remaining)
	}

	after := h.facets(t, facetRange(statsHour))
	if got, want := values(after.Environment), values(before.Environment); !equal(got, want) {
		t.Errorf("environments = %v after the raw rows went, want %v", got, want)
	}
	if got, want := values(after.Name), values(before.Name); !equal(got, want) {
		t.Errorf("names = %v after the raw rows went, want %v", got, want)
	}
}

// A range across the watermark sums both halves and counts nothing twice,
// which is the property the whole seam rests on.
func TestFacetsAcrossTheWatermarkCountOnce(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)
	// One hour rolled, the next left to the tail.
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600, 0))
	h.seedFacetHour(t, statsHour+3600, 6, "chat", "production", "2026.8.31")

	from := time.Unix(statsHour, 0).UTC().Format(time.RFC3339)
	to := time.Unix(statsHour+2*3600, 0).UTC().Format(time.RFC3339)
	body := h.facets(t, "?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to))

	if got := values(body.Environment); !equal(got, []string{"production:4", "prod:1", "staging:1"}) {
		t.Errorf("environments = %v; a trace was counted twice or lost at the seam", got)
	}
	if got := values(body.Name); !equal(got, []string{"chat:4", "summarize:2"}) {
		t.Errorf("names = %v", got)
	}
}

// A value ingested while the panel is open is on the list the next time it
// opens, without waiting for a pass: that is what the live tail is for.
func TestANewEnvironmentAppearsWithoutAPass(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600, 0))

	h.seedFacetHour(t, statsHour+3600, 7, "chat", "canary", "2026.9.1")

	from := time.Unix(statsHour, 0).UTC().Format(time.RFC3339)
	to := time.Unix(statsHour+2*3600, 0).UTC().Format(time.RFC3339)
	body := h.facets(t, "?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to))

	var found bool
	for _, one := range body.Environment {
		if one.Value == "canary" && one.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("canary is not on the list: %v", values(body.Environment))
	}
}

// What "no range" means (spec 027 #18): everything the rollup holds, not the
// last thirty days. The listing these boxes filter says *Any time*, so a list
// covering a month sat beside a table covering all of it, and an environment
// last seen forty days ago was in the rows and not in the panel.
func TestFacetsWithNoRangeReachBackAsFarAsTheRollupHolds(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// The fixture hour is months before now, which is exactly the case the
	// thirty-day default used to answer with nothing.
	h.seedFacetCorpus(t, statsHour)

	body := h.facets(t, "")
	if got := values(body.Environment); !equal(got, []string{"production:3", "prod:1", "staging:1"}) {
		t.Errorf("environments = %v over a corpus older than thirty days", got)
	}
}

// And what it does not mean: a live scan into history the rollup no longer
// holds. This endpoint's one exception to spec 013 #13 (#18) — everywhere else
// that rule answers the hours past the window from the raw rows, and here that
// scan would be the whole table, unbounded, to lengthen a list of values to
// tick.
func TestFacetsDoNotScanBelowTheRollupFloor(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600, 0))

	// A raw trace older than every rolled hour: seeded after the pass, so the
	// rollup's floor stays where it is and this hour is only in `traces`.
	h.seedFacetHour(t, statsHour-5*3600, 8, "chat", "archived", "2026.8.1")

	body := h.facets(t, "")
	for _, one := range body.Environment {
		if one.Value == "archived" {
			t.Errorf("environments = %v; the answer reached below the rollup floor",
				values(body.Environment))
		}
	}
	// The rolled hours are still all there — the floor bounds the answer, it
	// does not shorten it.
	if got := values(body.Environment); !equal(got, []string{"production:3", "prod:1", "staging:1"}) {
		t.Errorf("environments = %v", got)
	}

	// And the answer says which range it covered, not which one was asked
	// for: a list claiming all of history while it holds what the rollup
	// holds is the claim the counts exist to make checkable.
	if want := time.Unix(statsHour, 0).UTC().Format(time.RFC3339); body.From != want {
		t.Errorf("from = %q, want the rollup's floor %q", body.From, want)
	}
}

// A value the filter cannot be given is not offered (spec 027 #19). A trace
// name is free text, so this is reachable rather than theoretical: ticking
// `search,web` would write `?name=search,web`, the server would read two
// names, and the listing would come back wrong under a box that stayed ticked.
func TestFacetsOmitValuesTheFilterCannotExpress(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetHour(t, statsHour, 1, "search,web", "production", "2026.8.30")
	h.seedFacetHour(t, statsHour, 2, " padded ", "production", "2026.8.30")
	h.seedFacetHour(t, statsHour, 3, "chat", "production", "2026.8.30")

	// Both halves of the seam have to agree: the live tail first, then the
	// same hour out of the rollup.
	live := h.facets(t, facetRange(statsHour))
	if got := values(live.Name); !equal(got, []string{"chat:1"}) {
		t.Errorf("names from the live tail = %v", got)
	}

	h.rollTheCorpus(t, time.Unix(statsHour+2*3600, 0))
	rolled := h.facets(t, facetRange(statsHour))
	if got := values(rolled.Name); !equal(got, []string{"chat:1"}) {
		t.Errorf("names from the rollup = %v", got)
	}
}

// The cap, and the honesty about it (spec 027 #2): a project that emits a
// release string per commit gets the busiest hundred and a number for the rest.
func TestFacetsAreCappedAndSayHowMany(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// 105 releases: the first is carried by two traces so that the ranking
	// has something to rank, and the other 104 by one each.
	h.seedFacetHour(t, statsHour, 1, "chat", "production", "r-000")
	h.seedFacetHour(t, statsHour, 2, "chat", "production", "r-000")
	for n := 3; n <= 106; n++ {
		h.seedFacetHour(t, statsHour, n, "chat", "production", fmt.Sprintf("r-%03d", n-2))
	}

	body := h.facets(t, facetRange(statsHour))
	if len(body.Release) != 100 {
		t.Fatalf("%d releases came back, want the cap of 100", len(body.Release))
	}
	if body.Omitted.Release != 5 {
		t.Errorf("omitted.release = %d, want 5", body.Omitted.Release)
	}
	if body.Release[0].Value != "r-000" || body.Release[0].Count != 2 {
		t.Errorf("the busiest release is %+v, want r-000 twice over", body.Release[0])
	}
	// The cap is per column: one long list must not truncate the others.
	if len(body.Environment) != 1 || body.Omitted.Environment != 0 {
		t.Errorf("environments = %v, omitted %d", values(body.Environment), body.Omitted.Environment)
	}
}

// The parameters, and what is refused (spec 003 #23's rule, applied here).
func TestFacetsParameterValidation(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)

	expectError(t, h.get(t, "/api/v1/facets?environment=production"), 400,
		"unknown query parameter")
	expectError(t, h.get(t, "/api/v1/facets?from="), 400, "without a value")
	expectError(t, h.get(t, "/api/v1/facets?from=yesterday"), 400, "from")

	// An inverted window is a broken question, and three empty lists with a
	// `200` would be a well-formed answer to it (spec 027 #13).
	expectError(t, h.get(t,
		"/api/v1/facets?from=2026-09-01T00:00:00Z&to=2026-08-01T00:00:00Z"), 400,
		"from must be before to")

	// `?to=` alone is *not* that question any more (#18): `from` starts at the
	// beginning of time, floored at the oldest hour the rollup holds, so a
	// past `to` names a real window — the one ending there — and an empty
	// answer to it is the truth rather than a shrug.
	past := h.facets(t, "?to=2020-01-01T00:00:00Z")
	if len(past.Environment) != 0 {
		t.Errorf("a window ending in 2020 answered %v", values(past.Environment))
	}

	// With no range at all it answers everything the rollup holds, and says
	// which window that was.
	body := h.facets(t, "")
	if body.From == "" || body.To == "" {
		t.Error("the default range is not reported")
	}
	if len(body.Environment) == 0 {
		t.Error("the default range answered nothing over a seeded corpus")
	}
}

// A repeated many-valued parameter is a `400`, not a silent first-wins
// (spec 027 #14). `tag` beside it is repeatable on purpose, which is exactly
// what makes the wrong spelling likely.
func TestARepeatedListParameterIsRefused(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)

	expectError(t, h.get(t, "/api/v1/traces?environment=production&environment=staging"),
		400, "pass one comma-separated list")
	// And a list longer than the cap is refused by the endpoint too, rather
	// than reaching the driver and coming back as `too many SQL variables`
	// (#20).
	expectError(t, h.get(t, "/api/v1/traces?environment="+listOf(facetCap+1)),
		400, "at most 100 values in a list")

	// And `tag`, which means something else by the same spelling, is
	// untouched: two tags are an AND.
	expectStatus(t, h.get(t, "/api/v1/traces?tag=a&tag=b"), 200)
}

// listItems and listOf build a list of n distinct values, and the query string
// spelling it. Long lists are the point: each item is one bound parameter, and
// past SQLite's limit on those an uncapped list came back a `500` (#20).
func listItems(n int) []string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("e%d", i)
	}
	return items
}

func listOf(n int) string { return strings.Join(listItems(n), ",") }

// The parsing itself, at the one place every endpoint reaches it through.
func TestFilterListParsing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string
		fails string
	}{
		{"absent", "", nil, ""},
		{"one value", "environment=production", []string{"production"}, ""},
		{"a list", "environment=production,staging", []string{"production", "staging"}, ""},
		{"trimmed", "environment=production,%20staging", []string{"production", "staging"}, ""},
		{"deduped", "environment=a,b,a", []string{"a", "b"}, ""},
		{"an empty item", "environment=a,,b", nil, "empty item in list"},
		{"a trailing comma", "environment=a,", nil, "empty item in list"},
		{"whitespace as an item", "environment=a,%20", nil, "empty item in list"},
		{"repeated", "environment=a&environment=b", nil, "pass one comma-separated list"},
		{"at the cap", "environment=" + listOf(facetCap), listItems(facetCap), ""},
		{"over the cap", "environment=" + listOf(facetCap+1), nil,
			"at most 100 values in a list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			got, err := filterList(values, "environment")
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.fails)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("filterList = %v, want %v", got, tc.want)
			}
		})
	}
}

// The three filters take a list on every endpoint that takes them (spec 027
// #1), and a trace matches when its column equals any item.
func TestManyValuedFiltersOnEveryEndpoint(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)

	t.Run("the listing", func(t *testing.T) {
		page := decodeJSON[struct {
			Traces []struct {
				ID string `json:"id"`
			} `json:"traces"`
		}](t, h.get(t, "/api/v1/traces?environment=staging,prod"))
		if len(page.Traces) != 2 {
			t.Errorf("%d traces, want the two thin environments", len(page.Traces))
		}
	})

	t.Run("the listing, by name", func(t *testing.T) {
		page := decodeJSON[struct {
			Traces []struct {
				Name string `json:"name"`
			} `json:"traces"`
		}](t, h.get(t, "/api/v1/traces?name=chat,summarize"))
		if len(page.Traces) != 5 {
			t.Errorf("%d traces, want all five", len(page.Traces))
		}
	})

	t.Run("the listing, by release", func(t *testing.T) {
		page := decodeJSON[struct {
			Traces []struct {
				ID string `json:"id"`
			} `json:"traces"`
		}](t, h.get(t, "/api/v1/traces?release=2026.8.30,2026.8.31"))
		if len(page.Traces) != 3 {
			t.Errorf("%d traces, want the three that named a release", len(page.Traces))
		}
	})

	t.Run("traces/last", func(t *testing.T) {
		rec := h.get(t, "/api/v1/traces/last?environment=staging,prod")
		expectStatus(t, rec, 200)
	})

	t.Run("the statistics", func(t *testing.T) {
		buckets := h.statsBuckets(t, "/api/v1/stats?group_by=environment&environment=staging,prod")
		if len(buckets) != 2 {
			t.Errorf("buckets = %+v, want one per environment asked for", buckets)
		}
	})

	t.Run("the score trends", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats/scores?environment=staging,prod")
		expectStatus(t, rec, 200)
	})

	t.Run("the session listing", func(t *testing.T) {
		rec := h.get(t, "/api/v1/sessions?environment=staging,prod")
		expectStatus(t, rec, 200)
	})
}

// What the list refuses, and what it forgives (spec 027 #1).
func TestListParsing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedFacetCorpus(t, statsHour)

	count := func(path string) int {
		rec := h.get(t, path)
		expectStatus(t, rec, 200)
		return len(decodeJSON[struct {
			Traces []struct{} `json:"traces"`
		}](t, rec).Traces)
	}

	t.Run("an empty item is a 400, on every endpoint that takes the list", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/traces?environment=production,,staging",
			"/api/v1/traces?environment=production,",
			"/api/v1/traces?name=chat,%20",
			"/api/v1/traces?release=,2026.8.30",
			"/api/v1/sessions?environment=production,",
			"/api/v1/stats?environment=production,",
			"/api/v1/stats/scores?environment=production,",
		} {
			expectError(t, h.get(t, path), 400, "empty item in list")
		}
	})

	t.Run("an empty value is the 400 it always was", func(t *testing.T) {
		expectError(t, h.get(t, "/api/v1/traces?environment="), 400, "without a value")
	})

	t.Run("items are trimmed", func(t *testing.T) {
		if got := count("/api/v1/traces?environment=staging,%20prod"); got != 2 {
			t.Errorf("%d traces, want 2 — the space was not trimmed", got)
		}
	})

	t.Run("duplicates collapse", func(t *testing.T) {
		if got := count("/api/v1/traces?environment=staging,staging"); got != 1 {
			t.Errorf("%d traces, want the one staging trace counted once", got)
		}
	})

	t.Run("one value is what it always was", func(t *testing.T) {
		if got := count("/api/v1/traces?environment=production"); got != 3 {
			t.Errorf("%d traces, want 3", got)
		}
	})

	// `/stats/scores?name=` is the *score* name, not the trace name, and is
	// not a list: one score name is what a series is.
	t.Run("the score name is untouched", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats/scores?name=hallucination,thumbs")
		expectStatus(t, rec, 200)
		body := decodeJSON[struct {
			Series []struct{} `json:"series"`
		}](t, rec)
		if len(body.Series) != 0 {
			t.Errorf("%d series, want none: no score is named %q",
				len(body.Series), "hallucination,thumbs")
		}
	})
}
