package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Datasets, items and runs (spec 014): the nouns an eval needs and the store
// did not have. A dataset is a named, versioned set of test cases; a run is a
// container the harness opens and closes around one pass over it; the traces
// the pass produced carry the run's id in two columns of their own (#2). The
// store never executes anything (#1) — it links, keeps, summarizes and
// compares.
//
// Every write here is a WriteJob (spec 003 #9) and every version number is
// assigned inside its transaction, exactly as prompt versions are (#5): the
// writer is the only goroutine committing, which is what makes the clock
// gapless without a lock of its own.

// Run statuses (#8). The set is closed by a CHECK in schema 0010.
const (
	RunRunning  = "running"
	RunFinished = "finished"
	RunFailed   = "failed"
)

// Dataset is the envelope around a set of items: its name, its description,
// where its version clock stands, and how many items and runs it holds.
type Dataset struct {
	Name        string
	Description string
	// Metadata is raw JSON stored inline; nil when none was sent.
	Metadata []byte
	Version  int
	// ItemCount is the number of live items at the current version, and
	// RunCount the number of runs in any state.
	ItemCount int64
	RunCount  int64
	CreatedAt int64
	UpdatedAt int64
}

// DatasetItem is one row of an item's history: the item as it was at the
// dataset version the row was written at (#5). Every listing resolves one row
// per item, so a caller reading "the dataset at version V" sees each item
// once.
type DatasetItem struct {
	ID string
	// Seq is the item's place in the listing, assigned at its first insert
	// and carried by every later row of it (#21).
	Seq int64
	// Version is the dataset version this row was written at — not the
	// version it was read at, which may be later.
	Version  int
	Archived bool
	// The three bodies are opaque, compacted JSON as first sent (#4, #6,
	// #32); nil when the row carries none.
	Input               []byte
	ExpectedOutput      []byte
	Metadata            []byte
	SourceTraceID       string
	SourceObservationID string
	CreatedAt           int64
}

// DatasetRun is one run of a dataset (#7, #8).
type DatasetRun struct {
	ID             string
	Dataset        string
	DatasetVersion int
	Name           string
	Metadata       []byte
	Status         string
	Error          string
	CreatedAt      int64
	// FinishedAt is zero while the run is still running.
	FinishedAt int64
}

// DatasetCounts is what deleting a dataset would take with it (#20): its live
// items, its runs, and the traces those runs were keeping out of the sweep.
type DatasetCounts struct {
	Items        int64
	Runs         int64
	PinnedTraces int64
}

// DatasetUpsert is PUT /api/v1/datasets/{name}: it creates the envelope or
// replaces its description and metadata, and never touches the items or the
// version clock.
type DatasetUpsert struct {
	ProjectID   string
	Name        string
	Description string
	Metadata    []byte
	Now         int64

	// Dataset is the row after the write, filled by apply.
	Dataset *Dataset
}

func (d *DatasetUpsert) apply(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`INSERT INTO datasets (project_id, name, description, metadata, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, name) DO UPDATE SET
		   description = excluded.description,
		   metadata    = excluded.metadata,
		   updated_at  = excluded.updated_at`,
		d.ProjectID, d.Name, nullString(d.Description), nullJSON(d.Metadata), d.Now, d.Now,
	); err != nil {
		return fmt.Errorf("upsert dataset %s: %w", d.Name, err)
	}
	dataset, err := datasetByName(tx, d.ProjectID, d.Name)
	if err != nil {
		return err
	}
	d.Dataset = dataset
	return nil
}

// DatasetItemInput is one item as a POST carries it, already validated and
// compacted by the handler.
type DatasetItemInput struct {
	ID                  string
	Input               []byte
	ExpectedOutput      []byte
	Metadata            []byte
	SourceTraceID       string
	SourceObservationID string
}

// same reports whether a stored row already says everything this input says.
// The bodies compare as JSON values, not as text (#6, #32): key order, how a
// string was escaped and how a number was spelled do not count as a change,
// so the same cases re-declared through another client land on the same
// version. What is stored stays the compacted text as first sent. The source
// pair takes part too (#23): a case that learned where it came from has
// changed, even if its bodies did not.
func (in *DatasetItemInput) same(row *DatasetItem) bool {
	return row != nil && !row.Archived &&
		sameJSON(row.Input, in.Input) &&
		sameJSON(row.ExpectedOutput, in.ExpectedOutput) &&
		sameJSON(row.Metadata, in.Metadata) &&
		row.SourceTraceID == in.SourceTraceID &&
		row.SourceObservationID == in.SourceObservationID
}

// DatasetItemsWrite is POST /api/v1/datasets/{name}/items: one or many items,
// all or nothing (spec 003 #7), one version tick for the whole batch if
// anything in it changed and none if nothing did (#5, #6). The dataset comes
// into being on the first write to its name.
type DatasetItemsWrite struct {
	ProjectID string
	Dataset   string
	Items     []*DatasetItemInput
	Now       int64

	// Version is the dataset's version after the write and Changed how many
	// items produced a row, both filled by apply.
	Version int
	Changed int
}

func (w *DatasetItemsWrite) apply(tx *sql.Tx) error {
	w.Version, w.Changed = 0, 0

	// The envelope, created if this is the first write to the name. A
	// POST that changes nothing still leaves the row behind, which is what
	// makes "declare the dataset at the top of every CI run" idempotent.
	if _, err := tx.Exec(
		`INSERT INTO datasets (project_id, name, created_at, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(project_id, name) DO NOTHING`,
		w.ProjectID, w.Dataset, w.Now, w.Now); err != nil {
		return fmt.Errorf("create dataset %s: %w", w.Dataset, err)
	}
	var (
		version int
		nextSeq int64
	)
	if err := tx.QueryRow(`SELECT version, next_seq FROM datasets WHERE project_id = ? AND name = ?`,
		w.ProjectID, w.Dataset).Scan(&version, &nextSeq); err != nil {
		return fmt.Errorf("read dataset %s: %w", w.Dataset, err)
	}

	// Decide first, write second: the version advances once for the batch,
	// and only if something in it changed (#6).
	type pending struct {
		input *DatasetItemInput
		seq   int64
	}
	var changed []pending
	for _, item := range w.Items {
		current, err := latestItemRow(tx, w.ProjectID, w.Dataset, item.ID)
		if err != nil {
			return err
		}
		if item.same(current) {
			continue
		}
		seq := nextSeq
		if current != nil {
			// An edit, or a re-post after an archive: the item keeps
			// its place in the listing (#21, edge cases).
			seq = current.Seq
		} else {
			nextSeq++
		}
		changed = append(changed, pending{input: item, seq: seq})
	}
	if len(changed) == 0 {
		w.Version = version
		return nil
	}

	version++
	for _, p := range changed {
		if _, err := tx.Exec(
			`INSERT INTO dataset_items (project_id, dataset, item_id, dataset_version, seq, archived,
			                            input, expected_output, metadata,
			                            source_trace_id, source_observation_id, created_at)
			 VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
			w.ProjectID, w.Dataset, p.input.ID, version, p.seq,
			nullJSON(p.input.Input), nullJSON(p.input.ExpectedOutput), nullJSON(p.input.Metadata),
			nullString(p.input.SourceTraceID), nullString(p.input.SourceObservationID), w.Now,
		); err != nil {
			return fmt.Errorf("insert item %s of dataset %s: %w", p.input.ID, w.Dataset, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE datasets SET version = ?, next_seq = ?, updated_at = ? WHERE project_id = ? AND name = ?`,
		version, nextSeq, w.Now, w.ProjectID, w.Dataset); err != nil {
		return fmt.Errorf("advance dataset %s: %w", w.Dataset, err)
	}
	w.Version, w.Changed = version, len(changed)
	return nil
}

// DatasetItemArchive is DELETE /api/v1/datasets/{name}/items/{id}: a new row
// at a new version with `archived = 1`, so the item is gone from the current
// version and still there at every earlier one (#5).
type DatasetItemArchive struct {
	ProjectID string
	Dataset   string
	ItemID    string
	Now       int64

	// Version is the version the archive landed on, filled by apply.
	Version int
}

func (a *DatasetItemArchive) apply(tx *sql.Tx) error {
	a.Version = 0
	current, err := latestItemRow(tx, a.ProjectID, a.Dataset, a.ItemID)
	if err != nil {
		return err
	}
	if current == nil || current.Archived {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf(
			"dataset %q has no item %s at its current version", a.Dataset, a.ItemID)}
	}
	var version int
	if err := tx.QueryRow(`SELECT version FROM datasets WHERE project_id = ? AND name = ?`,
		a.ProjectID, a.Dataset).Scan(&version); err != nil {
		return fmt.Errorf("read dataset %s: %w", a.Dataset, err)
	}
	version++
	// The archived row keeps the bodies of the row it retires, so an item's
	// history reads as what was there at each version, including the last.
	if _, err := tx.Exec(
		`INSERT INTO dataset_items (project_id, dataset, item_id, dataset_version, seq, archived,
		                            input, expected_output, metadata,
		                            source_trace_id, source_observation_id, created_at)
		 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
		a.ProjectID, a.Dataset, a.ItemID, version, current.Seq,
		nullJSON(current.Input), nullJSON(current.ExpectedOutput), nullJSON(current.Metadata),
		nullString(current.SourceTraceID), nullString(current.SourceObservationID), a.Now,
	); err != nil {
		return fmt.Errorf("archive item %s of dataset %s: %w", a.ItemID, a.Dataset, err)
	}
	if _, err := tx.Exec(
		`UPDATE datasets SET version = ?, updated_at = ? WHERE project_id = ? AND name = ?`,
		version, a.Now, a.ProjectID, a.Dataset); err != nil {
		return fmt.Errorf("advance dataset %s: %w", a.Dataset, err)
	}
	a.Version = version
	return nil
}

// DatasetDelete is the confirmed half of DELETE /api/v1/datasets/{name}
// (#20): the dataset, every version of every item and every run go, and the
// traces the runs held are returned to the retention window rather than
// deleted. The echo is checked inside the transaction like every other
// destructive job (spec 005 #8).
type DatasetDelete struct {
	ProjectID string
	Name      string
	Confirm   string

	// Counts is what went, filled by apply.
	Counts DatasetCounts
}

func (d *DatasetDelete) apply(tx *sql.Tx) error {
	d.Counts = DatasetCounts{}
	dataset, err := datasetByName(tx, d.ProjectID, d.Name)
	if err != nil {
		return err
	}
	if dataset == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("dataset %q not found", d.Name)}
	}
	if d.Confirm != d.Name {
		return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the dataset's name, %q, for this to happen", d.Name)}
	}
	counts, err := datasetCounts(tx, d.ProjectID, d.Name)
	if err != nil {
		return err
	}
	// The items and the runs go through the cascades of schema 0010. The
	// traces stay: their columns still name the run, which now names
	// nothing, and the next sweep treats them as any other trace (#3).
	if _, err := tx.Exec(`DELETE FROM datasets WHERE project_id = ? AND name = ?`,
		d.ProjectID, d.Name); err != nil {
		return fmt.Errorf("delete dataset %s: %w", d.Name, err)
	}
	d.Counts = counts
	return nil
}

// RunCreate is POST /api/v1/datasets/{name}/runs (#3, #7, #9). A run pins
// the dataset's version at this instant unless the harness names an older one,
// and a client-supplied id makes the create itself retry-safe: a second POST
// with the same id returns the existing run unchanged.
type RunCreate struct {
	ProjectID string
	Dataset   string
	ID        string
	Name      string
	Metadata  []byte
	// DatasetVersion is the explicit pin, or nil for the current version.
	DatasetVersion *int
	Now            int64

	// Run is the row after the write and Existed reports whether it was
	// already there, both filled by apply.
	Run     *DatasetRun
	Existed bool
}

func (r *RunCreate) apply(tx *sql.Tx) error {
	r.Run, r.Existed = nil, false
	dataset, err := datasetByName(tx, r.ProjectID, r.Dataset)
	if err != nil {
		return err
	}
	if dataset == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("dataset %q not found", r.Dataset)}
	}
	existing, err := runByID(tx, r.ProjectID, r.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Dataset != r.Dataset {
			// The same id under another dataset is not a retry, it is
			// two harnesses sharing an id; answering with the other
			// dataset's run would hand one of them the wrong container.
			return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
				"run %s already exists in dataset %q", r.ID, existing.Dataset)}
		}
		r.Run, r.Existed = existing, true
		return nil
	}
	version := dataset.Version
	if r.DatasetVersion != nil {
		if *r.DatasetVersion > dataset.Version {
			return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
				"dataset %q is at version %d; a run cannot pin version %d",
				r.Dataset, dataset.Version, *r.DatasetVersion)}
		}
		version = *r.DatasetVersion
	}
	if _, err := tx.Exec(
		`INSERT INTO dataset_runs (project_id, id, dataset, dataset_version, name, metadata, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProjectID, r.ID, r.Dataset, version, nullString(r.Name), nullJSON(r.Metadata), RunRunning, r.Now,
	); err != nil {
		return fmt.Errorf("create run %s: %w", r.ID, err)
	}
	r.Run, err = runByID(tx, r.ProjectID, r.ID)
	return err
}

// RunFinish is POST /api/v1/runs/{id}/finish (#8): the harness says the run
// is over, as finished or as failed with its reason. The store never infers
// it, and a run closed twice is a 409.
type RunFinish struct {
	ProjectID string
	ID        string
	Status    string
	Error     string
	Now       int64

	Run *DatasetRun
}

func (f *RunFinish) apply(tx *sql.Tx) error {
	f.Run = nil
	run, err := runByID(tx, f.ProjectID, f.ID)
	if err != nil {
		return err
	}
	if run == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("run %s not found", f.ID)}
	}
	if run.Status != RunRunning {
		return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
			"run %s is already %s", f.ID, run.Status)}
	}
	if _, err := tx.Exec(
		`UPDATE dataset_runs SET status = ?, error = ?, finished_at = ? WHERE project_id = ? AND id = ?`,
		f.Status, nullString(f.Error), f.Now, f.ProjectID, f.ID); err != nil {
		return fmt.Errorf("finish run %s: %w", f.ID, err)
	}
	f.Run, err = runByID(tx, f.ProjectID, f.ID)
	return err
}

// RunDelete is DELETE /api/v1/runs/{id} (#20): one row of bookkeeping goes,
// and the traces it held are released to the ordinary window — not deleted.
// It is the release valve of the pin (#13), and a harness that creates a run
// per CI job prunes old ones with it from a script.
type RunDelete struct {
	ProjectID string
	ID        string

	// Released is how many traces the run was keeping from the sweep,
	// filled by apply.
	Released int64
}

func (d *RunDelete) apply(tx *sql.Tx) error {
	d.Released = 0
	run, err := runByID(tx, d.ProjectID, d.ID)
	if err != nil {
		return err
	}
	if run == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("run %s not found", d.ID)}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM traces WHERE project_id = ? AND run_id = ?`,
		d.ProjectID, d.ID).Scan(&d.Released); err != nil {
		return fmt.Errorf("count run %s traces: %w", d.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM dataset_runs WHERE project_id = ? AND id = ?`,
		d.ProjectID, d.ID); err != nil {
		return fmt.Errorf("delete run %s: %w", d.ID, err)
	}
	return nil
}

// Datasets lists a project's datasets by name (API contract). Ordering by
// name is what makes the cursor a keyset, exactly as it is for prompts.
// Backward pages towards the start of the alphabet; rows still come back in
// name order either way (spec 009 #2).
func (s *Store) Datasets(projectID string, limit int, after string, backward bool) ([]*Dataset, error) {
	comparison, order := ">", "ASC"
	if backward {
		comparison, order = "<", "DESC"
	}
	query := `SELECT ` + datasetColumns + ` FROM datasets d WHERE d.project_id = ?`
	args := []any{projectID}
	if after != "" {
		query += ` AND d.name ` + comparison + ` ?`
		args = append(args, after)
	}
	query += ` ORDER BY d.name ` + order + ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	defer rows.Close()

	var out []*Dataset
	for rows.Next() {
		dataset, err := scanDataset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dataset)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if backward {
		slices.Reverse(out)
	}
	return out, nil
}

// Dataset returns one dataset with its counts, or nil when the name is
// unknown.
func (s *Store) Dataset(projectID, name string) (*Dataset, error) {
	dataset, err := scanDataset(s.db.QueryRow(
		`SELECT `+datasetColumns+` FROM datasets d WHERE d.project_id = ? AND d.name = ?`,
		projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return dataset, err
}

// DatasetPreview counts what deleting a dataset would take (#20), outside
// any transaction: it is the dry run, and the confirmed job counts again for
// itself.
func (s *Store) DatasetPreview(projectID, name string) (*Dataset, DatasetCounts, error) {
	dataset, err := s.Dataset(projectID, name)
	if err != nil || dataset == nil {
		return nil, DatasetCounts{}, err
	}
	counts := DatasetCounts{Items: dataset.ItemCount, Runs: dataset.RunCount}
	if err := s.db.QueryRow(pinnedByDatasetQuery, projectID, name).Scan(&counts.PinnedTraces); err != nil {
		return nil, counts, fmt.Errorf("count dataset %s pinned traces: %w", name, err)
	}
	return dataset, counts, nil
}

// datasetColumns is the one SELECT list every dataset read shares. The two
// counts are correlated subqueries: a dataset has a handful of runs and a page
// of datasets is short, and the item count at the current version is the
// same "items at V" query every read here uses (data contract).
const datasetColumns = `d.name, d.description, d.metadata, d.version, d.created_at, d.updated_at,
	(` + itemCountAtVersion + `),
	(SELECT COUNT(*) FROM dataset_runs r WHERE r.project_id = d.project_id AND r.dataset = d.name)`

// itemCountAtVersion counts the live items of dataset `d` at its own version.
const itemCountAtVersion = `SELECT COUNT(*) FROM dataset_items i
	 WHERE i.project_id = d.project_id AND i.dataset = d.name AND i.archived = 0
	   AND i.dataset_version = (SELECT MAX(dataset_version) FROM dataset_items
	                             WHERE project_id = i.project_id AND dataset = i.dataset
	                               AND item_id = i.item_id AND dataset_version <= d.version)`

// pinnedByDatasetQuery counts the traces the runs of one dataset keep out of
// the sweep.
const pinnedByDatasetQuery = `SELECT COUNT(*) FROM traces t
	 WHERE t.project_id = ? AND t.run_id IN
	       (SELECT id FROM dataset_runs r WHERE r.project_id = t.project_id AND r.dataset = ?)`

func scanDataset(row scanner) (*Dataset, error) {
	var (
		dataset     Dataset
		description sql.NullString
		metadata    sql.NullString
	)
	if err := row.Scan(&dataset.Name, &description, &metadata, &dataset.Version,
		&dataset.CreatedAt, &dataset.UpdatedAt, &dataset.ItemCount, &dataset.RunCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan dataset: %w", err)
	}
	dataset.Description = description.String
	if metadata.Valid {
		dataset.Metadata = []byte(metadata.String)
	}
	return &dataset, nil
}

func datasetByName(tx *sql.Tx, projectID, name string) (*Dataset, error) {
	dataset, err := scanDataset(tx.QueryRow(
		`SELECT `+datasetColumns+` FROM datasets d WHERE d.project_id = ? AND d.name = ?`,
		projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return dataset, err
}

func datasetCounts(tx *sql.Tx, projectID, name string) (DatasetCounts, error) {
	dataset, err := datasetByName(tx, projectID, name)
	if err != nil || dataset == nil {
		return DatasetCounts{}, err
	}
	counts := DatasetCounts{Items: dataset.ItemCount, Runs: dataset.RunCount}
	if err := tx.QueryRow(pinnedByDatasetQuery, projectID, name).Scan(&counts.PinnedTraces); err != nil {
		return counts, fmt.Errorf("count dataset %s pinned traces: %w", name, err)
	}
	return counts, nil
}

// DatasetItemFilter is one page of "the dataset at version V" (#19, #21).
type DatasetItemFilter struct {
	// Version is the dataset version to resolve the items at.
	Version int
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After is the seq of the last row of the previous page; nil starts at
	// whichever end Backward names.
	After *int64
	// Backward pages towards the start of the listing. Rows still come
	// back in `seq` order either way.
	Backward bool
}

// itemColumns is the SELECT list every item read shares.
const itemColumns = `i.item_id, i.seq, i.dataset_version, i.archived, i.input, i.expected_output,
	i.metadata, i.source_trace_id, i.source_observation_id, i.created_at`

// itemsAtVersion is the "items at version V" predicate of the data contract:
// per item, the row with the greatest version at or below V. Binds the
// version.
const itemsAtVersion = `i.dataset_version = (SELECT MAX(dataset_version) FROM dataset_items
	 WHERE project_id = i.project_id AND dataset = i.dataset
	   AND item_id = i.item_id AND dataset_version <= ?)`

// DatasetItems lists the live items of a dataset at a version, in `seq`
// order, whole (#19).
func (s *Store) DatasetItems(projectID, dataset string, filter DatasetItemFilter) ([]*DatasetItem, error) {
	comparison, order := ">", "ASC"
	if filter.Backward {
		comparison, order = "<", "DESC"
	}
	where := []string{"i.project_id = ?", "i.dataset = ?", itemsAtVersion, "i.archived = 0"}
	args := []any{projectID, dataset, filter.Version}
	if filter.After != nil {
		where = append(where, "i.seq "+comparison+" ?")
		args = append(args, *filter.After)
	}
	args = append(args, filter.Limit)

	rows, err := s.db.Query(
		`SELECT `+itemColumns+` FROM dataset_items i WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY i.seq `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list items of dataset %s: %w", dataset, err)
	}
	defer rows.Close()

	var out []*DatasetItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// DatasetItem returns one live item as of a version, or nil when the item did
// not exist — or was archived — at that version.
func (s *Store) DatasetItem(projectID, dataset, itemID string, version int) (*DatasetItem, error) {
	item, err := scanItem(s.db.QueryRow(
		`SELECT `+itemColumns+` FROM dataset_items i
		 WHERE i.project_id = ? AND i.dataset = ? AND i.item_id = ? AND `+itemsAtVersion+` AND i.archived = 0`,
		projectID, dataset, itemID, version))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

// DatasetItemVersions lists every row of one item, newest first, archived
// rows included (API contract). An item's history is a handful of rows, so it
// comes back whole.
func (s *Store) DatasetItemVersions(projectID, dataset, itemID string) ([]*DatasetItem, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+` FROM dataset_items i
		 WHERE i.project_id = ? AND i.dataset = ? AND i.item_id = ?
		 ORDER BY i.dataset_version DESC`, projectID, dataset, itemID)
	if err != nil {
		return nil, fmt.Errorf("list versions of item %s: %w", itemID, err)
	}
	defer rows.Close()

	var out []*DatasetItem
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// latestItemRow is the item's newest row of any kind, archived or not, inside
// a write transaction: the row a write compares itself against (#6) and the
// row that says whether an archive has anything to archive.
func latestItemRow(tx *sql.Tx, projectID, dataset, itemID string) (*DatasetItem, error) {
	item, err := scanItem(tx.QueryRow(
		`SELECT `+itemColumns+` FROM dataset_items i
		 WHERE i.project_id = ? AND i.dataset = ? AND i.item_id = ?
		 ORDER BY i.dataset_version DESC LIMIT 1`, projectID, dataset, itemID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

func scanItem(row scanner) (*DatasetItem, error) {
	var (
		item              DatasetItem
		archived          int
		input             sql.NullString
		expected          sql.NullString
		metadata          sql.NullString
		sourceTrace       sql.NullString
		sourceObservation sql.NullString
	)
	if err := row.Scan(&item.ID, &item.Seq, &item.Version, &archived, &input, &expected,
		&metadata, &sourceTrace, &sourceObservation, &item.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan dataset item: %w", err)
	}
	item.Archived = archived == 1
	if input.Valid {
		item.Input = []byte(input.String)
	}
	if expected.Valid {
		item.ExpectedOutput = []byte(expected.String)
	}
	if metadata.Valid {
		item.Metadata = []byte(metadata.String)
	}
	item.SourceTraceID, item.SourceObservationID = sourceTrace.String, sourceObservation.String
	return &item, nil
}

// RunCursor is the keyset of the last row of a page of runs: the pair the
// listing sorts by.
type RunCursor struct {
	CreatedAt int64
	ID        string
}

// RunFilter narrows a listing of runs. With a Dataset it is the per-dataset
// listing of spec 014; without one it is the project-wide listing of spec 016
// #2, which walks `idx_dataset_runs_created` instead of the dataset's own
// index. Status is exact and optional.
type RunFilter struct {
	Dataset  string
	Status   string
	Limit    int
	After    *RunCursor
	Backward bool
}

// runConditions is the WHERE clause both the listing and its count share, so
// the number beside a page counts the rows the page is cut from.
func runConditions(projectID string, filter RunFilter) ([]string, []any) {
	where := []string{"project_id = ?"}
	args := []any{projectID}
	if filter.Dataset != "" {
		where = append(where, "dataset = ?")
		args = append(args, filter.Dataset)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	return where, args
}

// runsQuery builds the listing: newest first by `(created_at, id)`, seeking
// past the cursor when there is one. Exposed to the plan test, which holds
// the project-wide shape to the index spec 016 #2 added for it.
func runsQuery(projectID string, filter RunFilter) (string, []any) {
	comparison, order := "<", "DESC"
	if filter.Backward {
		comparison, order = ">", "ASC"
	}
	where, args := runConditions(projectID, filter)
	if filter.After != nil {
		where = append(where, "(created_at, id) "+comparison+" (?, ?)")
		args = append(args, filter.After.CreatedAt, filter.After.ID)
	}
	query := `SELECT ` + runColumns + ` FROM dataset_runs WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at ` + order + `, id ` + order + ` LIMIT ?`
	return query, append(args, filter.Limit)
}

// Runs lists a dataset's runs newest first, paged both ways like every other
// listing (spec 009 #2).
func (s *Store) Runs(projectID, dataset string, limit int, after *RunCursor, backward bool) ([]*DatasetRun, error) {
	return s.ListRuns(projectID, RunFilter{Dataset: dataset, Limit: limit, After: after, Backward: backward})
}

// ListRuns lists runs by the filter, newest first: one dataset's, or the
// whole project's (spec 016 #2).
func (s *Store) ListRuns(projectID string, filter RunFilter) ([]*DatasetRun, error) {
	query, args := runsQuery(projectID, filter)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []*DatasetRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// CountRuns counts the runs a filter matches, up to cap — the capped count of
// spec 009, over the same conditions the listing is cut from.
func (s *Store) CountRuns(projectID string, filter RunFilter, cap int) (int, error) {
	where, args := runConditions(projectID, filter)
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM (SELECT 1 FROM dataset_runs WHERE `+strings.Join(where, " AND ")+` LIMIT ?)`,
		append(args, cap)...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count runs: %w", err)
	}
	return count, nil
}

// Run returns one run, or nil when the id is unknown.
func (s *Store) Run(projectID, id string) (*DatasetRun, error) {
	run, err := scanRun(s.db.QueryRow(
		`SELECT `+runColumns+` FROM dataset_runs WHERE project_id = ? AND id = ?`, projectID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

// PinnedTraces counts the traces a project's live runs keep out of the sweep
// (#13), which `GET /api/v1/system` reports so the operator can see the size
// of the exception.
func (s *Store) PinnedTraces(projectID string) (int64, error) {
	var n int64
	if err := s.db.QueryRow(pinnedTracesQuery, projectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count pinned traces: %w", err)
	}
	return n, nil
}

// pinnedTracesQuery counts one project's pinned traces. `run_id IS NOT NULL`
// is not only a filter: it is what lets the count seek the partial
// `idx_traces_run` (Decision 26) instead of scanning the project's traces,
// which a test on this constant holds to.
const pinnedTracesQuery = `SELECT COUNT(*) FROM traces t
	 WHERE t.project_id = ? AND t.run_id IS NOT NULL
	   AND EXISTS (SELECT 1 FROM dataset_runs r WHERE r.project_id = t.project_id AND r.id = t.run_id)`

const runColumns = `id, dataset, dataset_version, name, metadata, status, error, created_at, finished_at`

func runByID(tx *sql.Tx, projectID, id string) (*DatasetRun, error) {
	run, err := scanRun(tx.QueryRow(
		`SELECT `+runColumns+` FROM dataset_runs WHERE project_id = ? AND id = ?`, projectID, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return run, err
}

func scanRun(row scanner) (*DatasetRun, error) {
	var (
		run        DatasetRun
		name       sql.NullString
		metadata   sql.NullString
		errText    sql.NullString
		finishedAt sql.NullInt64
	)
	if err := row.Scan(&run.ID, &run.Dataset, &run.DatasetVersion, &name, &metadata,
		&run.Status, &errText, &run.CreatedAt, &finishedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan run: %w", err)
	}
	run.Name, run.Error = name.String, errText.String
	if metadata.Valid {
		run.Metadata = []byte(metadata.String)
	}
	run.FinishedAt = finishedAt.Int64
	return &run, nil
}
