package store

import (
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
// collected since. Ingest writes a ref with its trace, so the ref is settled,
// and one the Langfuse channel left pending is settled by it. The project's
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
			 SELECT ?, ?, ?, ?, 0 WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
			 ON CONFLICT (sha256, project_id, trace_id) DO UPDATE SET pending = 0`,
			ref.SHA256, projectID, ref.TraceID, now, ref.SHA256)
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
		 SELECT ?, ?, ?, ?, NOT EXISTS (SELECT 1 FROM traces WHERE project_id = ? AND id = ?)
		  WHERE EXISTS (SELECT 1 FROM media WHERE sha256 = ?)
		 ON CONFLICT DO NOTHING`,
		sha, projectID, traceID, now, projectID, traceID, sha); err != nil {
		return false, fmt.Errorf("add media ref: %w", err)
	}
	var one int
	err := tx.QueryRow(`SELECT 1 FROM media_refs WHERE sha256 = ? AND project_id = ? AND trace_id = ?`,
		sha, projectID, traceID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
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
		hold, err := projectHold(tx, projectID, sha)
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
func projectHold(q querier, projectID, sha string) (*MediaInfo, error) {
	info := MediaInfo{SHA256: sha}
	var size sql.NullInt64
	err := q.QueryRow(mediaScope, sha, projectID).Scan(&info.MimeType, &info.CreatedAt, &size)
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
func (s *Store) MediaFor(projectID, sha string) (*MediaFile, error) {
	var file MediaFile
	err := s.db.QueryRow(mediaRead, sha, projectID).Scan(&file.MimeType, &file.Body)
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
func (s *Store) MediaHeld(projectID, sha string) (*MediaInfo, error) {
	return projectHold(s.db, projectID, sha)
}

// MediaByLangfuseID resolves the SDK's id to a body this project holds (#9):
// the project's holders in the hash range the id's first sixteen bytes spell,
// then the whole id compared (Decision 26).
func (s *Store) MediaByLangfuseID(projectID, mediaID string) (*MediaInfo, error) {
	prefix, ok := shaPrefixOf(mediaID)
	if !ok {
		return nil, nil
	}
	rows, err := s.db.Query(mediaByID, projectID, prefix, prefix+"g")
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
func (m *mediaReads) resolve(s *Store, v any, projectID, traceID string) (any, error) {
	key := [2]string{projectID, traceID}
	held, asked := m.held[key]
	if !asked {
		var err error
		if held, err = s.traceMediaIDs(projectID, traceID); err != nil {
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
func (s *Store) traceMediaIDs(projectID, traceID string) (map[string]MediaInfo, error) {
	rows, err := s.db.Query(
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
func (s *Store) MediaSummary(projectID string) (MediaSummary, error) {
	var summary MediaSummary
	err := s.db.QueryRow(
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
func (s *Store) mediaFreed(projectID, traces string, traceArgs []any, raws string, rawArgs []any) (count, size int64, err error) {
	if traces == "" {
		traces = `SELECT NULL WHERE 0`
	}
	if raws == "" {
		raws = `SELECT NULL WHERE 0`
	}
	args := append(append(append([]any{}, traceArgs...), rawArgs...),
		projectID, projectID, projectID, projectID)
	err = s.db.QueryRow(
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
// Only for a trace the project has (#30): the ref is then settled. For a trace
// not here yet it writes nothing — its spans will carry the SDK's string, which
// ingest resolves to the body and writes the ref for then. A pending ref comes
// only from delivered bytes, so naming a hash again for a new trace every day
// cannot keep a body no trace claims alive.
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

func (a *MediaRefAdd) apply(tx *sql.Tx) error {
	a.Held = false
	hold, err := projectHold(tx, a.ProjectID, a.SHA256)
	if err != nil || hold == nil {
		return err
	}
	stored, err := traceStored(tx, a.ProjectID, a.TraceID)
	if err != nil {
		return err
	}
	if !stored {
		a.Held = true
		return nil
	}
	a.Held, err = writeChannelRef(tx, a.ProjectID, a.SHA256, a.TraceID, hold.MimeType, nowOr(a.Now))
	return err
}

// traceStored reports whether a project has a trace.
func traceStored(q querier, projectID, traceID string) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM traces WHERE project_id = ? AND id = ?`, projectID, traceID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// TraceStored reports whether a project has a trace, for the upload channel's
// checks before a body is read (#31).
func (s *Store) TraceStored(projectID, traceID string) (bool, error) {
	return traceStored(s.db, projectID, traceID)
}

// MediaRefStates counts one trace's media refs by state: pending, until its
// spans arrive, and settled.
func (s *Store) MediaRefStates(projectID, traceID string) (pending, settled int, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(pending), 0), COALESCE(SUM(1 - pending), 0)
	   FROM media_refs WHERE project_id = ? AND trace_id = ?`, projectID, traceID).Scan(&pending, &settled)
	return pending, settled, err
}

// MaxPendingMediaRefs is how many pending refs one project may have (#31): an
// upload whose trace has not come. Two orders of magnitude over a hundred
// pictures a second held for the seconds an export takes. A constant, not a
// setting.
const MaxPendingMediaRefs = 10000

// SetMaxPendingMediaRefs lowers the cap, for a test that has to reach it.
func (s *Store) SetMaxPendingMediaRefs(n int) { s.maxPendingMediaRefs = n }

// MaxPendingMediaRefsOf is the cap this store enforces.
func (s *Store) MaxPendingMediaRefsOf() int { return s.maxPendingMediaRefs }

// pendingFull reports whether a project has `limit` pending refs already: a
// seek on the partial index of 0025, which stops counting at the cap.
func pendingFull(q querier, projectID string, limit int) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM media_refs
	                    WHERE project_id = ? AND pending = 1 LIMIT ?)`, projectID, limit).Scan(&n)
	return n >= limit, err
}

// MediaPendingFull reports whether a project is at its pending-ref cap.
func (s *Store) MediaPendingFull(projectID string) (bool, error) {
	return pendingFull(s.db, projectID, s.maxPendingMediaRefs)
}

// ErrPendingFull is what the channel answers at the cap (#31).
const ErrPendingFull = "too many media uploads are waiting for their traces"

// KeyOfProject reports whether a public key is still one of a project's keys:
// an upload URL dies with the key that asked for it (#28).
func (s *Store) KeyOfProject(publicKey, projectID string) (bool, error) {
	return keyOfProject(s.db, publicKey, projectID)
}

func keyOfProject(q querier, publicKey, projectID string) (bool, error) {
	var one int
	err := q.QueryRow(`SELECT 1 FROM api_keys WHERE public_key = ? AND project_id = ?`,
		publicKey, projectID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// MediaUpload stores one body the Langfuse channel received and the ref of
// the trace it was sent for (#9), and the project's hold under the type the
// upload declared (Decision 25).
//
// The upload's grant is checked again here, because the handler's checks ran
// before the body and a revocation, an erasure or a burst of uploads can land
// in between: the key that asked must still be the project's (#28), the grant
// must postdate the project's last trace deletion (#29), and a ref that would
// be pending must fit under the cap (#31).
type MediaUpload struct {
	ProjectID string
	TraceID   string
	Body      MediaBody
	Now       int64

	// Key is the public key that asked for the URL, and Issued when it did
	// (Unix nanoseconds).
	Key    string
	Issued int64
	// PendingCap is the project's cap on pending refs.
	PendingCap int
}

// errUploadVoid is the refusal of an upload whose grant no longer stands.
var errUploadVoid = &Rejection{Kind: RejectForbidden, Message: "this upload URL is not valid; ask for a new one"}

func (u *MediaUpload) apply(tx *sql.Tx) error {
	alive, err := keyOfProject(tx, u.Key, u.ProjectID)
	if err != nil {
		return err
	}
	if !alive {
		return errUploadVoid
	}
	var after int64
	if err := tx.QueryRow(`SELECT media_grants_after FROM projects WHERE id = ?`, u.ProjectID).
		Scan(&after); err != nil {
		return fmt.Errorf("read the upload watermark: %w", err)
	}
	if u.Issued <= after {
		return errUploadVoid
	}
	stored, err := traceStored(tx, u.ProjectID, u.TraceID)
	if err != nil {
		return err
	}
	if !stored {
		full, err := pendingFull(tx, u.ProjectID, u.PendingCap)
		if err != nil {
			return err
		}
		if full {
			return &Rejection{Kind: RejectFull, Message: ErrPendingFull}
		}
	}
	now := nowOr(u.Now)
	if err := writeMediaBodies(tx, []MediaBody{u.Body}, now); err != nil {
		return err
	}
	_, err = writeChannelRef(tx, u.ProjectID, u.Body.SHA256, u.TraceID, u.Body.MimeType, now)
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
func (s *Store) orphanMediaRefs(before int64, limit int) ([]MediaOrphan, error) {
	rows, err := s.db.Query(
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
func (s *Store) orphanMedia(after string, page int) ([]any, string, error) {
	rows, err := s.db.Query(
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
// has arrived since is kept and no longer pending, one whose trace never came
// is deleted. It releases the holds the deleted refs leave and any hold the
// pass found with no ref behind it, restores the holds the pass found missing
// behind a ref, and then collects the bodies left with no ref, together with
// any body the pass found with no ref at all.
type mediaSweep struct {
	Refs   []MediaOrphan
	Bodies []any
	// Stale are holder rows the pass found with no ref of their project,
	// and Missing the pairs with a ref and no holder row.
	Stale, Missing []MediaHold
	Now            int64

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
			   AND NOT EXISTS (SELECT 1 FROM traces WHERE project_id = ? AND id = ?)`,
			ref.SHA256, ref.ProjectID, ref.TraceID, ref.ProjectID, ref.TraceID)
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
			`UPDATE media_refs SET pending = 0 WHERE sha256 = ? AND project_id = ? AND trace_id = ?`,
			ref.SHA256, ref.ProjectID, ref.TraceID); err != nil {
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
func (s *Store) holdDrift(after, upto string) (stale, missing []MediaHold, err error) {
	within := `sha256 > ?`
	args := []any{after}
	if upto != "" {
		within += ` AND sha256 <= ?`
		args = append(args, upto)
	}
	if stale, err = s.holdPairs(`SELECT h.sha256, h.project_id FROM media_holders h
		  WHERE h.`+within+` AND NOT `+holdsBody, args...); err != nil {
		return nil, nil, fmt.Errorf("find stale media holds: %w", err)
	}
	if missing, err = s.holdPairs(`SELECT x.sha256, x.project_id FROM (
		    SELECT sha256, project_id FROM media_refs WHERE `+within+`
		    UNION SELECT sha256, project_id FROM media_raw_refs WHERE `+within+`) x
		  WHERE NOT EXISTS (SELECT 1 FROM media_holders h
		                     WHERE h.sha256 = x.sha256 AND h.project_id = x.project_id)`,
		append(append([]any{}, args...), args...)...); err != nil {
		return nil, nil, fmt.Errorf("find missing media holds: %w", err)
	}
	return stale, missing, nil
}

func (s *Store) holdPairs(query string, args ...any) ([]MediaHold, error) {
	rows, err := s.db.Query(query, args...)
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
