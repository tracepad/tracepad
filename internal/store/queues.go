package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Annotation queues (spec 024): a named list of traces or observations, the
// score configs a reviewer must fill on each of them, and the bookkeeping that
// says who completed what.
//
// The queue is the question and the configs are its shape (#1); an item is a
// pointer at one trace or one of its observations (#2). The verdicts
// themselves are never stored here — they are scores, written through
// spec 003's endpoint, and *completed* means the server found them all on the
// target (#7). That is why deleting a queue takes its items and nothing else:
// the scores are the product of the work and are attached to the trace.
//
// Every write below is a WriteJob (spec 003 #9), `next` included: handing out
// an item claims it, and a claim is a row that must not race another
// reviewer's.

// Item statuses (#2).
const (
	ItemPending   = "pending"
	ItemCompleted = "completed"
	ItemSkipped   = "skipped"
)

// ItemStatuses is the closed set, for whoever validates a filter.
var ItemStatuses = []string{ItemPending, ItemCompleted, ItemSkipped}

// ClaimTTL is how long `next` holds an item for the annotator it handed it to
// (#5). A constant, documented rather than configurable: it is longer than a
// review and shorter than a coffee, and a knob would make "two people will not
// review one trace twice" depend on how the deployment was set up.
const ClaimTTL = 10 * time.Minute

// AnnotationQueue is one review programme.
type AnnotationQueue struct {
	Name        string
	Description string
	// ScoreConfigs are the names a reviewer must set on every item, in the
	// order the desk asks for them (#1).
	ScoreConfigs []string
	Counts       QueueCounts
	CreatedAt    int64
	UpdatedAt    int64
}

// Same reports whether two queues say the same thing, which is what makes a
// re-PUT of the same body a no-op (#1, the rule spec 014 #17 gave configs).
func (q *AnnotationQueue) Same(other *AnnotationQueue) bool {
	return q.Description == other.Description && slices.Equal(q.ScoreConfigs, other.ScoreConfigs)
}

// QueueCounts is how far a queue has got.
type QueueCounts struct {
	Pending   int64
	Completed int64
	Skipped   int64
}

// Total is how many items the queue holds in any state.
func (c QueueCounts) Total() int64 { return c.Pending + c.Completed + c.Skipped }

// AnnotationItem is one trace, or one observation of a trace, waiting for a
// verdict (#2).
type AnnotationItem struct {
	ID    string
	Queue string
	// TraceID is always set; ObservationID is empty when the item is the
	// trace itself.
	TraceID       string
	ObservationID string
	Status        string
	Seq           int64
	AddedAt       int64
	ClaimedBy     string
	// ClaimedUntil is zero when nothing holds the item.
	ClaimedUntil int64
	CompletedBy  string
	CompletedAt  int64
	SkipReason   string
}

// QueueTarget is what an add names: one trace, or one observation of it.
type QueueTarget struct {
	TraceID       string
	ObservationID string
}

// QueuePut is PUT /api/v1/queues/{name} (#1): create or replace the whole
// queue. A body equal to the stored one writes nothing, so a team that
// declares its queues at the top of a script leaves no trail of updates
// behind.
//
// The configs are checked here rather than in the handler because a config can
// be deleted between a handler-side read and the commit (spec 003 Decision 20,
// spec 014 #15). The list may be changed later and then governs the items not
// yet completed: nothing re-validates what is already done.
type QueuePut struct {
	ProjectID string
	Queue     *AnnotationQueue
	Now       int64

	// Stored is the row after the write and Created reports whether the
	// name was new, both filled by apply.
	Stored  *AnnotationQueue
	Created bool
}

func (p *QueuePut) apply(tx *sql.Tx) error {
	p.Stored, p.Created = nil, false
	for _, name := range p.Queue.ScoreConfigs {
		config, err := scoreConfigByName(tx, p.ProjectID, name)
		if err != nil {
			return err
		}
		if config == nil {
			return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
				"score config %q does not exist; declare it before a queue asks for it", name)}
		}
	}
	existing, err := queueByName(tx, p.ProjectID, p.Queue.Name)
	if err != nil {
		return err
	}
	if existing != nil && existing.Same(p.Queue) {
		p.Stored = existing
		return nil
	}
	configs, err := json.Marshal(p.Queue.ScoreConfigs)
	if err != nil {
		return fmt.Errorf("encode the score configs of queue %s: %w", p.Queue.Name, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO annotation_queues (project_id, name, description, score_configs, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, name) DO UPDATE SET
		   description   = excluded.description,
		   score_configs = excluded.score_configs,
		   updated_at    = excluded.updated_at`,
		p.ProjectID, p.Queue.Name, p.Queue.Description, string(configs), p.Now, p.Now,
	); err != nil {
		return fmt.Errorf("put queue %s: %w", p.Queue.Name, err)
	}
	p.Created = existing == nil
	p.Stored, err = queueByName(tx, p.ProjectID, p.Queue.Name)
	return err
}

// QueueDelete is DELETE /api/v1/queues/{name} (#8): the queue and its items,
// behind spec 005 #8's echo. The scores written while annotating stay — they
// are attached to the trace, not to the queue (#3).
type QueueDelete struct {
	ProjectID string
	Name      string
	Confirm   string

	// Items is how many went, filled by apply.
	Items int64
}

// weight is a whole window and more: the cascade takes every item the queue
// holds, however many adds filled it (spec 043 #35).
func (d *QueueDelete) weight() int { return commitsAlone }

func (d *QueueDelete) apply(tx *sql.Tx) error {
	d.Items = 0
	queue, err := queueByName(tx, d.ProjectID, d.Name)
	if err != nil {
		return err
	}
	if queue == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("queue %q not found", d.Name)}
	}
	if d.Confirm != d.Name {
		return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the queue's name, %q, for this to happen", d.Name)}
	}
	items := queue.Counts.Total()
	// The items go through schema 0014's cascade; the scores do not hang
	// off this table at all, which is the whole of #3 in one sentence.
	if _, err := tx.Exec(`DELETE FROM annotation_queues WHERE project_id = ? AND name = ?`,
		d.ProjectID, d.Name); err != nil {
		return fmt.Errorf("delete queue %s: %w", d.Name, err)
	}
	d.Items = items
	return nil
}

// QueueItemsAdd is POST /api/v1/queues/{name}/items (#4): one target or many,
// all or nothing. A target already in the queue comes back as the item it
// already is and counts as *existing*, whatever its status — which is what
// lets a filter be re-run and a script retried without doubling the queue.
type QueueItemsAdd struct {
	ProjectID string
	Queue     string
	Targets   []QueueTarget
	Now       int64

	// Items are the rows the targets resolve to, in input order; Added and
	// Existing count them. All filled by apply.
	Items    []*AnnotationItem
	Added    int
	Existing int
}

// weight is the targets the add names, up to 1,000 (spec 043 #35).
func (a *QueueItemsAdd) weight() int { return len(a.Targets) }

func (a *QueueItemsAdd) apply(tx *sql.Tx) error {
	a.Items, a.Added, a.Existing = nil, 0, 0
	if err := requireQueue(tx, a.ProjectID, a.Queue); err != nil {
		return err
	}
	seq, err := nextSeq(tx, a.ProjectID, a.Queue)
	if err != nil {
		return err
	}
	// A batch naming one target twice is one row: the second mention is
	// *existing* like any other repeat, and counting it as added would make
	// the answer disagree with the queue.
	inBatch := map[QueueTarget]*AnnotationItem{}
	for _, target := range a.Targets {
		if item := inBatch[target]; item != nil {
			a.Items = append(a.Items, item)
			a.Existing++
			continue
		}
		item, err := itemByTarget(tx, a.ProjectID, a.Queue, target)
		if err != nil {
			return err
		}
		if item == nil {
			if item, err = insertItem(tx, a.ProjectID, a.Queue, target, seq, a.Now); err != nil {
				return err
			}
			seq++
			a.Added++
		} else {
			a.Existing++
		}
		inBatch[target] = item
		a.Items = append(a.Items, item)
	}
	return nil
}

// QueueItemsFromTraces is POST /api/v1/queues/{name}/items/from-traces (#4):
// the newest `Limit` traces the listing's own filters match, added as trace
// items. The filter is the vocabulary the listing already has, so "queue what
// I am looking at" is one call with no second grammar.
//
// The selection happens inside the write transaction, one statement, using the
// listing's own query: two clients composing the same filter must enqueue the
// same traces, and re-running the SQL here would be a second answer to
// "which traces match".
type QueueItemsFromTraces struct {
	ProjectID string
	Queue     string
	Filter    TraceFilter
	// Limit is how many traces may be added, 1–1000.
	Limit int
	// Matched is how many traces the filter matched when the handler
	// counted them, before the job: what the add is weighed by when it is
	// under Limit. More may arrive before it runs; the weight is an estimate.
	Matched int
	Now     int64

	// Added, Existing and Capped are filled by apply. Capped says the
	// filter matched more than Limit, which is what tells the caller a
	// second call with `to=` set at the oldest added has more to take.
	Added    int
	Existing int
	Capped   bool
}

// weight is the items the add expects to write: the traces the filter matched,
// at most its Limit (spec 043 #35).
func (f *QueueItemsFromTraces) weight() int { return min(f.Limit, f.Matched) }

func (f *QueueItemsFromTraces) apply(tx *sql.Tx) error {
	f.Added, f.Existing, f.Capped = 0, 0, false
	if err := requireQueue(tx, f.ProjectID, f.Queue); err != nil {
		return err
	}
	// One row past the cap, which is how the listing learns there is
	// another page and how this learns there is another callful.
	filter := f.Filter
	filter.Limit = f.Limit + 1
	query, args := traceQuery(f.ProjectID, filter)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return fmt.Errorf("select the traces to queue: %w", err)
	}
	var ids []string
	for rows.Next() {
		row, err := scanTrace(rows)
		if err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, row.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) > f.Limit {
		f.Capped = true
		ids = ids[:f.Limit]
	}

	seq, err := nextSeq(tx, f.ProjectID, f.Queue)
	if err != nil {
		return err
	}
	for _, id := range ids {
		target := QueueTarget{TraceID: id}
		item, err := itemByTarget(tx, f.ProjectID, f.Queue, target)
		if err != nil {
			return err
		}
		if item != nil {
			f.Existing++
			continue
		}
		if _, err := insertItem(tx, f.ProjectID, f.Queue, target, seq, f.Now); err != nil {
			return err
		}
		seq++
		f.Added++
	}
	return nil
}

// QueueNext is GET /api/v1/queues/{name}/next (#5): the item to work on, and
// the claim that says somebody is working on it.
//
// Resume first: a reload must not hand the reviewer a different trace mid
// verdict. Otherwise the oldest pending item nobody holds — or holds any
// longer, because a claim that expires is what keeps an abandoned tab from
// blocking an item for ever.
type QueueNext struct {
	ProjectID string
	Queue     string
	Annotator string
	Now       int64

	// Item is what was handed out, nil when nothing is claimable, and
	// Pending counts the queue's pending items — which, when Item is nil,
	// is exactly the ones other people are holding. Both filled by apply.
	Item    *AnnotationItem
	Pending int64
}

func (n *QueueNext) apply(tx *sql.Tx) error {
	n.Item, n.Pending = nil, 0
	if err := requireQueue(tx, n.ProjectID, n.Queue); err != nil {
		return err
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM annotation_items
		  WHERE project_id = ? AND queue = ? AND status = ?`,
		n.ProjectID, n.Queue, ItemPending).Scan(&n.Pending); err != nil {
		return fmt.Errorf("count the pending items of queue %s: %w", n.Queue, err)
	}

	item, err := oneItem(tx,
		`SELECT `+annotationItemColumns+` FROM annotation_items
		  WHERE project_id = ? AND queue = ? AND status = ?
		    AND claimed_by = ? AND claimed_until > ?
		  ORDER BY seq LIMIT 1`,
		n.ProjectID, n.Queue, ItemPending, n.Annotator, n.Now)
	if err != nil {
		return err
	}
	if item == nil {
		item, err = oneItem(tx,
			`SELECT `+annotationItemColumns+` FROM annotation_items
			  WHERE project_id = ? AND queue = ? AND status = ?
			    AND (claimed_until IS NULL OR claimed_until <= ?)
			  ORDER BY seq LIMIT 1`,
			n.ProjectID, n.Queue, ItemPending, n.Now)
		if err != nil || item == nil {
			return err
		}
	}
	until := n.Now + int64(ClaimTTL)
	if _, err := tx.Exec(
		`UPDATE annotation_items SET claimed_by = ?, claimed_until = ?
		  WHERE project_id = ? AND id = ?`,
		n.Annotator, until, n.ProjectID, item.ID); err != nil {
		return fmt.Errorf("claim item %s: %w", item.ID, err)
	}
	item.ClaimedBy, item.ClaimedUntil = n.Annotator, until
	n.Item = item
	return nil
}

// QueueItemComplete is POST …/items/{id}/complete (#7). It succeeds only when
// every config the queue names has a score on the item's target, whoever wrote
// it: the queue promised a shape, and *completed* has to mean the shape was
// filled — checked where the scores are, by the server, so that the CLI and
// the desk agree.
//
// "Whoever wrote it" because a judge's verdict already on the trace is a
// verdict; the reviewer sees it prefilled and confirms or edits it. A second
// completion is refused rather than absorbed, so a race between two reviewers
// is visible to the one who lost it.
type QueueItemComplete struct {
	ProjectID string
	Queue     string
	ID        string
	Annotator string
	Now       int64

	// Item is the row after the write, filled by apply.
	Item *AnnotationItem
}

func (c *QueueItemComplete) apply(tx *sql.Tx) error {
	c.Item = nil
	item, queue, err := itemForWrite(tx, c.ProjectID, c.Queue, c.ID)
	if err != nil {
		return err
	}
	if item.Status == ItemCompleted {
		return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
			"item %s was already completed by %s", item.ID, orSomebody(item.CompletedBy))}
	}
	missing, err := missingScores(tx, c.ProjectID, queue.ScoreConfigs, item)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return &Rejection{
			Kind: RejectConflict,
			Message: fmt.Sprintf("item %s is missing a score for %s",
				item.ID, strings.Join(missing, ", ")),
			// The caller has to act on this one: the desk marks those
			// controls, and a script knows what to post before it
			// tries again (spec 021 #14's shape).
			Details: map[string]any{"missing": missing},
		}
	}
	if _, err := tx.Exec(
		`UPDATE annotation_items
		    SET status = ?, completed_by = ?, completed_at = ?, skip_reason = NULL,
		        claimed_by = NULL, claimed_until = NULL
		  WHERE project_id = ? AND id = ?`,
		ItemCompleted, c.Annotator, c.Now, c.ProjectID, item.ID); err != nil {
		return fmt.Errorf("complete item %s: %w", item.ID, err)
	}
	c.Item, err = itemByID(tx, c.ProjectID, c.Queue, c.ID)
	return err
}

// QueueItemSkip is POST …/items/{id}/skip (#7): not every trace deserves a
// verdict, and the reason is what makes that readable afterwards.
//
// A *completed* item is refused, exactly as a second completion is (Decision
// 18): the columns a skip writes are the ones the completion filled, so a
// stale tab pressing *Skip* would overwrite who gave the verdict and when, and
// drop the item out of the completed count while its scores sat on the trace.
// Reopen is the documented door for undoing a verdict.
type QueueItemSkip struct {
	ProjectID string
	Queue     string
	ID        string
	Annotator string
	Reason    string
	Now       int64

	Item *AnnotationItem
}

func (s *QueueItemSkip) apply(tx *sql.Tx) error {
	s.Item = nil
	item, _, err := itemForWrite(tx, s.ProjectID, s.Queue, s.ID)
	if err != nil {
		return err
	}
	if item.Status == ItemCompleted {
		return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
			"item %s was completed by %s; reopen it before skipping it",
			item.ID, orSomebody(item.CompletedBy))}
	}
	// The skipper is written to `completed_by`: the column says who
	// finished with the item, and a skip is one of the two ways to.
	if _, err := tx.Exec(
		`UPDATE annotation_items
		    SET status = ?, completed_by = ?, completed_at = ?, skip_reason = ?,
		        claimed_by = NULL, claimed_until = NULL
		  WHERE project_id = ? AND id = ?`,
		ItemSkipped, s.Annotator, s.Now, nullString(s.Reason), s.ProjectID, item.ID); err != nil {
		return fmt.Errorf("skip item %s: %w", item.ID, err)
	}
	s.Item, err = itemByID(tx, s.ProjectID, s.Queue, s.ID)
	return err
}

// QueueItemReopen is POST …/items/{id}/reopen (#7): a completed or skipped
// item back to pending, for the manager who disagrees with a verdict or wants
// a skip looked at again.
//
// An item that is *already* pending is not refused: what reopen does to it is
// release the claim, which is the one thing #5 says every finishing write
// does and the only door the desk's *Later* has (Decision 16). Idempotent
// either way — the end state is "pending, unclaimed", and asking for it twice
// is asking for the same thing.
type QueueItemReopen struct {
	ProjectID string
	Queue     string
	ID        string
	Annotator string

	Item *AnnotationItem
}

func (r *QueueItemReopen) apply(tx *sql.Tx) error {
	r.Item = nil
	item, _, err := itemForWrite(tx, r.ProjectID, r.Queue, r.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE annotation_items
		    SET status = ?, completed_by = NULL, completed_at = NULL, skip_reason = NULL,
		        claimed_by = NULL, claimed_until = NULL
		  WHERE project_id = ? AND id = ?`,
		ItemPending, r.ProjectID, item.ID); err != nil {
		return fmt.Errorf("reopen item %s: %w", item.ID, err)
	}
	r.Item, err = itemByID(tx, r.ProjectID, r.Queue, r.ID)
	return err
}

// QueueItemDelete is DELETE …/items/{id} (#8): one row, with none of the
// ceremony the queue's own deletion wears — a re-add recreates it, and the
// scores it was about were never here.
type QueueItemDelete struct {
	ProjectID string
	Queue     string
	ID        string
}

func (d *QueueItemDelete) apply(tx *sql.Tx) error {
	result, err := tx.Exec(
		`DELETE FROM annotation_items WHERE project_id = ? AND queue = ? AND id = ?`,
		d.ProjectID, d.Queue, d.ID)
	if err != nil {
		return fmt.Errorf("delete item %s: %w", d.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return &Rejection{Kind: RejectNotFound,
			Message: fmt.Sprintf("queue %q has no item %s", d.Queue, d.ID)}
	}
	return nil
}

// --- reads ---------------------------------------------------------------

// QueueItemFilter narrows an item listing (#8). The zero value lists the
// queue's items oldest first, which is the order they are worked in.
type QueueItemFilter struct {
	Status    string
	Annotator string
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After continues a previous page: the `seq` of the last row of it.
	After *int64
	// Backward pages towards the *older* end. Rows still come back oldest
	// first either way (spec 016 #19's ascending walk).
	Backward bool
}

// Queues lists a project's queues by name, whole: a project has as many
// queues as it has review programmes, which is a handful (spec 003's
// reasoning for configs).
func (s *Store) Queues(ctx context.Context, projectID string) ([]*AnnotationQueue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+queueColumns+` FROM annotation_queues q
		  WHERE q.project_id = ? ORDER BY q.name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list queues: %w", err)
	}
	defer rows.Close()

	out := []*AnnotationQueue{}
	for rows.Next() {
		queue, err := scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, queue)
	}
	return out, rows.Err()
}

// Queue returns one queue with its counts, or nil when the name has none.
func (s *Store) Queue(ctx context.Context, projectID, name string) (*AnnotationQueue, error) {
	queue, err := scanQueue(s.db.QueryRowContext(ctx,
		`SELECT `+queueColumns+` FROM annotation_queues q
		  WHERE q.project_id = ? AND q.name = ?`, projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return queue, err
}

// QueueItems lists one queue's items in `seq` order, oldest first.
func (s *Store) QueueItems(ctx context.Context, projectID, name string, filter QueueItemFilter) ([]*AnnotationItem, error) {
	query, args := itemQuery(projectID, name, filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the items of queue %s: %w", name, err)
	}
	defer rows.Close()

	var out []*AnnotationItem
	for rows.Next() {
		item, err := scanAnnotationItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A backward page arrives newest first, because that is the order the
	// index was read in; every caller reads this listing oldest first.
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// CountQueueItems answers "how many match", stopping at cap (spec 009 #4).
func (s *Store) CountQueueItems(ctx context.Context, projectID, name string, filter QueueItemFilter, cap int) (int, error) {
	where, args := itemConditions(projectID, name, filter)
	args = append(args, cap)
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM annotation_items WHERE `+
		strings.Join(where, " AND ")+` LIMIT ?)`, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count the items of queue %s: %w", name, err)
	}
	return count, nil
}

// QueueItem returns one item of one queue, or nil when it is not there.
func (s *Store) QueueItem(ctx context.Context, projectID, queue, id string) (*AnnotationItem, error) {
	item, err := scanAnnotationItem(s.db.QueryRowContext(ctx,
		`SELECT `+annotationItemColumns+` FROM annotation_items
		  WHERE project_id = ? AND queue = ? AND id = ?`, projectID, queue, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

// --- the shared parts ----------------------------------------------------

// queueColumns is the SELECT list every queue read shares. The three counts
// are correlated subqueries over `idx_annotation_items_next`, which starts
// with the queue and the status: a project has a handful of queues, and each
// count is a seek to one prefix rather than a scan of the table.
const queueColumns = `q.name, q.description, q.score_configs, q.created_at, q.updated_at,
	(` + statusCount + `'pending'),
	(` + statusCount + `'completed'),
	(` + statusCount + `'skipped')`

const statusCount = `SELECT COUNT(*) FROM annotation_items i
	 WHERE i.project_id = q.project_id AND i.queue = q.name AND i.status = `

const annotationItemColumns = `id, queue, trace_id, observation_id, status, seq, added_at,
	        claimed_by, claimed_until, completed_by, completed_at, skip_reason`

func scanQueue(row scanner) (*AnnotationQueue, error) {
	var (
		queue   AnnotationQueue
		configs string
	)
	if err := row.Scan(&queue.Name, &queue.Description, &configs,
		&queue.CreatedAt, &queue.UpdatedAt,
		&queue.Counts.Pending, &queue.Counts.Completed, &queue.Counts.Skipped); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan queue: %w", err)
	}
	if err := json.Unmarshal([]byte(configs), &queue.ScoreConfigs); err != nil {
		return nil, fmt.Errorf("decode the score configs of queue %s: %w", queue.Name, err)
	}
	if queue.ScoreConfigs == nil {
		queue.ScoreConfigs = []string{}
	}
	return &queue, nil
}

func scanAnnotationItem(row scanner) (*AnnotationItem, error) {
	var (
		item          AnnotationItem
		observationID sql.NullString
		claimedBy     sql.NullString
		claimedUntil  sql.NullInt64
		completedBy   sql.NullString
		completedAt   sql.NullInt64
		skipReason    sql.NullString
	)
	if err := row.Scan(&item.ID, &item.Queue, &item.TraceID, &observationID, &item.Status,
		&item.Seq, &item.AddedAt, &claimedBy, &claimedUntil,
		&completedBy, &completedAt, &skipReason); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan annotation item: %w", err)
	}
	item.ObservationID, item.ClaimedBy = observationID.String, claimedBy.String
	item.ClaimedUntil, item.CompletedBy = claimedUntil.Int64, completedBy.String
	item.CompletedAt, item.SkipReason = completedAt.Int64, skipReason.String
	return &item, nil
}

// itemConditions is everything a filter says about which items match, cursor
// excluded — the split spec 009 #4 made between a listing and its count.
func itemConditions(projectID, queue string, filter QueueItemFilter) ([]string, []any) {
	where := []string{"project_id = ?", "queue = ?"}
	args := []any{projectID, queue}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Annotator != "" {
		// Who has the item: whoever finished with it, and — for one still
		// pending — whoever is holding it (Decision 20). `completed_by`
		// alone made `status=pending&annotator=ada` answer "ada has
		// nothing open" for a queue ada is working through, which is a
		// well-formed answer to a different question (spec 003 #23).
		where = append(where,
			"(completed_by = ? OR (status = ? AND claimed_by = ?))")
		args = append(args, filter.Annotator, ItemPending, filter.Annotator)
	}
	return where, args
}

// itemQuery builds the listing statement, as a function so that a test can
// hand the shipped SQL to EXPLAIN QUERY PLAN and assert the index seek
// (spec 023's rule: the store never runs ANALYZE, so a new query owes a plan
// test).
func itemQuery(projectID, queue string, filter QueueItemFilter) (string, []any) {
	where, args := itemConditions(projectID, queue, filter)
	comparison, order := ">", "ASC"
	if filter.Backward {
		comparison, order = "<", "DESC"
	}
	if filter.After != nil {
		where = append(where, "seq "+comparison+" ?")
		args = append(args, *filter.After)
	}
	args = append(args, filter.Limit)
	return `SELECT ` + annotationItemColumns + ` FROM annotation_items
	 WHERE ` + strings.Join(where, " AND ") + `
	 ORDER BY seq ` + order + ` LIMIT ?`, args
}

func queueByName(tx *sql.Tx, projectID, name string) (*AnnotationQueue, error) {
	queue, err := scanQueue(tx.QueryRow(
		`SELECT `+queueColumns+` FROM annotation_queues q
		  WHERE q.project_id = ? AND q.name = ?`, projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return queue, err
}

// requireQueue refuses a write to a queue that is not there. The foreign key
// would refuse it too, with a message about a constraint; this one names the
// queue.
func requireQueue(tx *sql.Tx, projectID, name string) error {
	queue, err := queueByName(tx, projectID, name)
	if err != nil {
		return err
	}
	if queue == nil {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("queue %q not found", name)}
	}
	return nil
}

// itemForWrite loads the item and its queue, refusing both absences by name.
func itemForWrite(tx *sql.Tx, projectID, queue, id string) (*AnnotationItem, *AnnotationQueue, error) {
	stored, err := queueByName(tx, projectID, queue)
	if err != nil {
		return nil, nil, err
	}
	if stored == nil {
		return nil, nil, &Rejection{Kind: RejectNotFound,
			Message: fmt.Sprintf("queue %q not found", queue)}
	}
	item, err := itemByID(tx, projectID, queue, id)
	if err != nil {
		return nil, nil, err
	}
	if item == nil {
		return nil, nil, &Rejection{Kind: RejectNotFound,
			Message: fmt.Sprintf("queue %q has no item %s", queue, id)}
	}
	return item, stored, nil
}

func itemByID(tx *sql.Tx, projectID, queue, id string) (*AnnotationItem, error) {
	return oneItem(tx,
		`SELECT `+annotationItemColumns+` FROM annotation_items
		  WHERE project_id = ? AND queue = ? AND id = ?`, projectID, queue, id)
}

// itemByTarget is the dedupe of #2. The observation half is written as two
// statements rather than one with an `IS` comparison because that is what
// SQLite can answer from the UNIQUE index either way, and because NULL is not
// a value the index compares: a trace item is found by `observation_id IS
// NULL`, which the unique constraint would not have caught on insert.
func itemByTarget(tx *sql.Tx, projectID, queue string, target QueueTarget) (*AnnotationItem, error) {
	if target.ObservationID == "" {
		return oneItem(tx,
			`SELECT `+annotationItemColumns+` FROM annotation_items
			  WHERE project_id = ? AND queue = ? AND trace_id = ? AND observation_id IS NULL`,
			projectID, queue, target.TraceID)
	}
	return oneItem(tx,
		`SELECT `+annotationItemColumns+` FROM annotation_items
		  WHERE project_id = ? AND queue = ? AND trace_id = ? AND observation_id = ?`,
		projectID, queue, target.TraceID, target.ObservationID)
}

func oneItem(tx *sql.Tx, query string, args ...any) (*AnnotationItem, error) {
	item, err := scanAnnotationItem(tx.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return item, err
}

// nextSeq is the queue's clock: MAX(seq) + 1, read inside the write
// transaction, which is what makes it gapless without a lock of its own (the
// rule spec 003 #5 gave prompt versions).
func nextSeq(tx *sql.Tx, projectID, queue string) (int64, error) {
	var seq sql.NullInt64
	if err := tx.QueryRow(nextSeqQuery, projectID, queue).Scan(&seq); err != nil {
		return 0, fmt.Errorf("read the sequence of queue %s: %w", queue, err)
	}
	return seq.Int64 + 1, nil
}

// nextSeqQuery is named so that a test can hand the shipped SQL to EXPLAIN
// QUERY PLAN: it rides `idx_annotation_items_seq` and nothing else, and on the
// index the spec's data contract listed it was a scan of the whole queue on
// every add (Decision 19, found in review of PR #43).
const nextSeqQuery = `SELECT MAX(seq) FROM annotation_items WHERE project_id = ? AND queue = ?`

func insertItem(tx *sql.Tx, projectID, queue string, target QueueTarget,
	seq, now int64) (*AnnotationItem, error) {

	id, err := NewID()
	if err != nil {
		return nil, fmt.Errorf("mint an item id: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO annotation_items
		   (project_id, queue, id, trace_id, observation_id, status, seq, added_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, queue, id, target.TraceID, nullString(target.ObservationID),
		ItemPending, seq, now); err != nil {
		return nil, fmt.Errorf("add item to queue %s: %w", queue, err)
	}
	return &AnnotationItem{
		ID: id, Queue: queue, TraceID: target.TraceID, ObservationID: target.ObservationID,
		Status: ItemPending, Seq: seq, AddedAt: now,
	}, nil
}

// missingScores is Decision 7's check: the config names the queue asks for
// that have no score on this item's target. A trace item looks for scores that
// name no observation, and an observation item for scores that name *its*
// observation — the two are different verdicts about different things, and
// letting either stand in for the other would make *completed* mean nothing.
func missingScores(tx *sql.Tx, projectID string, configs []string, item *AnnotationItem) ([]string, error) {
	query := `SELECT 1 FROM scores
	           WHERE project_id = ? AND trace_id = ? AND name = ? AND observation_id IS NULL LIMIT 1`
	args := func(name string) []any { return []any{projectID, item.TraceID, name} }
	if item.ObservationID != "" {
		query = `SELECT 1 FROM scores
		          WHERE project_id = ? AND trace_id = ? AND name = ? AND observation_id = ? LIMIT 1`
		args = func(name string) []any {
			return []any{projectID, item.TraceID, name, item.ObservationID}
		}
	}
	var missing []string
	for _, name := range configs {
		var found int
		err := tx.QueryRow(query, args(name)...).Scan(&found)
		if err == sql.ErrNoRows {
			missing = append(missing, name)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("look for a %s score on trace %s: %w", name, item.TraceID, err)
		}
	}
	return missing, nil
}

func orSomebody(annotator string) string {
	if annotator == "" {
		return "somebody"
	}
	return annotator
}
