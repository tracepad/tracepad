package server

import (
	"log/slog"
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

// facetCap is how many values one column may carry, and maxFacetRange is the
// window the endpoint answers for when the caller names none.
const (
	// A hundred is already more than a checkbox list can show, and it
	// exists for the project that emits a release string per commit.
	// `omitted` says the list is not the whole truth rather than pretending
	// it is (spec 027 #2).
	facetCap = 100
	// The listing's own default window, so a panel that opens without
	// touching the range control asks about what the listing is showing.
	defaultFacetRange = 30 * 24 * time.Hour
)

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
	// reads them — and defaulted, unlike `/stats`, because a facet list over
	// all of history is a different question from the one the panel is
	// asking (#2).
	now := time.Now()
	from, to := now.Add(-defaultFacetRange).UnixNano(), now.UnixNano()
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

	counts := map[string]map[string]int64{}
	fold := func(row store.FacetRow) {
		column := counts[row.Column]
		if column == nil {
			column = map[string]int64{}
			counts[row.Column] = column
		}
		column[row.Value] += row.Count
	}
	if err := s.readFacets(project.ID, from, to, fold); err != nil {
		slog.Error("read the facets failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the filter values")
		return
	}

	answer := object{}.
		put("from", formatTime(from)).
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
// live scan and the first pass is the backfill.
func (s *Server) readFacets(projectID string, from, to int64, fold func(store.FacetRow)) error {
	state, err := s.store.RollupState(projectID)
	if err != nil {
		return err
	}

	rolledFrom, rolledTo := int64(0), int64(0)
	if state.RolledUntil > 0 {
		// How far back the rollup can speak for is what it holds, not what
		// a retention window says it should (spec 013 #17). The floor is
		// `stats_hourly`'s own oldest row, which is the same floor for all
		// four tables: one pass writes them together and one sweep takes
		// them together.
		oldest, held, err := s.store.OldestRolledHour(projectID)
		if err != nil {
			return err
		}
		if held {
			rolledFrom = max(hourCeiling(max(from, 0)), oldest)
			rolledTo = min(state.RolledUntil, store.HourOf(to))
		}
	}
	if rolledTo <= rolledFrom {
		return s.store.FacetTail(projectID, from, to, fold)
	}

	if err := s.store.FacetRows(projectID, rolledFrom, rolledTo, fold); err != nil {
		return err
	}
	// The partial hour at the head, and everything from the watermark on.
	// The two live segments and the rolled range are disjoint by
	// construction: a trace counted twice would be a count beside a value
	// that no listing can reproduce.
	head := rolledFrom * int64(time.Second)
	if from < head {
		if err := s.store.FacetTail(projectID, from, head, fold); err != nil {
			return err
		}
	}
	if tail := rolledTo * int64(time.Second); tail < to {
		return s.store.FacetTail(projectID, tail, to, fold)
	}
	return nil
}
