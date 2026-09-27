package server

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// The facets (spec 027 #2): the distinct values of `environment`, `release`
// and `name` over a range, each with its trace count.
//
// Three columns in one round trip because the panel needs one answer per
// opening; three endpoints would be three fetches racing three spinners. The
// counts ride because they cost nothing from the rollup — a `SUM(count)` over
// rows that are already there — and because they are what tells a reader that
// `prod: 1` is a typo and `production: 4656` is the environment.
//
// It splits at the watermark the way `/stats` does (spec 013 #5), through
// `readFacets` below: the rollup for the whole hours behind it, the live scan
// for the tail and the partial hours at either edge. The live tail is what
// keeps the list *complete* — an environment first seen a minute ago is in the
// panel now — without the scan spec 013 exists to avoid.

// facetCap is how many values one column may carry.
//
// A hundred is already more than a checkbox list can show, and it exists for
// the project that emits a release string per commit. `omitted` says the list
// is not the whole truth rather than pretending it is (spec 027 #2).
const facetCap = 100

// handleFacets serves the values each of the three filters can take.
func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "from", "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Half-open, `from` inclusive and `to` exclusive, exactly as `/stats`
	// reads them.
	//
	// `from` defaults to the beginning of time, which `readFacets` floors at
	// the oldest hour the rollup holds (spec 027 #18). The thirty-day default
	// this replaces was justified as "the listing's own default window", and
	// the listing has none — it says *Any time* — so a list of values from the
	// last thirty days sat beside a table showing all of them, and an
	// environment last seen forty days ago was in the rows and not in the
	// panel.
	from, to := int64(0), time.Now().UnixNano()
	for _, bound := range []struct {
		name   string
		target *int64
	}{
		{"from", &from},
		{"to", &to},
	} {
		raw := values.Get(bound.name)
		if raw == "" {
			continue
		}
		instant, err := parseTime(bound.name, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		*bound.target = instant
	}
	// An inverted or empty window is a `400` rather than three empty lists
	// with a `200` (spec 027 #13): an empty answer to a broken question is
	// indistinguishable from an empty project. Since #18 the defaults cannot
	// invert on their own — `from` starts at zero — so what this catches is
	// the range a caller spelled backwards, which is still worth saying out
	// loud rather than answering.
	if from >= to {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}

	counts := map[string]map[string]int64{}
	fold := func(row store.FacetRow) {
		column := counts[row.Column]
		if column == nil {
			column = map[string]int64{}
			counts[row.Column] = column
		}
		column[row.Value] += row.Count
	}
	// `answered` is the range the endpoint really covered, which is `from`
	// raised to the rollup's floor (#18). The answer reports that rather than
	// what was asked, because a list saying it covers all of history when it
	// covers what the rollup holds is the kind of claim the counts exist to
	// make checkable.
	answered, err := s.readFacets(r.Context(), project.ID, from, to, fold)
	if err != nil {
		readFailed(w, r, "failed to read the filter values", err)
		return
	}

	answer := object{}.
		put("from", formatTime(answered)).
		put("to", formatTime(to))
	omitted := object{}
	for _, column := range store.FacetColumns {
		kept, left := rankFacet(counts[column])
		answer = answer.put(column, kept)
		omitted = omitted.put(column, left)
	}
	writeJSON(w, http.StatusOK, answer.put("omitted", omitted))
}

// rankFacet orders one column's values and applies the cap: by count
// descending, then by value ascending, so a reader sees what the project
// mostly is before what it mostly is not, and two calls in a row agree about
// the order of two values with the same count.
//
// What the cap drops is what was rarest, and how many is what `omitted`
// carries: an answer truncated silently would be a wrong one.
func rankFacet(values map[string]int64) ([]object, int) {
	names := make([]string, 0, len(values))
	for value := range values {
		names = append(names, value)
	}
	sort.Slice(names, func(i, j int) bool {
		if values[names[i]] != values[names[j]] {
			return values[names[i]] > values[names[j]]
		}
		return names[i] < names[j]
	})
	omitted := 0
	if len(names) > facetCap {
		omitted = len(names) - facetCap
		names = names[:facetCap]
	}
	rows := make([]object, 0, len(names))
	for _, value := range names {
		rows = append(rows, object{}.put("value", value).put("count", values[value]))
	}
	return rows, omitted
}

// readFacets splits the asked range at the project's watermark, exactly as
// `readStats` and `readScoreTrends` do and for the same reasons: one seam, one
// set of rules, one place for the lag to be stated (spec 027 #3).
//
// A project nobody has rolled has a watermark of zero, so every query is the
// live scan and the first pass is the backfill — and there, with no rollup to
// floor the answer at, "no `from`" really is all of history.
// It answers with the `from` it actually used: the caller's, raised to the
// rollup's floor when there is one.
func (s *Server) readFacets(ctx context.Context, projectID string, from, to int64, fold func(store.FacetRow)) (int64, error) {
	state, err := s.store.RollupState(ctx, projectID)
	if err != nil {
		return from, err
	}

	rolledFrom, rolledTo := int64(0), int64(0)
	if state.RolledUntil > 0 {
		// How far back the rollup can speak for is what it holds, not what
		// a retention window says it should (spec 013 #17). The floor is
		// `stats_hourly`'s own oldest row, which is where one pass wrote all
		// four tables and where one sweep will take them.
		//
		// *Known limit* (spec 027 #17, the shape of spec 023 #15's and spec
		// 025 #21's): `names_hourly` arrived with migration 0016, and its
		// backfill can only fill an hour whose raw traces are still there. On
		// an install whose `retention_days` is shorter than its
		// `stats_retention_days`, the hours between the two windows have
		// environments and releases in `stats_hourly` and no name rows at all
		// — so a range reaching into them answers a full environment list
		// beside a shorter name list. `docs/api.md` says so. Flooring the name
		// column at its own `MIN(hour)` instead would not help: the answer
		// would be just as short, and the two other columns would shrink to
		// match it for no reason.
		oldest, held, err := s.store.OldestRolledHour(ctx, projectID)
		if err != nil {
			return from, err
		}
		if held {
			// The floor of the *whole* answer, not just of its rolled half:
			// `/facets` does not reach into history the rollup no longer
			// holds, which is this endpoint's one exception to spec 013 #13
			// (spec 027 #18). Everywhere else that rule sends the query to a
			// live scan of the raw rows; here that scan would be the whole
			// table, unbounded by anything, to lengthen a list of values to
			// tick by the values that only exist outside the window the
			// operator chose to keep. `stats_retention_days` is how far back
			// this list goes, and that is the same lever as everywhere else.
			from = max(from, oldest*int64(time.Second))
			rolledFrom = max(hourCeiling(from), oldest)
			rolledTo = min(state.RolledUntil, store.HourOf(to))
		}
	}
	if rolledTo <= rolledFrom {
		return from, s.store.FacetTail(ctx, projectID, from, to, fold)
	}

	if err := s.store.FacetRows(ctx, projectID, rolledFrom, rolledTo, fold); err != nil {
		return from, err
	}
	// The partial hour at the head, and everything from the watermark on.
	// The two live segments and the rolled range are disjoint by
	// construction: a trace counted twice would be a count beside a value
	// that no listing can reproduce.
	head := rolledFrom * int64(time.Second)
	if from < head {
		if err := s.store.FacetTail(ctx, projectID, from, head, fold); err != nil {
			return from, err
		}
	}
	if tail := rolledTo * int64(time.Second); tail < to {
		return from, s.store.FacetTail(ctx, projectID, tail, to, fold)
	}
	return from, nil
}
