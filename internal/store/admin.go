package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Administration (spec 005): projects, keys, retention windows and user-data
// erasure. Every change here is a WriteJob like every other durable write
// (spec 003 #9), and every destructive one re-reads what it is about to
// destroy inside its own transaction.
//
// That last part is the whole safety model. A preview is a read, and between
// a read and a commit a project can be renamed, restored or emptied; a
// confirmation checked against what the preview saw would be a confirmation of
// something that is no longer true (spec 003 Decision 20, spec 005 #8).

// DeleteCounts is what a destructive operation would remove, or did. Zero
// fields are reported as zero rather than omitted: "nothing of this kind" is
// an answer a caller acts on.
type DeleteCounts struct {
	Traces       int64
	Observations int64
	Scores       int64
	Payloads     int64
	RawBatches   int64
	// StatsHours is how many rolled hours a stats window would delete
	// (spec 013 #6). It is counted separately because it is the one thing
	// here that survives the trace sweep.
	StatsHours   int64
	Prompts      int64
	PromptLabels int64
	APIKeys      int64
	// AnnotationQueues and AnnotationItems are spec 024's two stores.
	// Counted because a destruction preview is what says how big the hole
	// will be before it opens (spec 005 #8): the erasure takes the items
	// pointing at the erased traces, and deleting a project takes both.
	AnnotationQueues int64
	AnnotationItems  int64
	// Media and MediaBytes are the bodies the project would stop holding —
	// those none of its traces or raw batches staying behind points at —
	// and their decoded bytes (spec 041 #11, #27): a preview that hid a
	// hundred megabytes of pictures would be a preview that lies by
	// omission. Whether another project keeps the bytes does not enter it.
	Media      int64
	MediaBytes int64
	// SessionScores are the scores that name a session and no trace: an
	// erasure takes those of the erased traces' sessions (spec 044 #7), the
	// sweep those whose session no trace carries any more (#8).
	SessionScores int64
	// DatasetItems are the items an erasure takes because a row of theirs
	// was cut from an erased trace (spec 044 #9) — items, not rows.
	DatasetItems int64
	// RawSpans are the erased spans taken out of the raw batches, and
	// RawBatchesRewritten and RawBatchesDeleted the batches that held them:
	// rewritten without them, or deleted — left empty, or a rewrite that
	// failed (spec 044 #2).
	RawSpans            int64
	RawBatchesRewritten int64
	RawBatchesDeleted   int64
	// Oldest is the arrival time of the oldest affected row (Unix
	// nanoseconds), or zero when nothing is affected.
	Oldest int64
}

// add sums what two chunks of one request deleted. Oldest is a preview's
// and is not summed.
func (c *DeleteCounts) add(o DeleteCounts) {
	c.Traces += o.Traces
	c.Observations += o.Observations
	c.Scores += o.Scores
	c.Payloads += o.Payloads
	c.AnnotationItems += o.AnnotationItems
	c.Media += o.Media
	c.MediaBytes += o.MediaBytes
	c.SessionScores += o.SessionScores
	c.DatasetItems += o.DatasetItems
	c.RawSpans += o.RawSpans
	c.RawBatchesRewritten += o.RawBatchesRewritten
	c.RawBatchesDeleted += o.RawBatchesDeleted
}

// Any reports whether the operation would remove anything at all.
func (c DeleteCounts) Any() bool {
	return c.Traces+c.Observations+c.Scores+c.Payloads+c.RawBatches+
		c.Prompts+c.PromptLabels+c.APIKeys+
		c.AnnotationQueues+c.AnnotationItems+c.Media+
		c.SessionScores+c.DatasetItems+c.RawSpans > 0
}

// OptionalDays is a retention window as a PATCH carries it. Absent, cleared
// and set are three different intentions: a body that says nothing about a
// window leaves it alone, and one that says `null` means keep forever
// (spec 005 #2).
type OptionalDays struct {
	Set   bool
	Value *int
}

// MaxRetentionDays bounds a retention window, at a little over a century.
// The real ceiling is arithmetic: a window is turned into a nanosecond cutoff,
// and past ~106751 days that multiplication overflows int64 and wraps the
// cutoff into the *future*, where it matches every row there is. A limit two
// orders of magnitude below the wrap is a limit nobody meets by accident and
// nobody reaches by mistake.
const MaxRetentionDays = 36500

// cutoffFor turns a window into the arrival time before which rows expire, and
// reports whether there is a window at all.
//
// A stored window beyond the ceiling is read as "keep forever" rather than
// clamped: it can only come from a hand-edited database, "essentially forever"
// is what such a number means, and of the two ways to be wrong about a
// deletion, not deleting is the recoverable one. Both the sweeper and the dry
// run go through here, so a preview cannot promise something the sweep would
// do differently.
func cutoffFor(days *int, now int64) (int64, bool) {
	if days == nil || *days > MaxRetentionDays {
		return 0, false
	}
	return now - int64(*days)*int64(24*time.Hour), true
}

// RetentionPreview counts what the given windows would delete from a project
// right now. It is the same arithmetic the sweeper does, which is what lets
// the dry run promise something the next pass will keep (spec 005 #8).
func (s *Store) RetentionPreview(ctx context.Context, projectID string, retention, raw, stats *int, now int64) (DeleteCounts, error) {
	var counts DeleteCounts
	if cutoff, windowed := cutoffFor(retention, now); windowed {
		expiring, err := s.expiredCounts(ctx, projectID, cutoff)
		if err != nil {
			return counts, err
		}
		counts = expiring
		// The session-only scores the same pass would take (spec 044 #8):
		// a session none of whose traces outlives this window loses its
		// verdicts with them.
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM scores s
			  WHERE s.project_id = ? AND s.trace_id IS NULL AND s.created_at < ?
			    AND NOT EXISTS (SELECT 1 FROM traces t WHERE t.project_id = s.project_id
			                     AND t.session_id = s.session_id
			                     AND NOT (t.ingested_at < ? AND `+notPinned+`))`,
			projectID, cutoff, cutoff).Scan(&counts.SessionScores); err != nil {
			return counts, fmt.Errorf("count expiring session scores: %w", err)
		}
	}
	// Raw follows the trace window unless it has one of its own (#6).
	rawWindow := raw
	if rawWindow == nil {
		rawWindow = retention
	}
	if cutoff, windowed := cutoffFor(rawWindow, now); windowed {
		batches, oldest, err := s.expiredRawCounts(ctx, projectID, cutoff)
		if err != nil {
			return counts, err
		}
		counts.RawBatches = batches
		counts.Oldest = earliest(counts.Oldest, oldest)
	}
	// The bodies both windows together would make the project stop
	// holding (spec 041 #11, #27): a body a trace keeps is not released by
	// its raw batch going, nor the other way round, so the two are asked
	// about as one deletion.
	var traces, raws string
	var traceArgs, rawArgs []any
	if cutoff, windowed := cutoffFor(retention, now); windowed {
		traces = `SELECT id FROM traces t WHERE t.project_id = ? AND t.ingested_at < ? AND ` + notPinned
		traceArgs = []any{projectID, cutoff}
	}
	if cutoff, windowed := cutoffFor(rawWindow, now); windowed {
		raws = `SELECT id FROM raw_batches WHERE project_id = ? AND received_at < ?`
		rawArgs = []any{projectID, cutoff}
	}
	if traces != "" || raws != "" {
		var err error
		if counts.Media, counts.MediaBytes, err = s.mediaFreed(ctx, projectID, traces, traceArgs, raws, rawArgs); err != nil {
			return counts, err
		}
	}
	// The rollup's own window, which is measured against the hour a row
	// summarizes rather than against arrival: the rows carry no arrival
	// time, and the question an operator asks of them is "how far back does
	// my history reach" (spec 013 #6).
	if stats != nil {
		if cutoff, windowed := cutoffFor(stats, now); windowed {
			hours, err := s.expiredStatsHours(ctx, projectID, cutoff/1e9)
			if err != nil {
				return counts, err
			}
			counts.StatsHours = hours
		}
	}
	return counts, nil
}

// expiredStatsHours counts the rolled hours a stats window would delete.
func (s *Store) expiredStatsHours(ctx context.Context, projectID string, cutoffSeconds int64) (int64, error) {
	var hours int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT hour) FROM stats_hourly WHERE project_id = ? AND hour < ?`,
		projectID, cutoffSeconds).Scan(&hours); err != nil {
		return 0, fmt.Errorf("count expiring rolled hours: %w", err)
	}
	return hours, nil
}

// expiredCounts counts what the trace window would take: the same predicate
// the sweep uses, pinned traces excluded (spec 014 #13).
func (s *Store) expiredCounts(ctx context.Context, projectID string, cutoff int64) (DeleteCounts, error) {
	var (
		counts DeleteCounts
		oldest sql.NullInt64
	)
	const expiring = `SELECT id FROM traces t WHERE t.project_id = ? AND t.ingested_at < ? AND ` + notPinned
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(t.ingested_at) FROM traces t WHERE t.project_id = ? AND t.ingested_at < ? AND `+notPinned,
		projectID, cutoff).Scan(&counts.Traces, &oldest)
	if err != nil {
		return counts, fmt.Errorf("count expiring traces: %w", err)
	}
	if oldest.Valid {
		counts.Oldest = oldest.Int64
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM observations WHERE project_id = ? AND trace_id IN (`+expiring+`)`,
		projectID, projectID, cutoff).Scan(&counts.Observations); err != nil {
		return counts, fmt.Errorf("count expiring observations: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scores WHERE project_id = ? AND trace_id IN (`+expiring+`)`,
		projectID, projectID, cutoff).Scan(&counts.Scores); err != nil {
		return counts, fmt.Errorf("count expiring scores: %w", err)
	}
	return counts, nil
}

func (s *Store) expiredRawCounts(ctx context.Context, projectID string, cutoff int64) (int64, int64, error) {
	var (
		batches int64
		oldest  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(received_at) FROM raw_batches WHERE project_id = ? AND received_at < ?`,
		projectID, cutoff).Scan(&batches, &oldest)
	if err != nil {
		return 0, 0, fmt.Errorf("count expiring raw batches: %w", err)
	}
	return batches, oldest.Int64, nil
}

// AffectedRun is a live run some of whose traces an erasure would take
// (spec 014 #14): the run then shows those items as missing, and the dry run
// names it so the operator sees the hole before it opens.
type AffectedRun struct {
	ID      string
	Dataset string
	Traces  int64
}

// ProjectPreview counts everything a project holds: what deleting it will
// eventually destroy, which is what the operator is being asked to confirm.
func (s *Store) ProjectPreview(ctx context.Context, projectID string) (DeleteCounts, error) {
	var (
		counts DeleteCounts
		oldest sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(ingested_at) FROM traces WHERE project_id = ?`, projectID).
		Scan(&counts.Traces, &oldest)
	if err != nil {
		return counts, fmt.Errorf("count a project's traces: %w", err)
	}
	if oldest.Valid {
		counts.Oldest = oldest.Int64
	}
	for _, table := range []struct {
		name  string
		count *int64
	}{
		{"observations", &counts.Observations},
		{"scores", &counts.Scores},
		{"raw_batches", &counts.RawBatches},
		{"prompts", &counts.Prompts},
		{"prompt_labels", &counts.PromptLabels},
		{"api_keys", &counts.APIKeys},
		{"annotation_queues", &counts.AnnotationQueues},
		{"annotation_items", &counts.AnnotationItems},
	} {
		// The table names are this package's own constants; only the
		// project id is bound.
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table.name+` WHERE project_id = ?`, projectID).
			Scan(table.count); err != nil {
			return counts, fmt.Errorf("count a project's %s: %w", table.name, err)
		}
	}
	// Every body the project holds, a ref the Langfuse channel wrote for a
	// trace that never came included: the project stops holding all of them
	// (spec 041 #11, #27), which is its media figure.
	summary, err := s.MediaSummary(ctx, projectID)
	counts.Media, counts.MediaBytes = summary.Count, summary.Bytes
	return counts, err
}

func earliest(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	default:
		return min(a, b)
	}
}

// ProjectCreate mints a project and its first key pair (spec 005, API
// contract). The name check is inside the transaction, because the answer to
// "is this name free" changes and a UNIQUE violation would reach the caller as
// a 500 rather than as the 409 that names the restorable project.
type ProjectCreate struct {
	Name string
	Keys KeyPair
	// Origin is who is creating the project, which is who minted its first
	// key (spec 045 #8).
	Origin KeyOrigin

	Project *Project
}

func (p *ProjectCreate) apply(tx *sql.Tx) error {
	existing, err := projectByName(tx, p.Name)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Deleted() {
			return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
				"project %q is deleted and can be restored until %s; its name stays reserved until then",
				p.Name, time.Unix(0, existing.PurgeAt()).UTC().Format(time.RFC3339))}
		}
		return &Rejection{Kind: RejectConflict,
			Message: fmt.Sprintf("project %q already exists", p.Name)}
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	project, err := insertProject(tx, id, p.Name, p.Keys, p.Origin)
	if err != nil {
		return err
	}
	p.Project = project
	return nil
}

// insertProject writes a project row and its first key, which may do
// everything a key may (spec 045 #5). Shared with the startup bootstrap, which
// runs before the writer exists and so cannot be a job (spec 001 #9).
func insertProject(tx *sql.Tx, id, name string, keys KeyPair, origin KeyOrigin) (*Project, error) {
	// RETURNING keeps the returned Project in sync with schema defaults
	// instead of duplicating them as Go literals.
	project, err := scanProject(tx.QueryRow(
		`INSERT INTO projects (id, name) VALUES (?, ?) RETURNING `+projectColumns, id, name))
	if err != nil {
		return nil, fmt.Errorf("create project %q: %w", name, err)
	}
	if err := insertKey(tx, id, keys, "", AllScopes, origin); err != nil {
		return nil, err
	}
	return project, nil
}

// KeyCreate adds a key pair to a project. Several active pairs are the point:
// create the new one, move the SDKs, revoke the old, and ingest never 401s in
// between (spec 005 #12).
type KeyCreate struct {
	ProjectID string
	Keys      KeyPair
	// Name says which program holds the key (spec 045 #6), Scopes what it
	// may do, spelled canonically (CanonicalScopes), and Origin who minted
	// it (#8).
	Name   string
	Scopes string
	Origin KeyOrigin

	CreatedAt string
}

func (k *KeyCreate) apply(tx *sql.Tx) error {
	project, err := projectByID(tx, k.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	if err := insertKey(tx, k.ProjectID, k.Keys, k.Name, k.Scopes, k.Origin); err != nil {
		return err
	}
	return tx.QueryRow(`SELECT created_at FROM api_keys WHERE public_key = ?`, k.Keys.PublicKey).
		Scan(&k.CreatedAt)
}

// KeyRevoke removes one key pair. Revoking the last one leaves a project that
// cannot ingest, so it is allowed — a declaratively provisioned deployment
// re-adds its keys from the environment — but never by accident (#12). The
// same holds for the last key that carries `ingest`, whatever keys are left
// beside it (spec 045 #11).
type KeyRevoke struct {
	ProjectID string
	PublicKey string
	Confirm   string

	Revoked bool
	Last    bool
}

func (k *KeyRevoke) apply(tx *sql.Tx) error {
	k.Revoked, k.Last = false, false

	keys, err := projectKeyScopes(context.Background(), tx, k.ProjectID)
	if err != nil {
		return err
	}
	if _, ok := keys[k.PublicKey]; !ok {
		return &Rejection{Kind: RejectNotFound,
			Message: fmt.Sprintf("this project has no key %q", k.PublicKey)}
	}

	k.Last = LastOfItsKind(keys, k.PublicKey)
	if k.Last {
		if _, err := confirmProjectName(tx, k.ProjectID, k.Confirm); err != nil {
			return err
		}
	}
	result, err := tx.Exec(`DELETE FROM api_keys WHERE public_key = ? AND project_id = ?`,
		k.PublicKey, k.ProjectID)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	rows, err := result.RowsAffected()
	k.Revoked = rows > 0
	return err
}

// LastOfItsKind reports whether revoking a key asks for the echo: it is the
// project's last key (spec 005 #12), or the last that carries `ingest` (spec
// 045 #11). `keys` is every key of the project with its scopes, the revoked
// one included — what ProjectKeyScopes answers, and what the revocation reads
// again inside its write, so that the preview and the write ask one rule.
func LastOfItsKind(keys map[string][]string, publicKey string) bool {
	if len(keys) == 1 {
		return true
	}
	if !slices.Contains(keys[publicKey], ScopeIngest) {
		return false
	}
	for other, scopes := range keys {
		if other != publicKey && slices.Contains(scopes, ScopeIngest) {
			return false
		}
	}
	return true
}

// ProjectUpdate renames a project or moves its retention windows. A window
// that shrinks is destructive and needs the echo (spec 005 #8); a rename is
// not, and the rename is applied after the echo has been checked against the
// name it is replacing.
type ProjectUpdate struct {
	ProjectID string
	Name      *string
	Retention OptionalDays
	RawWindow OptionalDays
	// StatsWindow is the rollup's own window (spec 013 #6). It shrinks
	// like the other two, and shrinking it destroys history that the trace
	// sweep deliberately spares, so it is confirmed like the other two.
	StatsWindow OptionalDays
	// Media is the project's media setting (spec 041 #6), nil to leave it.
	// Not destructive either way: `placeholder` stops keeping new bodies
	// and deletes none, and `store` keeps them again from the next export.
	Media *string
	// Confirm is the echo, required when either window shrinks. Whether it
	// is required is decided here rather than by the caller: the caller
	// decided it against a project row it read a moment earlier, and the
	// only reading that can gate a write is the one inside its transaction
	// (spec 003 Decision 20).
	Confirm string

	Project *Project
}

// Windows folds this update onto a project, giving the two windows it would
// leave behind. A field the update does not mention keeps its stored value.
func (u *ProjectUpdate) Windows(project *Project) (retention, raw, stats *int) {
	retention, raw, stats = project.RetentionDays, project.RawRetentionDays, project.StatsRetentionDays
	if u.Retention.Set {
		retention = u.Retention.Value
	}
	if u.RawWindow.Set {
		raw = u.RawWindow.Value
	}
	if u.StatsWindow.Set {
		stats = u.StatsWindow.Value
	}
	return retention, raw, stats
}

// Shrinks reports whether this update makes either window shorter, where "no
// window" is the longest window of all. Shorter means data that is kept today
// stops being kept, which is what needs confirming — including when nothing is
// old enough to be deleted yet, because what changed is the policy.
//
// The API asks this to decide whether to answer with a preview; apply asks it
// again, against the stored row, to decide whether to demand the echo.
func (u *ProjectUpdate) Shrinks(project *Project) bool {
	retention, raw, stats := u.Windows(project)
	if shorterWindow(retention, project.RetentionDays) {
		return true
	}
	// A shorter stats window destroys history the trace sweep spares by
	// design (spec 013 #6), which is the most destructive of the three: it
	// is the copy that was kept *because* the raw rows go.
	if shorterWindow(stats, project.StatsRetentionDays) {
		return true
	}
	// Raw follows the trace window when it has none of its own (#6), so
	// the comparison is between effective windows, not stored columns.
	return shorterWindow(
		effectiveWindow(raw, retention),
		effectiveWindow(project.RawRetentionDays, project.RetentionDays))
}

func effectiveWindow(own, fallback *int) *int {
	if own != nil {
		return own
	}
	return fallback
}

// shorterWindow reports whether `next` keeps data for less time than
// `current`. A nil window is infinite, so nothing is shorter than nil and nil
// is shorter than nothing.
func shorterWindow(next, current *int) bool {
	if next == nil {
		return false
	}
	return current == nil || *next < *current
}

func (u *ProjectUpdate) apply(tx *sql.Tx) error {
	project, err := projectByID(tx, u.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	if u.Shrinks(project) {
		if _, err := confirmProjectName(tx, u.ProjectID, u.Confirm); err != nil {
			return err
		}
	}
	if u.Name != nil && *u.Name != project.Name {
		taken, err := projectByName(tx, *u.Name)
		if err != nil {
			return err
		}
		if taken != nil {
			return &Rejection{Kind: RejectConflict,
				Message: fmt.Sprintf("project %q already exists", *u.Name)}
		}
		if _, err := tx.Exec(`UPDATE projects SET name = ? WHERE id = ?`, *u.Name, u.ProjectID); err != nil {
			return fmt.Errorf("rename project: %w", err)
		}
	}
	if u.Retention.Set {
		if _, err := tx.Exec(`UPDATE projects SET retention_days = ? WHERE id = ?`,
			nullDays(u.Retention.Value), u.ProjectID); err != nil {
			return fmt.Errorf("set retention window: %w", err)
		}
	}
	if u.RawWindow.Set {
		if _, err := tx.Exec(`UPDATE projects SET raw_retention_days = ? WHERE id = ?`,
			nullDays(u.RawWindow.Value), u.ProjectID); err != nil {
			return fmt.Errorf("set raw retention window: %w", err)
		}
	}
	if u.StatsWindow.Set {
		if _, err := tx.Exec(`UPDATE projects SET stats_retention_days = ? WHERE id = ?`,
			nullDays(u.StatsWindow.Value), u.ProjectID); err != nil {
			return fmt.Errorf("set stats retention window: %w", err)
		}
	}
	if u.Media != nil {
		if err := CheckMediaSetting(*u.Media); err != nil {
			return &Rejection{Kind: RejectInvalid, Message: err.Error()}
		}
		if _, err := tx.Exec(`UPDATE projects SET media = ? WHERE id = ?`, *u.Media, u.ProjectID); err != nil {
			return fmt.Errorf("set media setting: %w", err)
		}
	}
	u.Project, err = projectByID(tx, u.ProjectID)
	return err
}

func nullDays(days *int) any {
	if days == nil {
		return nil
	}
	return *days
}

// ProjectDelete is the soft delete of spec 005 #9: the keys stop working now,
// the data goes when the grace window runs out, and `restore` undoes it until
// then.
type ProjectDelete struct {
	ProjectID string
	Confirm   string
	Now       int64

	Project *Project
}

func (d *ProjectDelete) apply(tx *sql.Tx) error {
	project, err := confirmProjectName(tx, d.ProjectID, d.Confirm)
	if err != nil {
		return err
	}
	if project.Deleted() {
		return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
			"project %q is already deleted; it will be purged at %s",
			project.Name, time.Unix(0, project.PurgeAt()).UTC().Format(time.RFC3339))}
	}
	if _, err := tx.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`, d.Now, d.ProjectID); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	d.Project, err = projectByID(tx, d.ProjectID)
	return err
}

// ProjectRestore undoes a soft delete. It needs no confirmation: it destroys
// nothing, and the whole point of the grace window is that undoing is cheap.
type ProjectRestore struct {
	ProjectID string

	Project *Project
}

func (r *ProjectRestore) apply(tx *sql.Tx) error {
	project, err := projectByID(tx, r.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	if !project.Deleted() {
		return &Rejection{Kind: RejectInvalid,
			Message: fmt.Sprintf("project %q is not deleted", project.Name)}
	}
	if _, err := tx.Exec(`UPDATE projects SET deleted_at = NULL WHERE id = ?`, r.ProjectID); err != nil {
		return fmt.Errorf("restore project: %w", err)
	}
	r.Project, err = projectByID(tx, r.ProjectID)
	return err
}

// UserDataErase removes one chunk of a user's parsed data: the traces filed
// under the id, their observations, payloads and scores (spec 005 #7), the
// session-only scores of their sessions (spec 044 #7) and every version of the
// dataset items cut from them (#9). It is step 3 of an erasure; the raw
// batches are the steps around it (`EraseUserData`).
//
// One chunk per job, like the sweeper: the caller repeats while `More` says
// so, so a user with a year of traffic does not hold the writer for the
// length of one transaction.
//
// Each chunk re-rolls the hours it emptied **inside its own transaction**
// (spec 023 #19): a per-hour count is data derived from what was just erased
// (spec 013 #7), and a correction that commits with the deletion or not at
// all is the only one a client that hangs up between chunks cannot lose. The
// handler used to submit the rolls after the last chunk, so a browser that
// gave up at thirty seconds (spec 010 #10) left every hour the earlier chunks
// emptied still counting the traces — and a repeat of the request could not
// find those hours again, because the traces that named them were gone.
//
// A chunk takes the user's traces in the order they started, whole hours at a
// time (spec 047 #1): its first hour always, and each next one while the chunk
// stays within `Limit` traces and `RollBudget` of what its rolls recompute
// (#2). In start order a user's hours
// are contiguous, so each is rolled by the one chunk that takes it — or, for
// an hour holding more than `Limit` of the user's traces, which is cut, by
// each of the ⌈n / Limit⌉ chunks it spans. Taken in arrival order, as before, a chunk kept
// one hour's worth of the first 500 traces: for a client that exports out of
// start order, about two traces, one commit and one roll per chunk.
type UserDataErase struct {
	ProjectID string
	UserID    string
	Confirm   string
	Limit     int
	// RollBudget bounds what the chunk's rolls recompute, its first hour
	// included, though the first hour is taken whatever it costs (spec 047
	// #2); zero is DeleteRollBudget.
	RollBudget int64
	// Now is the clock the freeze is measured against (spec 013 #11): an
	// hour past the project's retention window is left as it stands. Zero
	// is the wall clock, not the epoch — measured against 1970 nothing
	// would be past the window, and a frozen hour would be recomputed
	// from what the sweep left of it.
	Now int64

	Counts DeleteCounts
	// Hours are the hours this chunk emptied; the ones below the watermark
	// were re-rolled.
	Hours []int64
	// More reports that traces of the user remain after this chunk: the
	// caller submits another. A chunk cut at an hour is not a chunk that
	// came back short.
	More bool
	// CompactionRequested is the stamp of the compaction this chunk asked
	// for, zero when it deleted nothing (spec 044 #1, #11).
	CompactionRequested int64
	// IDs are the traces this chunk deleted, Ingested when each arrived
	// and Updated when each last changed: the erasure's tail scrubs the
	// batches that arrived for them while it ran (spec 044 #4), and the whole
	// window of one that was not the user's when the erasure began.
	IDs      []string
	Ingested []int64
	Updated  []int64
}

// weight is the traces the chunk may take, its Limit (spec 043 #35): a floor,
// as a trace delete's is, since each takes its observations and scores with it.
func (e *UserDataErase) weight() int { return e.Limit }

func (e *UserDataErase) apply(tx *sql.Tx) error {
	e.Counts, e.CompactionRequested, e.IDs, e.Ingested, e.Updated = DeleteCounts{}, 0, nil, nil, nil
	// The echo is the user id here, not a project name: it is the identity
	// of what is being destroyed (spec 005 #8).
	if e.Confirm != e.UserID {
		return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the user id being erased, %q, to erase their data", e.UserID)}
	}
	project, err := projectByID(tx, e.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}

	// A chunk of nothing would say `More` for ever: `LIMIT 0` selects no
	// rows and a scan of zero rows is a full one. A programmer's mistake,
	// so it fails the job rather than being read as a size.
	if e.Limit <= 0 {
		return fmt.Errorf("erase a user's data: chunk limit %d is not positive", e.Limit)
	}

	e.Hours, e.More = nil, false
	// One trace past the chunk says whether there is more, and whether the
	// chunk's last hour is whole.
	rows, err := tx.Query(userTracesByStart, e.ProjectID, e.UserID, e.Limit+1)
	if err != nil {
		return fmt.Errorf("select a user's traces: %w", err)
	}
	type row struct {
		id                string
		hour              int64
		ingested, updated int64
	}
	var read []row
	var hours, deletes []int64
	for rows.Next() {
		var (
			r row
			// NULL for a trace no span has reached — one a score
			// or an item names before its spans arrived. Trace
			// deletion reads it the same way.
			timestamp    sql.NullInt64
			updated      sql.NullInt64
			observations int
		)
		if err := rows.Scan(&r.id, &timestamp, &r.ingested, &updated, &observations); err != nil {
			rows.Close()
			return err
		}
		r.hour, r.updated = HourOf(timestamp.Int64), updated.Int64
		read = append(read, r)
		hours = append(hours, r.hour)
		deletes = append(deletes, TraceDeleteCost(observations))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var ids []any
	updates, ingests := map[string]int64{}, map[string]int64{}
	if len(read) > 0 {
		cost, err := rollCosts(context.Background(), tx, e.ProjectID)
		if err != nil {
			return err
		}
		// One chunk is this job: the hours past it are the next job's to
		// price, against what the store holds then.
		ends, err := HourChunks(hours, deletes, cost, e.Limit, e.RollBudget, 1)
		if err != nil {
			return err
		}
		end := ends[0]
		e.More = end < len(read)
		// Once each: a trace with no start time reads as hour 0 and sorts
		// before one that started before the epoch, so the same hour can
		// come back later in the run.
		rolled := map[int64]bool{}
		for _, r := range read[:end] {
			if !rolled[r.hour] {
				rolled[r.hour] = true
				e.Hours = append(e.Hours, r.hour)
			}
			ids = append(ids, r.id)
			updates[r.id], ingests[r.id] = r.updated, r.ingested
		}
	}
	if len(ids) == 0 {
		// Nothing to delete or to roll; the per-user rows still go
		// (below), for a user whose every hour is frozen.
		return e.eraseRollup(tx)
	}

	// The body is the one trace deletion shares (spec 035 #3): the rows
	// hanging off the traces, the traces, the orphaned payloads, the search
	// entries, then one whole `RollHour` per hour this chunk touched, in
	// this same transaction. The chunks follow `idx_traces_user`, which is
	// start order, so an hour is rolled by one chunk. The erased user's own
	// summary is not recomputed: it goes outright, below.
	//
	// Before the traces go, what only they lead to: the sessions they
	// carried and the items cut from them are found through the traces.
	sessionScores, err := deleteSessionScores(tx, e.ProjectID, ids)
	if err != nil {
		return err
	}
	items, err := deleteSourcedItems(tx, e.ProjectID, ids, e.Now)
	if err != nil {
		return err
	}
	removal := &traceRemoval{projectID: e.ProjectID, ids: ids, hours: e.Hours,
		now: e.Now, skipUser: e.UserID}
	if e.Counts, err = removal.apply(tx); err != nil {
		return err
	}
	e.Counts.SessionScores, e.Counts.DatasetItems = sessionScores, items
	e.CompactionRequested = removal.compactionAt
	for _, id := range ids {
		e.IDs = append(e.IDs, id.(string))
		e.Ingested = append(e.Ingested, ingests[id.(string)])
		e.Updated = append(e.Updated, updates[id.(string)])
	}

	// The per-user rollup goes outright, in this same request (spec 023
	// #10): those rows are *about* the user, and a re-roll would recompute
	// them to nothing from raw rows that are gone — or, for a frozen hour
	// (spec 013 #11), could not recompute them at all. After the rolls,
	// not before: an hour that straddles the chunk boundary still holds
	// traces of the user, and its roll would write them back as a row and
	// a summary built from that one hour — a user listed again, with a
	// wrong number, between a hang-up and the repeat (review of PR #65).
	// On every chunk, because deleting is idempotent and the last chunk is
	// not known in advance.
	return e.eraseRollup(tx)
}

// userTracesByStart is a chunk's selection: the user's traces in the order they
// started, walking `idx_traces_user` (spec 047 #1).
const userTracesByStart = `SELECT id, timestamp, ingested_at, updated_at, observation_count FROM traces
	WHERE project_id = ? AND user_id = ? ORDER BY timestamp LIMIT ?`

// eraseRollup deletes the user's per-user rows and, when that removed any and
// the chunk has not asked already, asks for the compaction every erasure that
// deleted something asks for (spec 044 #1): those rows name the user too.
func (e *UserDataErase) eraseRollup(tx *sql.Tx) error {
	removed, err := deleteUserRollup(tx, e.ProjectID, e.UserID)
	if err != nil || removed == 0 || e.CompactionRequested != 0 {
		return err
	}
	e.CompactionRequested, err = requestCompaction(tx, e.Now)
	return err
}

// confirmProjectName is Decision 8 in one function: a destructive job executes
// only when its `confirm` echoes the stored name of what it destroys. An id is
// a string you paste; a name is a thing you mean, so a typoed or hallucinated
// target cannot match — spec 003 #23 applied to destruction.
func confirmProjectName(tx *sql.Tx, projectID, confirm string) (*Project, error) {
	project, err := projectByID(tx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	if confirm != project.Name {
		return nil, &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the project's name, %q, for this to happen", project.Name)}
	}
	return project, nil
}

func projectByID(tx *sql.Tx, id string) (*Project, error) {
	return txProject(tx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
}

func projectByName(tx *sql.Tx, name string) (*Project, error) {
	return txProject(tx, `SELECT `+projectColumns+` FROM projects WHERE name = ?`, name)
}

func txProject(tx *sql.Tx, query string, args ...any) (*Project, error) {
	project, err := scanProject(tx.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return project, nil
}
