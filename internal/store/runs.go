package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Reading an eval back (spec 014, part 2): a run's summary, its items with the
// attempts they got, and the comparison of two runs.
//
// Everything here is a read over rows part 1 already writes — the run's traces
// carry its id, the items carry their version — so nothing in this file
// changes the schema. What it does change is where the arithmetic lives: the
// mean of a score, the percentile of a latency and the verdict of a comparison
// are computed once, here, because a client that computes them is a client
// that can disagree with another client (spec 004 #1).

// RunItemCounts is how much of the dataset the run actually covered.
type RunItemCounts struct {
	// Total is the number of live items at the run's dataset version, and
	// Covered how many of them a trace of the run named.
	Total   int64
	Covered int64
	Missing int64
	// Unknown counts the run's *traces* that no item of its version
	// accounts for (Decision 29), not items: a harness that ran newer
	// cases, one that mistyped an id, and one that stamped a run and no
	// item all land here, and `?unknown=true` on the items view shows them.
	Unknown int64
}

// RunTraceStats is what the run's traffic cost and how it went.
type RunTraceStats struct {
	Count int64
	// AttemptsMax is the largest number of traces one item got: a run
	// where some case was retried three times says so here rather than
	// hiding it in an average.
	AttemptsMax int64
	// ErrorCount is how many of the run's traces failed — traces, not
	// observations, the same meaning the session roll-up gives the word
	// (spec 007).
	ErrorCount int64
	// TotalCost is summed over the traces that carried a cost and is nil
	// when none did: absent cost stays absent, it is not zero
	// (spec 002 #14).
	TotalCost *float64
	// LatencyP50 and LatencyP95 are exact nearest-rank percentiles over
	// the run's traces, not the histogram of spec 013: a run is hundreds
	// of traces, and at that size the exact number costs one sort and
	// beats a bucket's ±12%.
	LatencyP50 *int64
	LatencyP95 *int64
}

// RunScoreStat is one score name's aggregate over a run.
type RunScoreStat struct {
	Name string
	// DataType is the config's when the name has one, and otherwise the
	// type the run's own scores carry (Decision 30).
	DataType string
	// Direction is the config's; empty when the name has no config, which
	// is what makes a comparison say `changed` rather than `improved`
	// (#16).
	Direction string
	Count     int64
	// Mean, Min and Max are filled for numeric and boolean names.
	Mean *float64
	Min  *float64
	Max  *float64
	// Distribution is filled for categorical names: value to count. Text
	// names report `Count` only — a mean of prose is not a number, and a
	// distribution of free text is a list of the answers.
	Distribution map[string]int64
}

// PromptRef is one prompt an observation of the run ran. Version is nil when
// the client named a prompt without a usable version (spec 012 #5).
type PromptRef struct {
	Name    string
	Version *int64
}

// RunSummary is the answer to "how did this run go" (API contract → Runs).
type RunSummary struct {
	Items   RunItemCounts
	Traces  RunTraceStats
	Scores  []RunScoreStat
	Models  []string
	Prompts []PromptRef
}

// runTraces is the predicate every read here starts from: the traces one run
// holds. It is a seek on the partial idx_traces_run (#26).
const runTraces = `t.project_id = ? AND t.run_id = ?`

// itemAtRunVersion is "this trace's item is a live item of the dataset at the
// run's version", the predicate that splits covered traces from unknown ones.
// Binds the dataset name and the version twice over.
const itemAtRunVersion = `EXISTS (SELECT 1 FROM dataset_items i
	 WHERE i.project_id = t.project_id AND i.dataset = ? AND i.item_id = t.item_id
	   AND i.archived = 0
	   AND i.dataset_version = (SELECT MAX(dataset_version) FROM dataset_items
	                            WHERE project_id = i.project_id AND dataset = i.dataset
	                              AND item_id = i.item_id AND dataset_version <= ?))`

// RunSummary computes one run's summary. Every number is derived at read time
// rather than kept on the run row: a run's traces go on arriving after it is
// finished (a late span is still its span), and a stored summary would be
// wrong in exactly the window somebody is watching.
func (s *Store) RunSummary(projectID string, run *DatasetRun) (*RunSummary, error) {
	summary := &RunSummary{}
	var err error
	if summary.Items, err = s.runItemCounts(projectID, run); err != nil {
		return nil, err
	}
	if summary.Traces, err = s.runTraceStats(projectID, run.ID); err != nil {
		return nil, err
	}
	if summary.Scores, err = s.runScoreStats(projectID, run.ID); err != nil {
		return nil, err
	}
	if summary.Models, summary.Prompts, err = s.runModelsAndPrompts(projectID, run.ID); err != nil {
		return nil, err
	}
	return summary, nil
}

func (s *Store) runItemCounts(projectID string, run *DatasetRun) (RunItemCounts, error) {
	var counts RunItemCounts
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM dataset_items i
		 WHERE i.project_id = ? AND i.dataset = ? AND i.archived = 0 AND `+itemsAtVersion,
		projectID, run.Dataset, run.DatasetVersion).Scan(&counts.Total); err != nil {
		return counts, fmt.Errorf("count items of run %s: %w", run.ID, err)
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT t.item_id) FROM traces t
		 WHERE `+runTraces+` AND t.item_id IS NOT NULL AND `+itemAtRunVersion,
		projectID, run.ID, run.Dataset, run.DatasetVersion).Scan(&counts.Covered); err != nil {
		return counts, fmt.Errorf("count covered items of run %s: %w", run.ID, err)
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM traces t
		 WHERE `+runTraces+` AND (t.item_id IS NULL OR NOT `+itemAtRunVersion+`)`,
		projectID, run.ID, run.Dataset, run.DatasetVersion).Scan(&counts.Unknown); err != nil {
		return counts, fmt.Errorf("count unknown items of run %s: %w", run.ID, err)
	}
	counts.Missing = counts.Total - counts.Covered
	return counts, nil
}

func (s *Store) runTraceStats(projectID, runID string) (RunTraceStats, error) {
	var (
		stats RunTraceStats
		cost  sql.NullFloat64
	)
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(t.error_count > 0), 0), SUM(t.total_cost)
		 FROM traces t WHERE `+runTraces,
		projectID, runID).Scan(&stats.Count, &stats.ErrorCount, &cost); err != nil {
		return stats, fmt.Errorf("read trace stats of run %s: %w", runID, err)
	}
	if cost.Valid {
		stats.TotalCost = &cost.Float64
	}
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(attempts), 0) FROM
		   (SELECT COUNT(*) AS attempts FROM traces t
		    WHERE `+runTraces+` AND t.item_id IS NOT NULL GROUP BY t.item_id)`,
		projectID, runID).Scan(&stats.AttemptsMax); err != nil {
		return stats, fmt.Errorf("read attempts of run %s: %w", runID, err)
	}

	// Exact percentiles need the values, and a run's worth of them is a
	// page of integers. The sort is SQLite's, on the same index scan.
	rows, err := s.db.Query(
		`SELECT t.latency_ms FROM traces t
		 WHERE `+runTraces+` AND t.latency_ms IS NOT NULL ORDER BY t.latency_ms`,
		projectID, runID)
	if err != nil {
		return stats, fmt.Errorf("read latencies of run %s: %w", runID, err)
	}
	defer rows.Close()
	var latencies []int64
	for rows.Next() {
		var latency int64
		if err := rows.Scan(&latency); err != nil {
			return stats, fmt.Errorf("scan latency: %w", err)
		}
		latencies = append(latencies, latency)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	stats.LatencyP50 = exactPercentile(latencies, 50)
	stats.LatencyP95 = exactPercentile(latencies, 95)
	return stats, nil
}

// exactPercentile is the nearest-rank percentile of a sorted slice — the same
// definition Histogram.Percentile uses, without the bucket rounding.
func exactPercentile(sorted []int64, p int) *int64 {
	if len(sorted) == 0 {
		return nil
	}
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	value := sorted[rank-1]
	return &value
}

// runScoreStats aggregates the run's scores by name. The rows come back
// grouped by name and data type: a name that carried two types in one run —
// which #15 allows, since a config is a rule for what comes next — is
// reported under the type most of its scores used (Decision 30).
func (s *Store) runScoreStats(projectID, runID string) ([]RunScoreStat, error) {
	rows, err := s.db.Query(
		`SELECT s.name, s.data_type, COUNT(*), AVG(s.value), MIN(s.value), MAX(s.value)
		 FROM scores s JOIN traces t ON t.project_id = s.project_id AND t.id = s.trace_id
		 WHERE `+runTraces+`
		 GROUP BY s.name, s.data_type ORDER BY s.name, COUNT(*) DESC, s.data_type`,
		projectID, runID)
	if err != nil {
		return nil, fmt.Errorf("read scores of run %s: %w", runID, err)
	}
	defer rows.Close()

	var (
		stats  []RunScoreStat
		byName = map[string]int{}
	)
	for rows.Next() {
		var (
			name, dataType  string
			count           int64
			mean, low, high sql.NullFloat64
		)
		if err := rows.Scan(&name, &dataType, &count, &mean, &low, &high); err != nil {
			return nil, fmt.Errorf("scan score aggregate: %w", err)
		}
		// The first row of a name is its dominant type, because the
		// query orders by count. A second row is scores of another type
		// under the same name: counted, not aggregated, since there is
		// no number that could mean both.
		index, seen := byName[name]
		if !seen {
			byName[name] = len(stats)
			stats = append(stats, RunScoreStat{Name: name, DataType: dataType})
			index = len(stats) - 1
		}
		stat := &stats[index]
		stat.Count += count
		if stat.DataType != dataType {
			continue
		}
		switch dataType {
		case ScoreNumeric, ScoreBoolean:
			if mean.Valid {
				stat.Mean, stat.Min, stat.Max = &mean.Float64, &low.Float64, &high.Float64
			}
		case ScoreCategorical:
			distribution, err := s.scoreDistribution(projectID, runID, name)
			if err != nil {
				return nil, err
			}
			stat.Distribution = distribution
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachScoreConfigs(projectID, stats); err != nil {
		return nil, err
	}
	return stats, nil
}

// scoreDistribution counts one categorical name's values over a run.
func (s *Store) scoreDistribution(projectID, runID, name string) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT s.string_value, COUNT(*)
		 FROM scores s JOIN traces t ON t.project_id = s.project_id AND t.id = s.trace_id
		 WHERE `+runTraces+` AND s.name = ? AND s.data_type = ? AND s.string_value IS NOT NULL
		 GROUP BY s.string_value ORDER BY s.string_value`,
		projectID, runID, name, ScoreCategorical)
	if err != nil {
		return nil, fmt.Errorf("read distribution of %s: %w", name, err)
	}
	defer rows.Close()

	distribution := map[string]int64{}
	for rows.Next() {
		var (
			value string
			count int64
		)
		if err := rows.Scan(&value, &count); err != nil {
			return nil, fmt.Errorf("scan distribution: %w", err)
		}
		distribution[value] = count
	}
	return distribution, rows.Err()
}

// attachScoreConfigs fills in what the configs say about the names a run
// scored: the type they are supposed to be, and the direction a comparison
// needs to say `improved` rather than `changed` (#15, #16).
func (s *Store) attachScoreConfigs(projectID string, stats []RunScoreStat) error {
	if len(stats) == 0 {
		return nil
	}
	configs, err := s.ScoreConfigs(projectID)
	if err != nil {
		return err
	}
	byName := map[string]*ScoreConfig{}
	for _, config := range configs {
		byName[config.Name] = config
	}
	for i := range stats {
		config, ok := byName[stats[i].Name]
		if !ok {
			continue
		}
		stats[i].DataType = config.DataType
		stats[i].Direction = config.Direction
	}
	return nil
}

// runModelsAndPrompts reads what the run actually ran, from its observations
// rather than from what the harness declared (Decision 12).
func (s *Store) runModelsAndPrompts(projectID, runID string) ([]string, []PromptRef, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT o.model FROM observations o
		 JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		 WHERE `+runTraces+` AND o.model IS NOT NULL AND o.model <> ''
		 ORDER BY o.model`, projectID, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("read models of run %s: %w", runID, err)
	}
	defer rows.Close()
	models := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, nil, fmt.Errorf("scan model: %w", err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	promptRows, err := s.db.Query(
		`SELECT DISTINCT o.prompt_name, o.prompt_version FROM observations o
		 JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		 WHERE `+runTraces+` AND o.prompt_name IS NOT NULL AND o.prompt_name <> ''
		 ORDER BY o.prompt_name, o.prompt_version`, projectID, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("read prompts of run %s: %w", runID, err)
	}
	defer promptRows.Close()
	prompts := []PromptRef{}
	for promptRows.Next() {
		var (
			ref     PromptRef
			version sql.NullInt64
		)
		if err := promptRows.Scan(&ref.Name, &version); err != nil {
			return nil, nil, fmt.Errorf("scan prompt: %w", err)
		}
		if version.Valid {
			ref.Version = &version.Int64
		}
		prompts = append(prompts, ref)
	}
	return models, prompts, promptRows.Err()
}

// RunAttempt is one trace of a run: what it cost, how it went, what its root
// observation answered, and how it was scored.
type RunAttempt struct {
	TraceID    string
	Timestamp  int64
	ErrorCount int
	TotalCost  *float64
	LatencyMs  *int64
	// Output is the root observation's output payload — the earliest
	// starting observation with no parent — and nil when there is none.
	// ObservationID names the observation it came from, so a truncation
	// marker can point at a payload the reader can still fetch whole.
	Output        any
	ObservationID string
	Scores        []*Score
}

// RunItem is one row of the run's item view: a case, and the attempts the run
// made at it. Item is nil for an unknown group — traces of the run whose item
// is not in its version, shown only with `?unknown=true`.
type RunItem struct {
	Item *DatasetItem
	// ItemID is what the traces named, which for an unknown group is the
	// id nothing resolves, and empty for traces that named no item at all.
	ItemID   string
	Seq      int64
	Attempts []*RunAttempt
}

// RunItemFilter pages the item view. The cursor is the `seq` of the last row
// for known items; unknown groups come after every known item and page by
// their id.
type RunItemFilter struct {
	Limit          int
	After          *RunItemCursor
	Backward       bool
	IncludeUnknown bool
}

// RunItemCursor is where the previous page stopped. Bucket 0 is the dataset's
// own items in `seq` order (#21) and bucket 1 the unknown ones: one order over
// two kinds of row, so the cursor stays a single opaque token however the page
// falls.
type RunItemCursor struct {
	Bucket int
	Key    string
}

// Buckets of the item view: the dataset's own items first, in `seq` order
// (#21), then the unknown ones. Two reads rather than one union, because the
// two kinds of row have nothing in common but their place in the order —
// a union would have to invent ten null columns to say so.
const (
	knownItems   = 0
	unknownItems = 1
)

// RunItems lists the items of the run's dataset version with the attempts the
// run made at each, in `seq` order, and — with IncludeUnknown — the traces no
// item of that version accounts for, after them.
func (s *Store) RunItems(projectID string, run *DatasetRun, filter RunItemFilter) ([]*RunItem, error) {
	items, err := s.runItemRows(projectID, run, filter)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return items, nil
	}
	if err := s.attachAttempts(projectID, run.ID, items); err != nil {
		return nil, err
	}
	return items, nil
}

// runItemRows reads one page across the two buckets. Forward, it fills the
// page from the known items and tops it up with unknown ones; backward, it
// does the same from the other end, so a `prev` page is the mirror of the
// `next` that produced it.
func (s *Store) runItemRows(projectID string, run *DatasetRun, filter RunItemFilter) ([]*RunItem, error) {
	first, second := knownItems, unknownItems
	if filter.Backward {
		first, second = unknownItems, knownItems
	}
	var pages [][]*RunItem
	taken := 0
	for _, bucket := range []int{first, second} {
		if taken >= filter.Limit {
			break
		}
		if bucket == unknownItems && !filter.IncludeUnknown {
			continue
		}
		// A cursor inside one bucket skips the buckets before it, and
		// bounds only its own: the row it names is the last one the
		// caller saw.
		var after *string
		if filter.After != nil {
			switch {
			case filter.After.Bucket == bucket:
				after = &filter.After.Key
			case (filter.After.Bucket > bucket) != filter.Backward:
				continue
			}
		}
		page, err := s.runItemBucket(projectID, run, bucket, after, filter.Limit-taken, filter.Backward)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
		taken += len(page)
	}
	// Rows always come back in listing order, whichever way the page was
	// read: backward visits the buckets from the far end, so its pages go
	// back together the other way round.
	if filter.Backward {
		slices.Reverse(pages)
	}
	var out []*RunItem
	for _, page := range pages {
		out = append(out, page...)
	}
	return out, nil
}

// runItemBucket reads one bucket's rows: the dataset's items at the run's
// version, or the item ids the run's traces named that the version does not
// have.
func (s *Store) runItemBucket(projectID string, run *DatasetRun, bucket int,
	after *string, limit int, backward bool) ([]*RunItem, error) {
	comparison, order := ">", "ASC"
	if backward {
		comparison, order = "<", "DESC"
	}

	var (
		query string
		args  []any
	)
	if bucket == knownItems {
		where := []string{"i.project_id = ?", "i.dataset = ?", itemsAtVersion, "i.archived = 0"}
		args = []any{projectID, run.Dataset, run.DatasetVersion}
		if after != nil {
			where = append(where, "printf('%020d', i.seq) "+comparison+" ?")
			args = append(args, *after)
		}
		query = `SELECT ` + itemColumns + ` FROM dataset_items i
		         WHERE ` + strings.Join(where, " AND ") + `
		         ORDER BY i.seq ` + order + ` LIMIT ?`
	} else {
		where := []string{runTraces, "(t.item_id IS NULL OR NOT " + itemAtRunVersion + ")"}
		args = []any{projectID, run.ID, run.Dataset, run.DatasetVersion}
		if after != nil {
			where = append(where, "COALESCE(t.item_id, '') "+comparison+" ?")
			args = append(args, *after)
		}
		query = `SELECT DISTINCT COALESCE(t.item_id, '') FROM traces t
		         WHERE ` + strings.Join(where, " AND ") + `
		         ORDER BY 1 ` + order + ` LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list items of run %s: %w", run.ID, err)
	}
	defer rows.Close()

	var out []*RunItem
	for rows.Next() {
		if bucket == unknownItems {
			var itemID string
			if err := rows.Scan(&itemID); err != nil {
				return nil, fmt.Errorf("scan unknown item: %w", err)
			}
			out = append(out, &RunItem{ItemID: itemID})
			continue
		}
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, &RunItem{Item: item, ItemID: item.ID, Seq: item.Seq})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if backward {
		slices.Reverse(out)
	}
	return out, nil
}

// RunItemKey is the cursor key of one row of the item view: the `seq` as
// fixed-width text for a known item, and the item id for an unknown one, so
// one comparison orders a bucket whichever kind it holds.
func RunItemKey(item *RunItem) RunItemCursor {
	if item.Item == nil {
		return RunItemCursor{Bucket: unknownItems, Key: item.ItemID}
	}
	return RunItemCursor{Bucket: knownItems, Key: fmt.Sprintf("%020d", item.Seq)}
}

// attachAttempts fills a page of items with the run's traces for them: one
// query for the traces, one for their root observations' outputs and one for
// their scores, rather than three per row.
func (s *Store) attachAttempts(projectID, runID string, items []*RunItem) error {
	byItem := map[string]*RunItem{}
	ids := make([]any, 0, len(items))
	for _, item := range items {
		byItem[item.ItemID] = item
		ids = append(ids, item.ItemID)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")

	args := append([]any{projectID, runID}, ids...)
	rows, err := s.db.Query(
		`SELECT COALESCE(t.item_id, ''), t.id, t.timestamp, t.error_count, t.total_cost, t.latency_ms
		 FROM traces t WHERE `+runTraces+`
		   AND COALESCE(t.item_id, '') IN (`+placeholders+`)
		 ORDER BY t.timestamp, t.id`, args...)
	if err != nil {
		return fmt.Errorf("read attempts of run %s: %w", runID, err)
	}
	defer rows.Close()

	byTrace := map[string]*RunAttempt{}
	traceIDs := make([]any, 0, len(items))
	for rows.Next() {
		var (
			itemID    string
			attempt   RunAttempt
			timestamp sql.NullInt64
			cost      sql.NullFloat64
			latency   sql.NullInt64
		)
		if err := rows.Scan(&itemID, &attempt.TraceID, &timestamp,
			&attempt.ErrorCount, &cost, &latency); err != nil {
			return fmt.Errorf("scan attempt: %w", err)
		}
		attempt.Timestamp = timestamp.Int64
		if cost.Valid {
			attempt.TotalCost = &cost.Float64
		}
		if latency.Valid {
			attempt.LatencyMs = &latency.Int64
		}
		item, ok := byItem[itemID]
		if !ok {
			continue
		}
		stored := &attempt
		item.Attempts = append(item.Attempts, stored)
		byTrace[attempt.TraceID] = stored
		traceIDs = append(traceIDs, attempt.TraceID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(traceIDs) == 0 {
		return nil
	}
	if err := s.attachOutputs(projectID, traceIDs, byTrace); err != nil {
		return err
	}
	return s.attachScores(projectID, traceIDs, byTrace)
}

// attachOutputs reads each trace's root observation — the earliest starting
// span with no parent — and its output payload. The root is where a harness
// puts the answer it produced for a case; a trace whose root carried no output
// shows null rather than reaching down the tree for something that might be
// another span's business.
func (s *Store) attachOutputs(projectID string, traceIDs []any, byTrace map[string]*RunAttempt) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(traceIDs)), ", ")
	args := append([]any{projectID}, traceIDs...)
	rows, err := s.db.Query(
		`SELECT trace_id, id, output_id FROM
		   (SELECT o.trace_id AS trace_id, o.id AS id, o.output_id AS output_id,
		           ROW_NUMBER() OVER (PARTITION BY o.trace_id
		                              ORDER BY o.start_time, o.id) AS rank
		    FROM observations o
		    WHERE o.project_id = ? AND o.trace_id IN (`+placeholders+`)
		      AND (o.parent_observation_id IS NULL OR o.parent_observation_id = ''))
		 WHERE rank = 1`, args...)
	if err != nil {
		return fmt.Errorf("read run outputs: %w", err)
	}
	defer rows.Close()

	type root struct {
		traceID       string
		observationID string
		outputID      sql.NullInt64
	}
	var roots []root
	for rows.Next() {
		var found root
		if err := rows.Scan(&found.traceID, &found.observationID, &found.outputID); err != nil {
			return fmt.Errorf("scan run output: %w", err)
		}
		roots = append(roots, found)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The payload reads happen after the cursor is drained: readPayload
	// runs its own query, and SQLite will not have two open on one
	// connection mid-scan.
	for _, found := range roots {
		attempt, ok := byTrace[found.traceID]
		if !ok {
			continue
		}
		attempt.ObservationID = found.observationID
		output, err := s.readPayload(found.outputID)
		if err != nil {
			return err
		}
		attempt.Output = output
	}
	return nil
}

// attachScores reads the scores of a page's traces. They ride whole: a score
// is a number and a name, and cutting one would save nothing worth the
// ambiguity (#19's reasoning, applied to the smaller thing).
func (s *Store) attachScores(projectID string, traceIDs []any, byTrace map[string]*RunAttempt) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(traceIDs)), ", ")
	args := append([]any{projectID}, traceIDs...)
	rows, err := s.db.Query(
		`SELECT `+scoreColumns+` FROM scores
		 WHERE project_id = ? AND trace_id IN (`+placeholders+`)
		 ORDER BY name, timestamp, id`, args...)
	if err != nil {
		return fmt.Errorf("read run scores: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		score, err := scanScore(rows)
		if err != nil {
			return err
		}
		if attempt, ok := byTrace[score.TraceID]; ok {
			attempt.Scores = append(attempt.Scores, score)
		}
	}
	return rows.Err()
}

// ItemScore is one item's value for one score name inside one run: the mean
// over its attempts for a number, and the newest attempt's word for a
// category (Decision 30).
type ItemScore struct {
	DataType string
	Count    int64
	Mean     *float64
	Text     string
}

// RunItemValues is one run's per-item scores, keyed by item id and then by
// score name, together with the items the run attempted at all.
type RunItemValues struct {
	Attempted map[string]bool
	Scores    map[string]map[string]ItemScore
}

// RunValues reads what a run said about each of its items. The whole run is
// read at once, not paged: the counts a comparison reports — how many items
// improved, regressed and stayed — are statements about the pair of runs, and
// a page cannot produce them (Decision 31).
func (s *Store) RunValues(projectID, runID string) (*RunItemValues, error) {
	values := &RunItemValues{
		Attempted: map[string]bool{},
		Scores:    map[string]map[string]ItemScore{},
	}
	attempted, err := s.db.Query(
		`SELECT DISTINCT t.item_id FROM traces t
		 WHERE `+runTraces+` AND t.item_id IS NOT NULL`, projectID, runID)
	if err != nil {
		return nil, fmt.Errorf("read attempted items of run %s: %w", runID, err)
	}
	defer attempted.Close()
	for attempted.Next() {
		var itemID string
		if err := attempted.Scan(&itemID); err != nil {
			return nil, fmt.Errorf("scan attempted item: %w", err)
		}
		values.Attempted[itemID] = true
	}
	if err := attempted.Err(); err != nil {
		return nil, err
	}

	// Ordered by (item, name, time) so the aggregation below is one pass:
	// the mean accumulates and the newest string is simply the last one
	// seen.
	rows, err := s.db.Query(
		`SELECT t.item_id, s.name, s.data_type, s.value, s.string_value
		 FROM scores s JOIN traces t ON t.project_id = s.project_id AND t.id = s.trace_id
		 WHERE `+runTraces+` AND t.item_id IS NOT NULL
		 ORDER BY t.item_id, s.name, s.timestamp, s.id`, projectID, runID)
	if err != nil {
		return nil, fmt.Errorf("read item scores of run %s: %w", runID, err)
	}
	defer rows.Close()

	sums := map[string]map[string]float64{}
	for rows.Next() {
		var (
			itemID, name, dataType string
			value                  sql.NullFloat64
			text                   sql.NullString
		)
		if err := rows.Scan(&itemID, &name, &dataType, &value, &text); err != nil {
			return nil, fmt.Errorf("scan item score: %w", err)
		}
		if values.Scores[itemID] == nil {
			values.Scores[itemID] = map[string]ItemScore{}
			sums[itemID] = map[string]float64{}
		}
		score := values.Scores[itemID][name]
		score.DataType, score.Count = dataType, score.Count+1
		if value.Valid {
			sums[itemID][name] += value.Float64
			mean := sums[itemID][name] / float64(score.Count)
			score.Mean = &mean
		}
		if text.Valid {
			score.Text = text.String
		}
		values.Scores[itemID][name] = score
	}
	return values, rows.Err()
}

// CompareItem is one item as the comparison sees it: which runs had it, and
// what each of them scored.
type CompareItem struct {
	ItemID     string
	Seq        int64
	InVersionA bool
	InVersionB bool
}

// CompareItems is the union of the two runs' item sets, in `seq` order,
// restricted to the items at least one of them attempted: a case neither run
// ran has nothing to compare, and listing it would bury the ones that do.
func (s *Store) CompareItems(projectID string, a, b *DatasetRun) ([]*CompareItem, error) {
	// One select per side, tagged with which side it came from, so that
	// the group below can say "this item is in a's version and not in b's"
	// — which is the label the comparison owes a reader whose dataset
	// moved between the two runs.
	const side = `SELECT i.item_id AS item_id, i.seq AS seq, ? AS in_a, ? AS in_b
		        FROM dataset_items i
		        WHERE i.project_id = ? AND i.dataset = ? AND ` + itemsAtVersion + ` AND i.archived = 0`
	rows, err := s.db.Query(
		`SELECT u.item_id, MIN(u.seq), MAX(u.in_a), MAX(u.in_b)
		 FROM (`+side+` UNION ALL `+side+`) u
		 WHERE EXISTS (SELECT 1 FROM traces t
		               WHERE t.project_id = ? AND t.item_id = u.item_id AND t.run_id IN (?, ?))
		 GROUP BY u.item_id ORDER BY MIN(u.seq)`,
		1, 0, projectID, a.Dataset, a.DatasetVersion,
		0, 1, projectID, b.Dataset, b.DatasetVersion,
		projectID, a.ID, b.ID)
	if err != nil {
		return nil, fmt.Errorf("compare items of runs %s and %s: %w", a.ID, b.ID, err)
	}
	defer rows.Close()

	var out []*CompareItem
	for rows.Next() {
		var (
			item     CompareItem
			inA, inB int
		)
		if err := rows.Scan(&item.ItemID, &item.Seq, &inA, &inB); err != nil {
			return nil, fmt.Errorf("scan compared item: %w", err)
		}
		item.InVersionA, item.InVersionB = inA == 1, inB == 1
		row := item
		out = append(out, &row)
	}
	return out, rows.Err()
}
