package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The facets (spec 027): the distinct values of `environment`, `release` and
// `name` over a time range, each with the traces carrying it.
//
// It is spec 013's read seam a third time — hours behind the watermark from
// the rollup, the tail and the partial hours at either edge from the raw rows
// — because the panel that asks needs the list to be *complete*: an
// environment first seen a minute ago has to be there now, not after the next
// pass, and without the scan spec 013 exists to avoid (the tail is minutes of
// one project).
//
// Two of the three columns are already in `stats_hourly`'s tuple. The third
// gets `names_hourly`, rolled by the same job, frozen by its own rows and
// swept by the same window (#3).

// The three columns a facet answer covers, spelled as the query parameters
// they filter through: one name for the URL, the JSON and the rows below.
const (
	FacetEnvironment = "environment"
	FacetRelease     = "release"
	FacetName        = "name"
)

// FacetColumns is the three of them in the order the answer lists them.
var FacetColumns = []string{FacetEnvironment, FacetRelease, FacetName}

// FacetRow is one value of one column, with how many traces of the range
// carry it. Both halves of the seam yield this shape, so the server folds them
// with one function.
type FacetRow struct {
	Column string
	Value  string
	Count  int64
}

// facetRollupQueries is the rolled half: three statements over two tables.
//
// `environment` and `release` come from the **trace-unit** rows of
// `stats_hourly` — the ones with an empty `model`, which is the discriminator
// spec 013 #1 put there — summing the observation rows too would count a trace once per model
// it used. `name` comes from `names_hourly`, whose every row is a trace unit.
//
// The empty release is left out here rather than by the caller: a trace whose
// client named none is not a value to pick, and `?release=` is a 400 anyway
// (spec 003's empty-value rule). Every statement is a range read over its
// table's primary key, whose leading columns are `(project_id, hour)`.
var facetRollupQueries = []struct {
	column string
	query  string
}{
	{FacetEnvironment, `SELECT environment, SUM(count) FROM stats_hourly
	                     WHERE project_id = ? AND hour >= ? AND hour < ? AND model = ''
	                     GROUP BY environment`},
	{FacetRelease, `SELECT release, SUM(count) FROM stats_hourly
	                 WHERE project_id = ? AND hour >= ? AND hour < ? AND model = '' AND release != ''
	                 GROUP BY release`},
	{FacetName, `SELECT name, SUM(count) FROM names_hourly
	              WHERE project_id = ? AND hour >= ? AND hour < ?
	              GROUP BY name`},
}

// FacetRows reads the rolled values of a half-open hour range.
func (s *Store) FacetRows(ctx context.Context, projectID string, fromHour, toHour int64, yield func(FacetRow)) error {
	for _, source := range facetRollupQueries {
		rows, err := s.db.QueryContext(ctx, source.query, projectID, fromHour, toHour)
		if err != nil {
			return fmt.Errorf("read the %s facet: %w", source.column, err)
		}
		if err := scanFacet(rows, source.column, yield); err != nil {
			return err
		}
	}
	return nil
}

// facetTailQueries is the live half: the same three answers straight off
// `traces`, for the tail past the watermark and for the partial hours at
// either edge that no hourly row can answer.
//
// The `WHERE` of each one is what the rolled half's own exclusions say: a
// trace with no release, and one with no name, are not values to pick. The
// range is bounded by `idx_traces_timestamp`, which is what keeps the tail the
// minutes it is meant to be.
var facetTailQueries = []struct {
	column string
	query  string
}{
	{FacetEnvironment, `SELECT environment, COUNT(*) FROM traces
	                     WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
	                     GROUP BY environment`},
	{FacetRelease, `SELECT release, COUNT(*) FROM traces
	                 WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
	                   AND release IS NOT NULL AND release != ''
	                 GROUP BY release`},
	{FacetName, `SELECT name, COUNT(*) FROM traces
	              WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
	                AND name IS NOT NULL AND name != ''
	              GROUP BY name`},
}

// FacetTail reads the values of a half-open range of trace timestamps.
func (s *Store) FacetTail(ctx context.Context, projectID string, from, to int64, yield func(FacetRow)) error {
	for _, source := range facetTailQueries {
		rows, err := s.db.QueryContext(ctx, source.query, projectID, from, to)
		if err != nil {
			return fmt.Errorf("scan the %s facet: %w", source.column, err)
		}
		if err := scanFacet(rows, source.column, yield); err != nil {
			return err
		}
	}
	return nil
}

// scanFacet folds one statement's rows, skipping the empty value the way both
// halves' `WHERE` clauses already do — belt and braces for `environment`,
// whose column is `NOT NULL DEFAULT 'default'` and so can only be empty if a
// client sent an empty string.
// expressible reports whether a value can be asked for through the filter the
// facets exist to fill in (spec 027 #19).
//
// The list form is comma-separated and its items are trimmed, so a value with
// a comma in it, or with space at either end, cannot be spelled in a URL —
// `docs/api.md` has said so since #1. Offering one as a checkbox would be
// offering a filter that does not work: ticking `search,web` writes
// `?name=search,web`, the server reads two names, the listing comes back
// wrong, and the box stays ticked over it. A trace name is free text, so this
// is reachable rather than theoretical. The empty string is out for the reason
// it always was: it is not a value anyone can pick.
func expressible(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.Contains(value, ",")
}

func scanFacet(rows *sql.Rows, column string, yield func(FacetRow)) error {
	defer rows.Close()
	for rows.Next() {
		var (
			value sql.NullString
			count int64
		)
		if err := rows.Scan(&value, &count); err != nil {
			return fmt.Errorf("scan a %s facet row: %w", column, err)
		}
		if !expressible(value.String) {
			continue
		}
		yield(FacetRow{Column: column, Value: value.String, Count: count})
	}
	return rows.Err()
}

// nameRollQuery recomputes one `(project, hour)` of `names_hourly` from the
// raw rows: named so that a test can hand the shipped SQL to EXPLAIN QUERY
// PLAN (the method of spec 003 #25).
//
// It is the trace half of `rollHour` with a different `GROUP BY`, bounded by
// `idx_traces_timestamp` exactly as that one is. A trace with no name is left
// out for the reason the read side leaves it out: it is not a value anyone can
// filter by.
//
// The arithmetic is SQLite's — there is no histogram here — so the whole hour
// is a `DELETE` plus one `INSERT … SELECT` inside the job's transaction,
// idempotent by construction as spec 013 #3 requires.
const nameRollQuery = `INSERT INTO names_hourly (project_id, hour, name, count, error_count)
	 SELECT ?, ?, name, COUNT(*), SUM(error_count > 0)
	   FROM traces
	  WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
	    AND name IS NOT NULL AND name != ''
	  GROUP BY name`

// rollNameHour replaces one `(project, hour)` of `names_hourly` and reports
// how many names the hour produced.
func rollNameHour(tx *sql.Tx, projectID string, hour int64) (int, error) {
	if _, err := tx.Exec(
		`DELETE FROM names_hourly WHERE project_id = ? AND hour = ?`,
		projectID, hour); err != nil {
		return 0, fmt.Errorf("clear the names of hour %d: %w", hour, err)
	}
	result, err := tx.Exec(nameRollQuery,
		projectID, hour, projectID, hour*1e9, (hour+SecondsPerHour)*1e9)
	if err != nil {
		return 0, fmt.Errorf("write the name rows of hour %d: %w", hour, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(rows), nil
}

// NamesRollupHours reports which hours of a project hold name rows. The tests
// ask; nothing on the read path needs it.
func (s *Store) NamesRollupHours(ctx context.Context, projectID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT hour FROM names_hourly WHERE project_id = ? ORDER BY hour`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read the rolled name hours: %w", err)
	}
	return scanHours(rows)
}

// namesRollupSweep deletes rolled name rows older than the project's stats
// window. One chunk per job, like every other deletion this store does; the
// window is `stats_retention_days` and there is no knob of its own, because
// all four tables are one rollup (spec 027, config additions).
type namesRollupSweep struct {
	background
	ProjectID string
	Before    int64

	Deleted int64
}

func (s *namesRollupSweep) apply(tx *sql.Tx) error {
	result, err := tx.Exec(
		`DELETE FROM names_hourly
		 WHERE rowid IN (SELECT rowid FROM names_hourly
		                 WHERE project_id = ? AND hour < ? LIMIT ?)`,
		s.ProjectID, s.Before, DefaultSweepChunk)
	if err != nil {
		return fmt.Errorf("sweep the name rollup: %w", err)
	}
	s.Deleted, err = result.RowsAffected()
	return err
}
