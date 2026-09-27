package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
)

// Media (spec 041): the bodies ingest took out of the JSON, one row per
// distinct content (#2), and the two tables of who points at them — traces
// (#3) and raw batches (Decision 12). A body lives while either names it;
// every path that deletes traces or raw batches deletes their refs in its own
// transaction and collects the bodies left with none, the way it collects
// payloads.
//
// What a project sees of a body is its own (Decision 25): `media_holders`
// keeps, per body and project that points at it, the type that project
// stored it under and when its hold began. The row is written with the
// project's first ref to the body and deleted with its last, in the same
// transaction, so "does this project hold this body" is a seek on the pair
// (Decision 26) and never looks at another project's rows.

// MediaBody is one body to store, and MediaRef one trace pointing at one
// body: the walk's own types, written as it found them.
type (
	MediaBody = mapping.MediaBody
	MediaRef  = mapping.MediaRef
)

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
// already stored is left as it is — its bytes are the same whoever sent them —
// and a ref is written only while its body exists: a resolved Langfuse id
// names a body read outside this transaction, which a deletion may have
// collected since. A ref is pending until its trace is stored, and the trace's
// arrival settles it (Decision 13): within one transaction that is the same
// transaction, since the traces are written after the refs, but an export cut
// into slices writes every ref with its first slice and a trace perhaps with a
// later one (spec 043 #11). Pending, a ref whose trace never comes — an export
// that failed half-way and was never retried — is collected by the sweep, as
// the Langfuse channel's are, instead of holding its body for ever. One the
// channel left pending is settled the same way. The project's
// hold is recorded with its first ref, under the type this batch declared
// (Decision 25). It answers the bodies it recorded a hold of, so that the
// batch's raw refs do not record them again.
func writeMedia(tx *sql.Tx, projectID string, bodies []MediaBody, types map[string]string, refs []MediaRef,
	now int64) (map[string]bool, error) {
	if err := writeMediaBodies(tx, bodies, now); err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for _, ref := range refs {
		result, err := tx.Exec(
			`INSERT INTO media_refs (sha256, project_id, trace_id, created_at, pending)
			 SELECT ?, ?, ?, ?, NOT `+traceStoredExpr+` WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			 ON CONFLICT (sha256, project_id, trace_id) DO UPDATE SET pending = MIN(pending, excluded.pending)`,
			ref.SHA256, projectID, ref.TraceID, now, projectID, ref.TraceID, ref.SHA256)
		if err != nil {
			return nil, fmt.Errorf("store media ref %s: %w", ref.SHA256, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("store media ref %s: %w", ref.SHA256, err)
		}
		// One hold per body, however many of the batch's traces point at
		// it.
		if n == 0 || held[ref.SHA256] {
			continue
		}
		held[ref.SHA256] = true
		if err := holdMedia(tx, projectID, ref.SHA256, types[ref.SHA256], now); err != nil {
			return nil, err
		}
	}
	return held, nil
}

// declaredTypes is the type a batch declared for each of its bodies.
func declaredTypes(bodies []MediaBody) map[string]string {
	types := make(map[string]string, len(bodies))
	for _, body := range bodies {
		if _, seen := types[body.SHA256]; !seen {
			types[body.SHA256] = body.MimeType
		}
	}
	return types
}

// holdMedia records that a project holds a body, under the type it declared
// and from now (Decision 25). A project that already holds it keeps the type
// and the time it had: within a project the first type wins. A ref with no
// type of its own in hand — a Langfuse id resolved to a body the project
// already holds — finds the row there. Were it ever not there, the hold is
// `application/octet-stream`: never the type another project declared.
func holdMedia(tx *sql.Tx, projectID, sha, mimeType string, now int64) error {
	if _, err := tx.Exec(
		`INSERT INTO media_holders (sha256, project_id, mime_type, first_at)
		 SELECT m.sha256, ?, COALESCE(NULLIF(?, ''), 'application/octet-stream'), ? FROM media m WHERE m.sha256 = ?
		 ON CONFLICT DO NOTHING`,
		projectID, mimeType, now, sha); err != nil {
		return fmt.Errorf("hold media %s: %w", sha, err)
	}
	return nil
}

func writeMediaBodies(tx *sql.Tx, bodies []MediaBody, now int64) error {
	for _, body := range bodies {
		if _, err := tx.Exec(
			`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(sha256) DO NOTHING`,
			body.SHA256, body.MimeType, len(body.Body), body.Body, now); err != nil {
			return fmt.Errorf("store media %s: %w", body.SHA256, err)
		}
	}
	return nil
}

// writeChannelRef is the Langfuse channel's ref (#9), written when the SDK
// uploads — usually before the trace's spans arrive, so it is pending until
// they do (Decision 13) — with the project's hold under the type the upload
// declared. It reports whether a ref now exists: false when the body is gone.
func writeChannelRef(tx *sql.Tx, projectID, sha, traceID, mimeType string, now int64) (bool, error) {
	if _, err := tx.Exec(
		`INSERT INTO media_refs (sha256, project_id, trace_id, created_at, pending)
		 SELECT ?, ?, ?, ?, NOT `+traceStoredExpr+`
		  WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
		 ON CONFLICT DO NOTHING`,
		sha, projectID, traceID, now, projectID, traceID, sha); err != nil {
		return false, fmt.Errorf("add media ref: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(`SELECT `+refExistsExpr, sha, projectID, traceID).Scan(&exists); err != nil || !exists {
		return false, err
	}
	return true, holdMedia(tx, projectID, sha, mimeType, now)
}

// ErrMediaGone refuses an ingest whose payloads were rewritten to a body a
// deletion collected after the rewrite read it. The caller takes the batch
// again without resolving, so the client's reference string is kept rather
// than a reference to nothing.
var ErrMediaGone = errors.New("a resolved media body was collected before the write")

// mediaStillThere checks, inside the ingest's transaction, that the project
// still holds every body a Langfuse string was resolved to (Decision 26): a
// body another project keeps alive is not one this project may point at once
// its own last ref has gone.
func mediaStillThere(tx *sql.Tx, projectID string, shas []string) error {
	for _, sha := range shas {
		hold, err := projectHold(context.Background(), tx, projectID, sha)
		if err != nil {
			return err
		}
		if hold == nil {
			return ErrMediaGone
		}
	}
	return nil
}

// writeRawMediaRefs records the bodies a raw batch points at (Decision 12),
// under the batch's project (Decision 26), and the project's hold of each
// the batch's trace refs did not already record (`held`).
func writeRawMediaRefs(tx *sql.Tx, projectID string, batchID int64, shas []string, types map[string]string,
	held map[string]bool, now int64) error {
	for _, sha := range shas {
		result, err := tx.Exec(
			`INSERT INTO media_raw_refs (sha256, raw_batch_id, project_id)
			 SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			 ON CONFLICT DO NOTHING`,
			sha, batchID, projectID, sha)
		if err != nil {
			return fmt.Errorf("store raw media ref %s: %w", sha, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store raw media ref %s: %w", sha, err)
		}
		if n == 0 || held[sha] {
			continue
		}
		if err := holdMedia(tx, projectID, sha, types[sha], now); err != nil {
			return err
		}
	}
	return nil
}

// mediaDrop is what deleting a set of refs did to media: the bodies the
// project stopped holding with their bytes — what a deletion answers the
// project (Decision 27) — and the bodies collected, which only the sweep's log
// reports.
type mediaDrop struct {
	Released, ReleasedBytes int64
	Collected               int64
}

// dropTraceMedia deletes a set of traces' refs, releases the project's hold
// of the bodies it no longer points at, and collects the bodies no ref names
// any more.
func dropTraceMedia(tx *sql.Tx, projectID string, traceIDs []any) (mediaDrop, error) {
	var shas []any
	err := eachIn(traceIDs, func(batch []any) error {
		found, err := queryColumn[string](tx, `SELECT DISTINCT sha256 FROM media_refs WHERE project_id = ? AND trace_id IN (`+
			placeholders(len(batch))+`)`, append([]any{projectID}, batch...)...)
		shas = append(shas, found...)
		return err
	})
	if err != nil {
		return mediaDrop{}, fmt.Errorf("find the traces' media: %w", err)
	}
	if len(shas) == 0 {
		return mediaDrop{}, nil
	}
	if _, err := deleteIn(tx, `DELETE FROM media_refs WHERE project_id = ? AND trace_id IN`,
		[]any{projectID}, traceIDs); err != nil {
		return mediaDrop{}, fmt.Errorf("delete media refs: %w", err)
	}
	return releaseAndCollect(tx, projectID, shas)
}

// dropRawMedia is dropTraceMedia for a project's raw batches about to be
// deleted.
func dropRawMedia(tx *sql.Tx, projectID string, batchIDs []any) (mediaDrop, error) {
	var shas []any
	err := eachIn(batchIDs, func(batch []any) error {
		found, err := queryColumn[string](tx, `SELECT DISTINCT sha256 FROM media_raw_refs WHERE raw_batch_id IN (`+
			placeholders(len(batch))+`)`, batch...)
		shas = append(shas, found...)
		return err
	})
	if err != nil {
		return mediaDrop{}, fmt.Errorf("find the raw batches' media: %w", err)
	}
	if len(shas) == 0 {
		return mediaDrop{}, nil
	}
	if _, err := deleteIn(tx, `DELETE FROM media_raw_refs WHERE raw_batch_id IN`, nil, batchIDs); err != nil {
		return mediaDrop{}, fmt.Errorf("delete raw media refs: %w", err)
	}
	return releaseAndCollect(tx, projectID, shas)
}

func releaseAndCollect(tx *sql.Tx, projectID string, shas []any) (mediaDrop, error) {
	var drop mediaDrop
	var err error
	if drop.Released, drop.ReleasedBytes, err = releaseMedia(tx, projectID, shas); err != nil {
		return drop, err
	}
	drop.Collected, _, err = collectMedia(tx, shas)
	return drop, err
}

// releaseMedia deletes the project's hold of those of the given bodies it no
// longer points at, from a trace or a raw batch (Decision 25), and answers how
// many it released and their bytes (Decision 27). Asked before collecting, so
// the body's size is still there to read.
func releaseMedia(tx *sql.Tx, projectID string, shas []any) (count, size int64, err error) {
	err = eachIn(shas, func(batch []any) error {
		rows, err := tx.Query(
			`DELETE FROM media_holders WHERE project_id = ? AND sha256 IN (`+placeholders(len(batch))+`)
			   AND NOT EXISTS (SELECT 1 FROM media_refs r
			                    WHERE r.sha256 = media_holders.sha256 AND r.project_id = media_holders.project_id)
			   AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr
			                    WHERE rr.sha256 = media_holders.sha256 AND rr.project_id = media_holders.project_id)
			 RETURNING (SELECT size FROM media m WHERE m.sha256 = media_holders.sha256)`,
			append([]any{projectID}, batch...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n sql.NullInt64
			if err := rows.Scan(&n); err != nil {
				return err
			}
			count++
			size += n.Int64
		}
		return rows.Err()
	})
	if err != nil {
		return 0, 0, fmt.Errorf("release media: %w", err)
	}
	return count, size, nil
}

// collectMedia deletes those of the given bodies nothing points at any more:
// no trace of any project, no raw batch. The predicate is the whole check, so
// a body another project still holds survives this project's deletion. A
// holder row left over goes with its body by cascade.
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

// holdsBody is the rest of the scope of #7 as Decision 26 has it, asked of a
// holder row h: a project holds a body when it has a holder row for it and a
// ref to it, from a trace or from a raw batch — each a seek on (sha256,
// project_id). The one predicate every "does this project hold this body"
// asks.
const holdsBody = `(EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = h.sha256 AND r.project_id = h.project_id)
	    OR EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = h.sha256 AND rr.project_id = h.project_id))`

// mediaScope answers, for a project holding a body, the project's own type,
// the start of its hold (Decision 25) and the body's size, and no row
// otherwise: a hash nobody holds and a hash only another project holds are
// the same few missed probes. One statement, so one snapshot; the size is read
// only for a row the scope admits.
const mediaScope = `SELECT h.mime_type, h.first_at, (SELECT m.size FROM media m WHERE m.sha256 = h.sha256)
	   FROM media_holders h
	  WHERE h.sha256 = ? AND h.project_id = ? AND ` + holdsBody

// projectHold is the one lookup of a project's hold of a body (Decision 26):
// nil when the project holds none, or the body was collected meanwhile.
func projectHold(ctx context.Context, q querier, projectID, sha string) (*MediaInfo, error) {
	info := MediaInfo{SHA256: sha}
	var size sql.NullInt64
	err := q.QueryRowContext(ctx, mediaScope, sha, projectID).Scan(&info.MimeType, &info.CreatedAt, &size)
	if err == sql.ErrNoRows || err == nil && !size.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up media %s: %w", sha, err)
	}
	info.Size = size.Int64
	return &info, nil
}

// mediaByID is the Langfuse id's lookup (#9, Decision 26): a seek on this
// project's holder rows in the hash range the id's first sixteen bytes spell,
// through `idx_media_holders_project`, so no other project's row is read.
const mediaByID = `SELECT h.sha256, h.mime_type, h.first_at,
	        (SELECT m.size FROM media m WHERE m.sha256 = h.sha256)
	   FROM media_holders h
	  WHERE h.project_id = ? AND h.sha256 >= ? AND h.sha256 < ? AND ` + holdsBody

// mediaRead is the scope with the body read in the same statement, so in one
// snapshot: the body only once the answer is yes, and never after the
// project's hold went. A NULL body is one collected meanwhile.
const mediaRead = `SELECT h.mime_type, (SELECT m.body FROM media m WHERE m.sha256 = h.sha256)
	   FROM media_holders h
	  WHERE h.sha256 = ? AND h.project_id = ? AND ` + holdsBody

// MediaFor reads one body for a project, under the project's own type, or nil
// when the project holds no ref to it — which is also what a body that does
// not exist looks like, so a hash is not a capability across projects (#7).
func (s *Store) MediaFor(ctx context.Context, projectID, sha string) (*MediaFile, error) {
	var file MediaFile
	err := s.db.QueryRowContext(ctx, mediaRead, sha, projectID).Scan(&file.MimeType, &file.Body)
	if err == sql.ErrNoRows || err == nil && file.Body == nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read media %s: %w", sha, err)
	}
	return &file, nil
}

// MediaInfo is what a Langfuse lookup answers: the body's identity and size,
// never its bytes, and the asking project's own type and the start of its
// hold (Decision 25).
type MediaInfo struct {
	SHA256    string
	MimeType  string
	Size      int64
	CreatedAt int64
}

// MediaHeld reports whether a project holds a ref to a body, and what the body
// is to it. It decides the Langfuse channel's `uploadUrl: null` (#9): only a
// project that already holds the bytes skips the upload, because a hash any
// project could name would otherwise be a way to adopt another's picture.
func (s *Store) MediaHeld(ctx context.Context, projectID, sha string) (*MediaInfo, error) {
	return projectHold(ctx, s.db, projectID, sha)
}

// MediaByLangfuseID resolves the SDK's id to a body this project holds (#9):
// the project's holders in the hash range the id's first sixteen bytes spell,
// then the whole id compared (Decision 26).
func (s *Store) MediaByLangfuseID(ctx context.Context, projectID, mediaID string) (*MediaInfo, error) {
	prefix, ok := shaPrefixOf(mediaID)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, mediaByID, projectID, prefix, prefix+"g")
	if err != nil {
		return nil, fmt.Errorf("look up media %s: %w", mediaID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var info MediaInfo
		var size sql.NullInt64
		if err := rows.Scan(&info.SHA256, &info.MimeType, &info.CreatedAt, &size); err != nil {
			return nil, err
		}
		if size.Valid && MediaIDFor(info.SHA256) == mediaID {
			info.Size = size.Int64
			return &info, nil
		}
	}
	return nil, rows.Err()
}

// langfuseMarker is the Langfuse reference string's opening, looked for in a
// payload's bytes before anything is decoded for it.
var langfuseMarker = []byte(mapping.LangfuseMarker)

// mediaReads resolves, for one read, the Langfuse reference strings a
// trace's upload has since caught up with (Decision 21). The bodies a trace's
// refs hold are asked for once per trace, however many of its payloads carry
// a string.
type mediaReads struct {
	held map[[2]string]map[string]MediaInfo
}

// resolve answers a payload with each Langfuse reference string the trace
// now has a body for read as the reference: the bodies the trace's refs name,
// keyed by the SDK's id for them. The stored payload is not rewritten, and
// the raw archive keeps the string as sent.
func (m *mediaReads) resolve(ctx context.Context, s *Store, v any, projectID, traceID string) (any, error) {
	key := [2]string{projectID, traceID}
	held, asked := m.held[key]
	if !asked {
		var err error
		if held, err = s.traceMediaIDs(ctx, projectID, traceID); err != nil {
			return nil, err
		}
		if m.held == nil {
			m.held = map[[2]string]map[string]MediaInfo{}
		}
		m.held[key] = held
	}
	if len(held) == 0 {
		return v, nil
	}
	out, _ := mapping.ResolveLangfuseMedia(v, func(id string) (string, int64, bool) {
		info, ok := held[id]
		return info.SHA256, info.Size, ok
	})
	return out, nil
}

// traceMediaIDs lists the bodies a trace's refs name, by their Langfuse id.
func (s *Store) traceMediaIDs(ctx context.Context, projectID, traceID string) (map[string]MediaInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.sha256, m.size FROM media_refs r JOIN media m ON m.sha256 = r.sha256
		  WHERE r.project_id = ? AND r.trace_id = ?`, projectID, traceID)
	if err != nil {
		return nil, fmt.Errorf("read the trace's media: %w", err)
	}
	defer rows.Close()
	held := map[string]MediaInfo{}
	for rows.Next() {
		var info MediaInfo
		if err := rows.Scan(&info.SHA256, &info.Size); err != nil {
			return nil, err
		}
		held[MediaIDFor(info.SHA256)] = info
	}
	return held, rows.Err()
}

// MediaSummary is the system endpoint's media figure (#11): the bodies a
// project points at and their decoded bytes. A body two projects share is in
// both figures, because each would keep it alive alone.
type MediaSummary struct {
	Count int64
	Bytes int64
}

// MediaSummary counts what a project's refs hold: its holds, one per body it
// points at from a trace or a raw batch (Decisions 25, 26), read through
// `idx_media_holders_project` and asked the same predicate as every read.
func (s *Store) MediaSummary(ctx context.Context, projectID string) (MediaSummary, error) {
	var summary MediaSummary
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(m.size), 0)
		   FROM media_holders h JOIN media m ON m.sha256 = h.sha256
		  WHERE h.project_id = ? AND `+holdsBody,
		projectID).Scan(&summary.Count, &summary.Bytes)
	if err != nil {
		return summary, fmt.Errorf("summarise media: %w", err)
	}
	return summary, nil
}

// mediaFreed counts the bodies a deletion would make the project stop holding
// (#11 as Decision 27 has it): those some of the project's traces or raw
// batches going point at, and none of its refs staying does. Whether another
// project keeps the bytes on disk does not enter the figure. `traces` selects
// trace ids of the project and `raws` raw batch ids of it; either may be
// empty, which is none.
func (s *Store) mediaFreed(ctx context.Context, projectID, traces string, traceArgs []any, raws string, rawArgs []any) (count, size int64, err error) {
	if traces == "" {
		traces = `SELECT NULL WHERE 0`
	}
	if raws == "" {
		raws = `SELECT NULL WHERE 0`
	}
	args := append(append(append([]any{}, traceArgs...), rawArgs...),
		projectID, projectID, projectID, projectID)
	err = s.db.QueryRowContext(ctx,
		`WITH gone(trace_id) AS (`+traces+`), gone_raw(id) AS (`+raws+`)
		 SELECT COUNT(*), COALESCE(SUM(m.size), 0) FROM media m
		  WHERE m.sha256 IN (SELECT sha256 FROM media_refs WHERE project_id = ? AND trace_id IN gone
		                     UNION
		                     SELECT sha256 FROM media_raw_refs WHERE project_id = ? AND raw_batch_id IN gone_raw)
		    AND NOT EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = m.sha256
		                     AND r.project_id = ? AND r.trace_id NOT IN gone)
		    AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = m.sha256
		                     AND rr.project_id = ? AND rr.raw_batch_id NOT IN gone_raw)`,
		args...).Scan(&count, &size)
	if err != nil {
		return 0, 0, fmt.Errorf("count the media a deletion releases: %w", err)
	}
	return count, size, nil
}

// MediaRefAdd records that a trace points at a body the project already holds
// — the Langfuse channel's answer to a second identical upload, which asks
// for no bytes (#9). It reports whether the project still held the body to
// point at: asked again inside the write (Decision 26), because a body whose
// last ref in this project went since the handler asked may be kept alive by
// another project's, and pointing at it then would be a hold without the
// bytes.
//
// For a trace the project has, the ref is settled. For one not here yet it is
// pending, as the upload's would be, and keeps the body until the trace's
// spans settle it (#30). It is dated as the bytes are in the project — the
// newest of the project's refs to the body, or the hold's first instant when
// there is none — but no earlier than an hour inside the grace, so that it
// outlives the next sweep: naming a hash again, for any trace, extends a body
// no trace claims by an hour at most, never by the grace. A new pending ref
// counts toward the cap like the upload's (#31). A trace deleted or erased
// within the hour gets no ref, as its upload would get none (#29).
type MediaRefAdd struct {
	ProjectID string
	SHA256    string
	TraceID   string
	Now       int64

	// Held is whether the project still held the body inside the write:
	// false when it was collected since the handler asked, and the bytes
	// have to be asked for.
	Held bool
}

// nullAnswerHold is how long a pending ref the null answer writes keeps its
// body at least (#30): longer than the sweep's hour, far longer than the
// seconds the spans take.
const nullAnswerHold = time.Hour

func (a *MediaRefAdd) apply(tx *sql.Tx) error {
	a.Held = false
	hold, err := projectHold(context.Background(), tx, a.ProjectID, a.SHA256)
	if err != nil || hold == nil {
		return err
	}
	place, err := refPlaceOf(context.Background(), tx, a.ProjectID, a.SHA256, a.TraceID)
	if err != nil {
		return err
	}
	now := nowOr(a.Now)
	// No sooner than an hour inside the grace (#30): the spans the null
	// answer tells the SDK to send without their bytes are on their way.
	floor := now - int64(MediaOrphanGrace) + int64(nullAnswerHold)
	// A trace removed since the handler's check is refused as the check
	// would have refused it, not answered as held (#29), and a new pending
	// ref meets the cap.
	if err := placeRefusal(context.Background(), tx, a.ProjectID, place); err != nil {
		return err
	}
	if place == refWritten {
		// Named again for its own trace, a ref still pending takes the same
		// hour a new one would: a ref near the end of its grace would
		// otherwise go, with the body, right after the answer.
		if _, err := tx.Exec(`UPDATE media_refs SET created_at = MAX(created_at, ?)
		                       WHERE sha256 = ? AND project_id = ? AND trace_id = ? AND pending = 1`,
			floor, a.SHA256, a.ProjectID, a.TraceID); err != nil {
			return fmt.Errorf("hold media ref %s: %w", a.SHA256, err)
		}
		a.Held = true
		return nil
	}
	at := now
	if place == refPending {
		var aged int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(created_at), ?) FROM media_refs
		                        WHERE sha256 = ? AND project_id = ?`,
			hold.CreatedAt, a.SHA256, a.ProjectID).Scan(&aged); err != nil {
			return fmt.Errorf("read the age of media %s: %w", a.SHA256, err)
		}
		at = min(now, max(aged, floor))
	}
	a.Held, err = writeChannelRef(tx, a.ProjectID, a.SHA256, a.TraceID, hold.MimeType, at)
	return err
}

// traceStoredExpr is whether a project has a trace, the one test of it the
// channel's refs use: a stored trace settles a ref (Decision 13) and keeps it
// out of the cap (#31). Its arguments are the project and the trace.
const traceStoredExpr = `EXISTS (SELECT 1 FROM traces WHERE project_id = ? AND id = ?)`

// refExistsExpr is whether a ref is already written; its arguments are the
// hash, the project and the trace.
const refExistsExpr = `EXISTS (SELECT 1 FROM media_refs WHERE sha256 = ? AND project_id = ? AND trace_id = ?)`

// traceVoidedExpr is whether a deletion or an erasure removed a trace within
// the upload URL's lifetime and the slack (#29); its arguments are the
// project, the trace and voidedSince. Bounded by the row's age, not by the
// sweep that forgets it: how often that runs decides only how long the row
// is kept.
const traceVoidedExpr = `EXISTS (SELECT 1 FROM media_voided WHERE project_id = ? AND trace_id = ? AND at >= ?)`

// voidedSince is the oldest removal that still voids an upload: on the wall
// clock, as the removal was stamped.
func voidedSince() int64 {
	return time.Now().Add(-(MediaUploadWindow + mediaVoidedSlack)).UnixNano()
}

// refPlace is where a channel ref to a body for a trace would stand — the one
// rule the ask, the upload's checks and the null answer share.
type refPlace int

const (
	// refWritten: the ref is there already; writing it again adds nothing.
	refWritten refPlace = iota
	// refSettled: the trace is stored, so the ref is settled (Decision 13)
	// and outside the cap (#31) — a trace sent again after its deletion
	// too, which is seen and deletable like any other.
	refSettled
	// refVoid: a deletion or an erasure removed the trace within the hour
	// and it is not here now; nothing is written for it (#29).
	refVoid
	// refPending: a new pending ref, which the cap counts (#31).
	refPending
)

// refFactsExpr selects the three facts that place a ref, with refFactsArgs.
const refFactsExpr = traceStoredExpr + `, ` + refExistsExpr + `, ` + traceVoidedExpr

func refFactsArgs(projectID, sha, traceID string) []any {
	return []any{projectID, traceID, sha, projectID, traceID, projectID, traceID, voidedSince()}
}

func placeOf(stored, written, voided bool) refPlace {
	switch {
	case written:
		return refWritten
	case stored:
		return refSettled
	case voided:
		return refVoid
	default:
		return refPending
	}
}

// refPlaceOf reads the facts and places the ref.
func refPlaceOf(ctx context.Context, q ctxQuerier, projectID, sha, traceID string) (refPlace, error) {
	var stored, written, voided bool
	if err := q.QueryRowContext(ctx, `SELECT `+refFactsExpr, refFactsArgs(projectID, sha, traceID)...).
		Scan(&stored, &written, &voided); err != nil {
		return 0, fmt.Errorf("look up the ref of media %s: %w", sha, err)
	}
	return placeOf(stored, written, voided), nil
}

// MediaUploadWindow is how long an upload URL is good for, and so how long a
// removed trace's uploads stay void: no URL issued before the removal outlives
// it (#29), and none is issued after it.
const MediaUploadWindow = time.Hour

// mediaVoidedSlack is how much longer than the window a removed trace is
// remembered: a URL's expiry is signed in whole seconds from a clock read
// before the ask's check, and a removal is stamped as its chunk ends, not as
// it commits — the rest of a group commit comes after — so a URL the check
// let through can outlive the stamp's hour by that much (#29). Five minutes,
// far past any commit a writer finishes, for a trace sent again that waits
// that much longer for its pictures.
const mediaVoidedSlack = 5 * time.Minute

// voidUploads records, in the transaction that removes them and before they
// go, the traces whose uploads are void from now (#29).
func voidUploads(tx *sql.Tx, projectID string, traceIDs []any, now int64) error {
	return eachIn(traceIDs, func(batch []any) error {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO media_voided (project_id, trace_id, at)
		    SELECT project_id, id, ? FROM traces WHERE project_id = ? AND id IN (`+placeholders(len(batch))+`)`,
			append([]any{now, projectID}, batch...)...); err != nil {
			return fmt.Errorf("void the removed traces' uploads: %w", err)
		}
		return nil
	})
}

// restampVoided moves the rows a removal wrote as its chunk began to now, as
// the chunk ends: a URL whose ask read the pool while the chunk ran is dated
// from then, and the row's hour has to start no earlier (#29).
func restampVoided(tx *sql.Tx, projectID string, stamp int64) error {
	if _, err := tx.Exec(`UPDATE media_voided SET at = ? WHERE project_id = ? AND at = ?`,
		time.Now().UnixNano(), projectID, stamp); err != nil {
		return fmt.Errorf("stamp the removed traces' uploads: %w", err)
	}
	return nil
}

// MaxPendingMediaRefs is how many pending refs one project may have (#31): an
// upload whose trace has not come. Two orders of magnitude over a hundred
// pictures a second held for the seconds an export takes. A constant, not a
// setting.
const MaxPendingMediaRefs = 10000

// maxPendingMediaRefs is the cap the reads and the jobs count against:
// MaxPendingMediaRefs, lowered only by this package's tests. It is shared, so
// a test that changes it must not run in parallel (t.Parallel) with another
// that uploads.
var maxPendingMediaRefs = MaxPendingMediaRefs

// placeRefusal is the refusal a ref's place leads to — the one rule the ask,
// the upload and the null answer share: ErrTraceRemoved for a removed trace
// that is not here (#29), errPendingFull for a new pending ref past the cap
// (#31), and nil for a ref written already or a trace the project has.
func placeRefusal(ctx context.Context, q querier, projectID string, place refPlace) error {
	switch place {
	case refVoid:
		return ErrTraceRemoved
	case refPending:
		return pendingRoom(ctx, q, projectID, maxPendingMediaRefs)
	}
	return nil
}

// errPendingFull is the refusal at the cap (#31).
var errPendingFull = &Rejection{Kind: RejectFull,
	Message: "too many media uploads are waiting for their traces"}

// pendingRoom refuses one more pending ref when the project already has
// `limit` of them: a seek on the partial index of 0026, which stops counting
// there.
func pendingRoom(ctx context.Context, q querier, projectID string, limit int) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT 1 FROM media_refs
	                       WHERE project_id = ? AND pending = 1 LIMIT ?)`,
		projectID, limit).Scan(&n); err != nil {
		return fmt.Errorf("count the pending media refs: %w", err)
	}
	if n >= limit {
		return errPendingFull
	}
	return nil
}

// MediaUploadRoom is the channel POST's check, a read of the pool before any
// job: ErrTraceRemoved for a trace a deletion or an erasure removed within the
// hour and not here now (#29) — a URL issued for it would outlive the
// removal's record — and errPendingFull's rejection when the ref its answer
// would lead to, the upload's or the null answer's, would be a new pending
// one past the cap (#31). The guard has settled the project and the key.
func (s *Store) MediaUploadRoom(ctx context.Context, projectID, sha, traceID string) error {
	place, err := refPlaceOf(ctx, s.db, projectID, sha, traceID)
	if err != nil {
		return err
	}
	return placeRefusal(ctx, s.db, projectID, place)
}

// MediaGrant is what an upload URL lets its holder store, as far as the store
// checks it: for which project, trace and body, and which key asked for it
// (#28).
type MediaGrant struct {
	ProjectID string
	TraceID   string
	SHA256    string
	Key       string
}

// ErrUploadVoid is the refusal of an upload whose grant no longer stands, and
// the channel's one answer to a URL it will not take.
var ErrUploadVoid = &Rejection{Kind: RejectForbidden, Message: "this upload URL is not valid; ask for a new one"}

// ErrTraceRemoved is the refusal of the ask, and of the upload, for a trace a
// deletion or an erasure removed within the hour and that is not here (#29):
// no new URL would be taken either.
var ErrTraceRemoved = &Rejection{Kind: RejectForbidden,
	Message: "this trace was deleted or erased within the hour; its media is not stored"}

// MediaGrantRefusal checks a grant before its body is read, as the write will
// check it again: nil when it would be stored now, ErrUploadVoid when the
// project is gone or the key that asked is no longer the project's (#28),
// ErrTraceRemoved when its trace was deleted or erased within the hour (#29),
// and the full rejection
// when its ref would be a new pending one past the cap (#31). It also answers
// the project's media setting, under which the upload keeps nothing and the
// cap does not apply. One statement, and the count only for a ref that would
// be pending.
func (s *Store) MediaGrantRefusal(ctx context.Context, g MediaGrant) (media string, refusal error) {
	return grantRefusal(ctx, s.db, g)
}

// ctxQuerier is a querier that also takes a context: the pool or the write's
// transaction.
type ctxQuerier interface {
	querier
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func grantRefusal(ctx context.Context, q ctxQuerier, g MediaGrant) (string, error) {
	var (
		media                   string
		gone, alive             bool
		stored, written, voided bool
	)
	args := append([]any{g.Key}, refFactsArgs(g.ProjectID, g.SHA256, g.TraceID)...)
	err := q.QueryRowContext(ctx, `SELECT p.media, p.deleted_at IS NOT NULL,
	        EXISTS (SELECT 1 FROM api_keys k WHERE k.public_key = ? AND k.project_id = p.id),
	        `+refFactsExpr+`
	   FROM projects p WHERE p.id = ?`, append(args, g.ProjectID)...).
		Scan(&media, &gone, &alive, &stored, &written, &voided)
	if err == sql.ErrNoRows {
		return "", ErrUploadVoid
	}
	if err != nil {
		return "", fmt.Errorf("check an upload grant: %w", err)
	}
	place := placeOf(stored, written, voided)
	if gone || !alive {
		return media, ErrUploadVoid
	}
	// The placeholder setting keeps nothing, so the cap does not apply; a
	// removed trace is refused under either setting.
	if media == MediaPlaceholder && place != refVoid {
		return media, nil
	}
	return media, placeRefusal(ctx, q, g.ProjectID, place)
}

// MediaUpload stores one body the Langfuse channel received and the ref of
// the trace it was sent for (#9), and the project's hold under the type the
// upload declared (Decision 25).
//
// The upload's grant is checked again here, because the handler's checks ran
// before the body and a revocation, a deletion or a burst of uploads can land
// in between (#28, #29, #31). A deletion's own transaction drops the refs of
// the traces it removes and voids their uploads, so an upload is either in
// before it — and dropped with the trace — or refused after it. A project
// switched to the placeholder setting since keeps nothing (#6).
type MediaUpload struct {
	Grant MediaGrant
	Body  MediaBody
	Now   int64
}

func (u *MediaUpload) apply(tx *sql.Tx) error {
	media, err := grantRefusal(context.Background(), tx, u.Grant)
	if err != nil || media == MediaPlaceholder {
		return err
	}
	now := nowOr(u.Now)
	if err := writeMediaBodies(tx, []MediaBody{u.Body}, now); err != nil {
		return err
	}
	_, err = writeChannelRef(tx, u.Grant.ProjectID, u.Body.SHA256, u.Grant.TraceID, u.Body.MimeType, now)
	return err
}

// mediaVoidedSweep forgets the removed traces whose uploads no URL can still
// carry (#29): those removed more than the URL's lifetime, and the slack,
// ago.
type mediaVoidedSweep struct {
	Before int64
	Limit  int

	Removed int64
}

func (m *mediaVoidedSweep) apply(tx *sql.Tx) error {
	result, err := tx.Exec(`DELETE FROM media_voided WHERE (project_id, trace_id) IN
	    (SELECT project_id, trace_id FROM media_voided WHERE at < ? LIMIT ?)`, m.Before, m.Limit)
	if err != nil {
		return fmt.Errorf("forget voided uploads: %w", err)
	}
	m.Removed, err = result.RowsAffected()
	return err
}

func nowOr(now int64) int64 {
	if now == 0 {
		return time.Now().UnixNano()
	}
	return now
}

// orphanMediaRefs finds the Langfuse channel's refs still pending past the
// grace (Decision 13) — a seek on the partial index, which holds only those.
// Read outside the writer like the orphaned payloads are; the job decides each
// one inside its transaction.
func (s *Store) orphanMediaRefs(ctx context.Context, before int64, limit int) ([]MediaOrphan, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sha256, project_id, trace_id FROM media_refs
		  WHERE pending = 1 AND created_at < ?
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

// orphanMedia finds bodies no ref names at all — what only a hand-edited
// database would leave, since every write and deletion of refs is one
// transaction. A belt that cheap to wear has to stay cheap: each pass reads
// one page of the primary key after the cursor and checks those, and the
// next pass goes on from where this one stopped, wrapping at the end. It
// answers the orphans and the cursor to start from next time.
func (s *Store) orphanMedia(ctx context.Context, after string, page int) ([]any, string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT sha256,
		        NOT EXISTS (SELECT 1 FROM media_refs r WHERE r.sha256 = m.sha256)
		    AND NOT EXISTS (SELECT 1 FROM media_raw_refs rr WHERE rr.sha256 = m.sha256)
		   FROM media m WHERE sha256 > ? ORDER BY sha256 LIMIT ?`, after, page)
	if err != nil {
		return nil, after, fmt.Errorf("find orphaned media: %w", err)
	}
	defer rows.Close()
	var (
		orphans []any
		last    string
		read    int
	)
	for rows.Next() {
		var orphan bool
		if err := rows.Scan(&last, &orphan); err != nil {
			return nil, after, err
		}
		read++
		if orphan {
			orphans = append(orphans, last)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, after, err
	}
	if read < page {
		last = ""
	}
	return orphans, last, nil
}

// MediaOrphan is one pending ref past the grace.
type MediaOrphan struct {
	SHA256    string
	ProjectID string
	TraceID   string
}

// mediaSweep settles the pending refs the read pass found: one whose trace
// never came, and that is still past the grace, is deleted — a null answer
// since the read may have given it an hour (#30), and it is kept; one whose
// trace is stored is kept and no longer pending, a belt for a database edited
// by hand, since a trace's insert settles its refs (#31). It releases the
// holds the deleted refs leave and any hold the pass found with no ref behind
// it, restores the holds the pass found missing behind a ref, and then
// collects the bodies left with no ref, together with any body the pass found
// with no ref at all.
type mediaSweep struct {
	Refs   []MediaOrphan
	Bodies []any
	// Stale are holder rows the pass found with no ref of their project,
	// and Missing the pairs with a ref and no holder row.
	Stale, Missing []MediaHold
	Now            int64
	// Before is the grace's edge the read pass used.
	Before int64

	// Dropped counts the refs deleted, Released and Restored the holds
	// deleted and written, and Deleted the bodies collected — each what
	// this transaction did, not what the read pass found.
	Dropped, Released, Restored, Deleted int64
}

func (m *mediaSweep) apply(tx *sql.Tx) error {
	m.Dropped, m.Released, m.Restored, m.Deleted = 0, 0, 0, 0
	var shas []any
	released := map[string][]any{}
	for _, ref := range m.Refs {
		result, err := tx.Exec(
			`DELETE FROM media_refs WHERE sha256 = ? AND project_id = ? AND trace_id = ? AND pending = 1
			   AND created_at < ? AND NOT `+traceStoredExpr,
			ref.SHA256, ref.ProjectID, ref.TraceID, m.Before, ref.ProjectID, ref.TraceID)
		if err != nil {
			return fmt.Errorf("delete orphaned media ref: %w", err)
		}
		if n, err := result.RowsAffected(); err == nil && n > 0 {
			m.Dropped += n
			shas = append(shas, ref.SHA256)
			released[ref.ProjectID] = append(released[ref.ProjectID], ref.SHA256)
			continue
		}
		if _, err := tx.Exec(
			`UPDATE media_refs SET pending = 0 WHERE sha256 = ? AND project_id = ? AND trace_id = ?
			   AND pending = 1 AND `+traceStoredExpr,
			ref.SHA256, ref.ProjectID, ref.TraceID, ref.ProjectID, ref.TraceID); err != nil {
			return fmt.Errorf("settle media ref: %w", err)
		}
	}
	for _, hold := range m.Stale {
		released[hold.ProjectID] = append(released[hold.ProjectID], hold.SHA256)
		shas = append(shas, hold.SHA256)
	}
	// One release per project; each re-checks, in this transaction, that
	// no ref of the project is left.
	for projectID, held := range released {
		n, _, err := releaseMedia(tx, projectID, held)
		if err != nil {
			return err
		}
		m.Released += n
	}
	// A ref with no hold behind it: the type the project declared is not
	// kept anywhere else, so the hold is octet-stream — never another
	// project's type — from now.
	for _, hold := range m.Missing {
		result, err := tx.Exec(
			`INSERT INTO media_holders (sha256, project_id, mime_type, first_at)
			 SELECT ?, ?, 'application/octet-stream', ?
			  WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			    AND (EXISTS (SELECT 1 FROM media_refs WHERE sha256 = ? AND project_id = ?)
			         OR EXISTS (SELECT 1 FROM media_raw_refs WHERE sha256 = ? AND project_id = ?))
			 ON CONFLICT DO NOTHING`,
			hold.SHA256, hold.ProjectID, nowOr(m.Now), hold.SHA256,
			hold.SHA256, hold.ProjectID, hold.SHA256, hold.ProjectID)
		if err != nil {
			return fmt.Errorf("restore media hold: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("restore media hold: %w", err)
		}
		m.Restored += n
	}
	shas = append(shas, m.Bodies...)
	count, _, err := collectMedia(tx, shas)
	m.Deleted = count
	return err
}

// MediaHold is one project's holder row of one body.
type MediaHold struct {
	SHA256    string
	ProjectID string
}

// holdDrift finds, among the bodies in (after, upto] — the page the look for
// bodies with no ref just read; an empty upto is the end of the table — the
// holder rows with no ref of their project behind them, and the pairs with a
// ref and no holder row. Every write and deletion keeps the two in step in
// one transaction, so only a hand-edited database leaves either: the reads
// refuse a hold that is not both (Decision 26), and the sweep repairs it so
// that it does not stay.
func (s *Store) holdDrift(ctx context.Context, after, upto string) (stale, missing []MediaHold, err error) {
	within := `sha256 > ?`
	args := []any{after}
	if upto != "" {
		within += ` AND sha256 <= ?`
		args = append(args, upto)
	}
	if stale, err = s.holdPairs(ctx, `SELECT h.sha256, h.project_id FROM media_holders h
		  WHERE h.`+within+` AND NOT `+holdsBody, args...); err != nil {
		return nil, nil, fmt.Errorf("find stale media holds: %w", err)
	}
	if missing, err = s.holdPairs(ctx, `SELECT x.sha256, x.project_id FROM (
		    SELECT sha256, project_id FROM media_refs WHERE `+within+`
		    UNION SELECT sha256, project_id FROM media_raw_refs WHERE `+within+`) x
		  WHERE NOT EXISTS (SELECT 1 FROM media_holders h
		                     WHERE h.sha256 = x.sha256 AND h.project_id = x.project_id)`,
		append(append([]any{}, args...), args...)...); err != nil {
		return nil, nil, fmt.Errorf("find missing media holds: %w", err)
	}
	return stale, missing, nil
}

func (s *Store) holdPairs(ctx context.Context, query string, args ...any) ([]MediaHold, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaHold
	for rows.Next() {
		var hold MediaHold
		if err := rows.Scan(&hold.SHA256, &hold.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, hold)
	}
	return out, rows.Err()
}

// MediaUploadKey is the key the Langfuse channel signs its upload URLs with
// (Decision 22): the same on every start of the server over this database.
func (s *Store) MediaUploadKey() []byte { return s.mediaUploadKey }

// serverKey reads a named key, minting it first when there is none. Written
// at open, before the writer runs, like the migrations; a second process
// racing the first start keeps whichever key was written first.
func (s *Store) serverKey(name string) ([]byte, error) {
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("mint the %s key: %w", name, err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO server_keys (name, key, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		name, fresh, time.Now().UnixNano()); err != nil {
		return nil, fmt.Errorf("store the %s key: %w", name, err)
	}
	var key []byte
	if err := s.db.QueryRow(`SELECT key FROM server_keys WHERE name = ?`, name).Scan(&key); err != nil {
		return nil, fmt.Errorf("read the %s key: %w", name, err)
	}
	return key, nil
}

// CheckMediaSetting is the one rule for a project's media setting (#6),
// asked by the handler before a dry run and by the update inside its write.
func CheckMediaSetting(setting string) error {
	if setting != MediaStore && setting != MediaPlaceholder {
		return fmt.Errorf("media must be %q or %q, got %q", MediaStore, MediaPlaceholder, setting)
	}
	return nil
}

// ValidMediaSHA reports a lower-case hex SHA-256, the only spelling a media
// path accepts.
func ValidMediaSHA(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// dropProjectMedia deletes every ref and hold a purged project still has and
// collects the bodies only it pointed at (spec 041 #3). A body another project
// also points at survives: the refs carry the project for exactly this. The
// raw refs went with the project's batches by cascade; its holder rows name
// those bodies too.
func dropProjectMedia(tx *sql.Tx, projectID string) error {
	shas, err := queryColumn[string](tx,
		`SELECT sha256 FROM media_refs WHERE project_id = ?
		 UNION SELECT sha256 FROM media_holders WHERE project_id = ?`, projectID, projectID)
	if err != nil {
		return fmt.Errorf("find a project's media: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM media_refs WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("delete a project's media refs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM media_holders WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("delete a project's media holds: %w", err)
	}
	_, _, err = collectMedia(tx, shas)
	return err
}
