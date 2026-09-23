package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Media (spec 041): the bodies ingest took out of the JSON, one row per
// distinct content (#2), and the two tables of who points at them — traces
// (#3) and raw batches (Decision 12). A body lives while either names it;
// every path that deletes traces or raw batches deletes their refs in its own
// transaction and collects the bodies left with none, the way it collects
// payloads.

// MediaBody is one body to store.
type MediaBody struct {
	SHA256   string
	MimeType string
	Body     []byte
}

// MediaRef is one trace pointing at one body.
type MediaRef struct {
	SHA256  string
	TraceID string
}

// MediaOrphanGrace is how long a ref may name a trace that is not there before
// the sweep takes it (Decision 13). The Langfuse channel records the ref when
// the SDK asks to upload, and the spans follow on the exporter's own clock —
// seconds, or minutes of retries — so an hour-old sweep reading "no trace" as
// "never" would take a picture whose trace was merely late.
const MediaOrphanGrace = 24 * time.Hour

// MediaIDFor is the Langfuse SDK's id for a body: the first 22 characters of
// the URL-safe base64 of its SHA-256 (`langfuse/media.py`, `_get_media_id`),
// which the SDK checks against the server's answer (#9).
func MediaIDFor(sha string) string {
	sum, err := hex.DecodeString(sha)
	if err != nil || len(sum) != sha256.Size {
		return ""
	}
	return base64.URLEncoding.EncodeToString(sum)[:22]
}

// shaPrefixOf turns a Langfuse media id into the hex prefix its first sixteen
// bytes spell, the range a lookup seeks on; the id's last four bits are
// compared in full afterwards, by MediaIDFor.
func shaPrefixOf(mediaID string) (string, bool) {
	if len(mediaID) != 22 {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(mediaID)
	if err != nil || len(decoded) < 16 {
		return "", false
	}
	return hex.EncodeToString(decoded[:16]), true
}

// writeMedia stores an ingest's bodies and refs inside its transaction. A body
// already stored is left as it is — the first stored type wins (spec 041, edge
// cases) — and a ref is written only while its body exists: a resolved
// Langfuse id names a body read outside this transaction, which a deletion may
// have collected since.
func writeMedia(tx *sql.Tx, projectID string, bodies []MediaBody, refs []MediaRef, now int64) error {
	for _, body := range bodies {
		if _, err := tx.Exec(
			`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(sha256) DO NOTHING`,
			body.SHA256, body.MimeType, len(body.Body), body.Body, now); err != nil {
			return fmt.Errorf("store media %s: %w", body.SHA256, err)
		}
	}
	for _, ref := range refs {
		if _, err := tx.Exec(
			`INSERT INTO media_refs (sha256, project_id, trace_id, created_at)
			 SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			 ON CONFLICT DO NOTHING`,
			ref.SHA256, projectID, ref.TraceID, now, ref.SHA256); err != nil {
			return fmt.Errorf("store media ref %s: %w", ref.SHA256, err)
		}
	}
	return nil
}

// writeRawMediaRefs records the bodies a raw batch points at (Decision 12).
func writeRawMediaRefs(tx *sql.Tx, batchID int64, shas []string) error {
	for _, sha := range shas {
		if _, err := tx.Exec(
			`INSERT INTO media_raw_refs (sha256, raw_batch_id)
			 SELECT ?, ? WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			 ON CONFLICT DO NOTHING`,
			sha, batchID, sha); err != nil {
			return fmt.Errorf("store raw media ref %s: %w", sha, err)
		}
	}
	return nil
}

// dropTraceMedia deletes a set of traces' refs and collects the bodies no ref
// names any more. It answers how many bodies went and their bytes.
func dropTraceMedia(tx *sql.Tx, projectID string, traceIDs []any) (int64, int64, error) {
	var shas []any
	err := eachIn(traceIDs, func(batch []any) error {
		rows, err := tx.Query(`SELECT DISTINCT sha256 FROM media_refs WHERE project_id = ? AND trace_id IN (`+
			placeholders(len(batch))+`)`, append([]any{projectID}, batch...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				return err
			}
			shas = append(shas, sha)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, 0, fmt.Errorf("find the traces' media: %w", err)
	}
	if len(shas) == 0 {
		return 0, 0, nil
	}
	if _, err := deleteIn(tx, `DELETE FROM media_refs WHERE project_id = ? AND trace_id IN`,
		[]any{projectID}, traceIDs); err != nil {
		return 0, 0, fmt.Errorf("delete media refs: %w", err)
	}
	return collectMedia(tx, shas)
}

// dropRawMedia is dropTraceMedia for raw batches about to be deleted.
func dropRawMedia(tx *sql.Tx, batchIDs []any) (int64, int64, error) {
	var shas []any
	err := eachIn(batchIDs, func(batch []any) error {
		rows, err := tx.Query(`SELECT DISTINCT sha256 FROM media_raw_refs WHERE raw_batch_id IN (`+
			placeholders(len(batch))+`)`, batch...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				return err
			}
			shas = append(shas, sha)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, 0, fmt.Errorf("find the raw batches' media: %w", err)
	}
	if len(shas) == 0 {
		return 0, 0, nil
	}
	if _, err := deleteIn(tx, `DELETE FROM media_raw_refs WHERE raw_batch_id IN`, nil, batchIDs); err != nil {
		return 0, 0, fmt.Errorf("delete raw media refs: %w", err)
	}
	return collectMedia(tx, shas)
}

// collectMedia deletes those of the given bodies nothing points at any more:
// no trace of any project, no raw batch. The predicate is the whole check, so
// a body another project still holds survives this project's deletion.
func collectMedia(tx *sql.Tx, shas []any) (count, size int64, err error) {
	err = eachIn(shas, func(batch []any) error {
		rows, err := tx.Query(
			`DELETE FROM media WHERE sha256 IN (`+placeholders(len(batch))+`)
			   AND NOT EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = media.sha256)
			   AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = media.sha256)
			 RETURNING size`, batch...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n int64
			if err := rows.Scan(&n); err != nil {
				return err
			}
			count++
			size += n
		}
		return rows.Err()
	})
	if err != nil {
		return 0, 0, fmt.Errorf("collect media: %w", err)
	}
	return count, size, nil
}

// MediaFile is one body as the read endpoint serves it.
type MediaFile struct {
	MimeType string
	Body     []byte
}

// holdsMedia is the scope of #7: a body is readable by a project that points
// at it, from a trace or from a raw batch, and by nobody else.
const holdsMedia = `(EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = m.sha256 AND r.project_id = ?)
	 OR EXISTS (SELECT 1 FROM media_raw_refs rr JOIN raw_batches b ON b.id = rr.raw_batch_id
	            WHERE rr.sha256 = m.sha256 AND b.project_id = ?))`

// MediaFor reads one body for a project, or nil when the project holds no ref
// to it — which is also what a body that does not exist looks like, so a hash
// is not a capability across projects (#7).
func (s *Store) MediaFor(projectID, sha string) (*MediaFile, error) {
	var file MediaFile
	err := s.db.QueryRow(
		`SELECT m.mime_type, m.body FROM media m WHERE m.sha256 = ? AND `+holdsMedia,
		sha, projectID, projectID).Scan(&file.MimeType, &file.Body)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read media %s: %w", sha, err)
	}
	return &file, nil
}

// MediaInfo is what a Langfuse lookup answers: the body's identity and size,
// never its bytes.
type MediaInfo struct {
	SHA256    string
	MimeType  string
	Size      int64
	CreatedAt int64
}

// MediaHeld reports whether a project holds a ref to a body, and what the body
// is. It decides the Langfuse channel's `uploadUrl: null` (#9): only a
// project that already holds the bytes skips the upload, because a hash any
// project could name would otherwise be a way to adopt another's picture.
func (s *Store) MediaHeld(projectID, sha string) (*MediaInfo, error) {
	var info MediaInfo
	err := s.db.QueryRow(
		`SELECT m.sha256, m.mime_type, m.size, m.created_at FROM media m WHERE m.sha256 = ? AND `+holdsMedia,
		sha, projectID, projectID).Scan(&info.SHA256, &info.MimeType, &info.Size, &info.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up media %s: %w", sha, err)
	}
	return &info, nil
}

// MediaByLangfuseID resolves the SDK's id to a body this project holds (#9):
// a seek on the hash range the id's first sixteen bytes spell, then the whole
// id compared.
func (s *Store) MediaByLangfuseID(projectID, mediaID string) (*MediaInfo, error) {
	prefix, ok := shaPrefixOf(mediaID)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT m.sha256, m.mime_type, m.size, m.created_at FROM media m
		  WHERE m.sha256 >= ? AND m.sha256 < ? AND `+holdsMedia,
		prefix, prefix+"g", projectID, projectID)
	if err != nil {
		return nil, fmt.Errorf("look up media %s: %w", mediaID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var info MediaInfo
		if err := rows.Scan(&info.SHA256, &info.MimeType, &info.Size, &info.CreatedAt); err != nil {
			return nil, err
		}
		if MediaIDFor(info.SHA256) == mediaID {
			return &info, nil
		}
	}
	return nil, rows.Err()
}

// MediaSummary is the system endpoint's media figure (#11): the bodies a
// project points at and their decoded bytes. A body two projects share is in
// both figures, because each would keep it alive alone.
type MediaSummary struct {
	Count int64
	Bytes int64
}

// MediaSummary counts what a project's refs hold.
func (s *Store) MediaSummary(projectID string) (MediaSummary, error) {
	var summary MediaSummary
	err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM media WHERE sha256 IN (
		   SELECT sha256 FROM media_refs WHERE project_id = ?
		   UNION
		   SELECT rr.sha256 FROM media_raw_refs rr JOIN raw_batches b ON b.id = rr.raw_batch_id
		    WHERE b.project_id = ?)`,
		projectID, projectID).Scan(&summary.Count, &summary.Bytes)
	if err != nil {
		return summary, fmt.Errorf("summarise media: %w", err)
	}
	return summary, nil
}

// mediaFreed counts the bodies a deletion would collect (#11): those some of
// the traces or raw batches going point at, and nothing staying does. `traces`
// selects trace ids of the project and `raws` raw batch ids; either may be
// empty, which is none.
func (s *Store) mediaFreed(projectID, traces string, traceArgs []any, raws string, rawArgs []any) (count, size int64, err error) {
	if traces == "" {
		traces = `SELECT NULL WHERE 0`
	}
	if raws == "" {
		raws = `SELECT NULL WHERE 0`
	}
	args := append(append(append([]any{}, traceArgs...), rawArgs...), projectID, projectID)
	err = s.db.QueryRow(
		`WITH gone(trace_id) AS (`+traces+`), gone_raw(id) AS (`+raws+`)
		 SELECT COUNT(*), COALESCE(SUM(m.size), 0) FROM media m
		  WHERE (m.sha256 IN (SELECT sha256 FROM media_refs WHERE project_id = ? AND trace_id IN gone)
		         OR m.sha256 IN (SELECT sha256 FROM media_raw_refs WHERE raw_batch_id IN gone_raw))
		    AND NOT EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = m.sha256
		                     AND NOT (r.project_id = ? AND r.trace_id IN gone))
		    AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = m.sha256
		                     AND rr.raw_batch_id NOT IN gone_raw)`,
		args...).Scan(&count, &size)
	if err != nil {
		return 0, 0, fmt.Errorf("count the media a deletion frees: %w", err)
	}
	return count, size, nil
}

// MediaRefAdd records that a trace points at a body the project already holds
// — the Langfuse channel's answer to a second identical upload, which asks
// for no bytes (#9). It reports whether the body was still there to point at.
type MediaRefAdd struct {
	ProjectID string
	SHA256    string
	TraceID   string
	Now       int64

	Added bool
}

func (a *MediaRefAdd) apply(tx *sql.Tx) error {
	result, err := tx.Exec(
		`INSERT INTO media_refs (sha256, project_id, trace_id, created_at)
		 SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
		 ON CONFLICT DO NOTHING`,
		a.SHA256, a.ProjectID, a.TraceID, nowOr(a.Now), a.SHA256)
	if err != nil {
		return fmt.Errorf("add media ref: %w", err)
	}
	if n, err := result.RowsAffected(); err == nil && n > 0 {
		a.Added = true
		return nil
	}
	var one int
	err = tx.QueryRow(`SELECT 1 FROM media_refs WHERE sha256 = ? AND project_id = ? AND trace_id = ?`,
		a.SHA256, a.ProjectID, a.TraceID).Scan(&one)
	a.Added = err == nil
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}

// MediaUpload stores one body the Langfuse channel received and the ref of
// the trace it was sent for (#9).
type MediaUpload struct {
	ProjectID string
	TraceID   string
	Body      MediaBody
	Now       int64
}

func (u *MediaUpload) apply(tx *sql.Tx) error {
	return writeMedia(tx, u.ProjectID, []MediaBody{u.Body},
		[]MediaRef{{SHA256: u.Body.SHA256, TraceID: u.TraceID}}, nowOr(u.Now))
}

func nowOr(now int64) int64 {
	if now == 0 {
		return time.Now().UnixNano()
	}
	return now
}

// orphanMediaRefs finds refs naming a trace the project does not have, older
// than the grace (Decision 13). Read outside the writer like the orphaned
// payloads are; the deletion re-checks inside its transaction.
func (s *Store) orphanMediaRefs(before int64, limit int) ([]MediaOrphan, error) {
	rows, err := s.db.Query(
		`SELECT sha256, project_id, trace_id FROM media_refs r
		  WHERE r.created_at < ?
		    AND NOT EXISTS (SELECT 1 FROM traces t WHERE t.project_id = r.project_id AND t.id = r.trace_id)
		  LIMIT ?`, before, limit)
	if err != nil {
		return nil, fmt.Errorf("find orphaned media refs: %w", err)
	}
	defer rows.Close()
	var out []MediaOrphan
	for rows.Next() {
		var o MediaOrphan
		if err := rows.Scan(&o.SHA256, &o.ProjectID, &o.TraceID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// orphanMedia finds bodies no ref names at all — what a hand-edited database,
// or a crash between two statements that should never have been two, would
// leave. The scan reads the primary-key index, never a body.
func (s *Store) orphanMedia(limit int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT sha256 FROM media m
		  WHERE NOT EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = m.sha256)
		    AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = m.sha256)
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("find orphaned media: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// MediaOrphan is one ref whose trace never came.
type MediaOrphan struct {
	SHA256    string
	ProjectID string
	TraceID   string
}

// mediaSweep deletes the orphaned refs the read pass found — each one only if
// its trace is still absent — and collects the bodies they leave, together
// with any body the pass found with no ref at all.
type mediaSweep struct {
	Refs   []MediaOrphan
	Bodies []string

	Deleted int64
}

func (m *mediaSweep) apply(tx *sql.Tx) error {
	m.Deleted = 0
	var shas []any
	for _, ref := range m.Refs {
		if _, err := tx.Exec(
			`DELETE FROM media_refs WHERE sha256 = ? AND project_id = ? AND trace_id = ?
			   AND NOT EXISTS (SELECT 1 FROM traces WHERE project_id = ? AND id = ?)`,
			ref.SHA256, ref.ProjectID, ref.TraceID, ref.ProjectID, ref.TraceID); err != nil {
			return fmt.Errorf("delete orphaned media ref: %w", err)
		}
		shas = append(shas, ref.SHA256)
	}
	for _, sha := range m.Bodies {
		shas = append(shas, sha)
	}
	count, _, err := collectMedia(tx, shas)
	m.Deleted = count
	return err
}

// ValidMediaSHA reports a lower-case hex SHA-256, the only spelling a media
// path accepts.
func ValidMediaSHA(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// dropProjectMedia deletes every ref a purged project still has and collects
// the bodies only it pointed at (spec 041 #3). A body another project also
// points at survives: the refs carry the project for exactly this.
func dropProjectMedia(tx *sql.Tx, projectID string) error {
	rows, err := tx.Query(`SELECT DISTINCT sha256 FROM media_refs WHERE project_id = ?`, projectID)
	if err != nil {
		return fmt.Errorf("find a project's media: %w", err)
	}
	var shas []any
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM media_refs WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("delete a project's media refs: %w", err)
	}
	_, _, err = collectMedia(tx, shas)
	return err
}
