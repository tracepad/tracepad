package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// The archive as something other than the sweeper can read (spec 019). Every
// accepted export has been kept byte for byte since spec 002 #9; this is the
// reader that finally lets it leave — a listing in arrival order and one body
// at a time, both scoped to the project whose keys asked (#9).

// RawContentTypeProtobuf is what an absent `content_type` means: every row
// written before schema 0012 arrived under the one encoding the endpoint
// accepted (spec 019 #8).
const RawContentTypeProtobuf = "application/x-protobuf"

// RawBatchRow is one row of the archive's listing: what the row already holds,
// plus the size a client wants to know before it fetches (spec 019 #3).
type RawBatchRow struct {
	// Number is the batch's number within its project, 1, 2, 3… in the
	// order its batches were stored — the `id` the API shows. The table's
	// own rowid is one sequence for every tenant and never leaves the store
	// (spec 019 #17).
	Number          int64
	ReceivedAt      int64
	Dialect         string
	ContentType     string
	ContentEncoding string
	// SizeBytes is the *decoded* length — what a fetch of the body returns —
	// and not the zstd length the row occupies.
	SizeBytes int64
	// ScrubbedAt is when an erasure rewrote the batch without the erased
	// spans (spec 044 #2); nil is a batch as received.
	ScrubbedAt *int64
}

// RawCursor is the keyset of the last row of a page. The batch's number is the
// tiebreak, unique within the project: two batches of one millisecond are
// still two batches, and a page that could not tell them apart would repeat or
// skip one.
type RawCursor struct {
	ReceivedAt int64
	Number     int64
}

// RawFilter is one page of the archive.
type RawFilter struct {
	// Since and Until bound `received_at`, half-open: Since inclusive,
	// Until exclusive, so walking a window a day at a time never replays a
	// batch twice.
	Since *int64
	Until *int64
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After continues a previous page. Nil starts at whichever end Backward
	// names.
	After *RawCursor
	// Backward pages towards the *older* end. This is the one listing in
	// the API whose natural order is forward — a replay has to preserve
	// arrival order (spec 019 #3) — so `next` walks towards newer batches
	// and `prev` back towards older ones, the mirror of every other
	// listing. Rows still come back oldest first either way: the direction
	// is how the page was found, not how it is read.
	Backward bool
}

// rawConditions builds everything the filter says about *which* batches match,
// cursor excluded: the listing adds a keyset and a page, the count adds
// neither (the shape of spec 009 #4).
//
// Every condition is on raw_batch_numbers, aliased `n`: the numbers and the
// arrival times are there, in the index the listing pages on (spec 019 #17).
func rawConditions(projectID string, filter RawFilter) ([]string, []any) {
	where := []string{"n.project_id = ?"}
	args := []any{projectID}
	if filter.Since != nil {
		where = append(where, "n.received_at >= ?")
		args = append(args, *filter.Since)
	}
	if filter.Until != nil {
		where = append(where, "n.received_at < ?")
		args = append(args, *filter.Until)
	}
	return where, args
}

// rawQuery builds the listing statement and its arguments, as a function of
// its own so a test can hand the exact shipped SQL to EXPLAIN QUERY PLAN and
// assert the seek rather than a scan (the method of spec 003 #25).
//
// `substr(body, 1, 18)` rather than `body`: the decoded length lives in the
// zstd frame header for every body big enough for the number to matter, so a
// page of five hundred rows reads a few kilobytes instead of the archive.
func rawQuery(projectID string, filter RawFilter) (string, []any) {
	where, args := rawConditions(projectID, filter)
	// The comparison and the order flip together: they are the same
	// statement about which way the page is being read.
	comparison, order := ">", "ASC"
	if filter.Backward {
		comparison, order = "<", "DESC"
	}
	if filter.After != nil {
		// A row-value comparison rather than the equivalent disjunction:
		// SQLite seeks straight to the cursor with the former (spec 003
		// #25).
		where = append(where, "(n.received_at, n.number) "+comparison+" (?, ?)")
		args = append(args, filter.After.ReceivedAt, filter.After.Number)
	}
	args = append(args, filter.Limit)
	return `SELECT n.number, n.received_at, b.dialect, b.content_type, b.content_encoding, b.scrubbed_at,
	               substr(b.body, 1, ` + fmt.Sprint(zstdHeaderPrefix) + `)
	 FROM raw_batch_numbers n JOIN raw_batches b ON b.id = n.batch_id
	 WHERE ` + strings.Join(where, " AND ") + `
	 ORDER BY n.received_at ` + order + `, n.number ` + order + ` LIMIT ?`, args
}

// RawBatches lists a project's raw batches, oldest first.
func (s *Store) RawBatches(ctx context.Context, projectID string, filter RawFilter) ([]*RawBatchRow, error) {
	query, args := rawQuery(projectID, filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list raw batches: %w", err)
	}
	defer rows.Close()

	var out []*RawBatchRow
	var unsized []*RawBatchRow
	for rows.Next() {
		var (
			row                          RawBatchRow
			dialect, contentType, coding sql.NullString
			scrubbed                     sql.NullInt64
			prefix                       []byte
		)
		if err := rows.Scan(&row.Number, &row.ReceivedAt, &dialect, &contentType, &coding,
			&scrubbed, &prefix); err != nil {
			return nil, err
		}
		row.ScrubbedAt = nullableTime(scrubbed)
		row.Dialect = dialect.String
		row.ContentType = rawContentType(contentType)
		row.ContentEncoding = coding.String
		size, known := decodedSize(prefix)
		row.SizeBytes = size
		out = append(out, &row)
		if !known {
			unsized = append(unsized, &row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The frames whose header did not carry the length. zstd omits it only
	// where it is cheap to recover — a body of a few hundred bytes — so this
	// is a second read of the rows that cost the least to decompress, and
	// never of the ones that would have cost the most.
	for _, row := range unsized {
		size, err := s.rawBodySize(ctx, projectID, row.Number)
		if err != nil {
			return nil, err
		}
		row.SizeBytes = size
	}
	// A backward page arrives newest first, because that is the order the
	// index was read in. Every caller reads this listing oldest first.
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// CountRawBatches answers "how many match", stopping at `cap`, the way every
// other listing's count does (spec 009 #4).
func (s *Store) CountRawBatches(ctx context.Context, projectID string, filter RawFilter, cap int) (int, error) {
	where, args := rawConditions(projectID, filter)
	args = append(args, cap)
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (SELECT 1 FROM raw_batch_numbers n WHERE `+
			strings.Join(where, " AND ")+` LIMIT ?)`, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count raw batches: %w", err)
	}
	return count, nil
}

// RawBody is one archived body with what a replay needs to send it again.
type RawBody struct {
	// Number is the batch's number within its project (spec 019 #17).
	Number      int64
	ReceivedAt  int64
	Dialect     string
	ContentType string
	// ScrubbedAt is when an erasure rewrote the batch (spec 044 #2); nil is
	// a batch as received.
	ScrubbedAt *int64
	// Body is the decoded body: gzip was removed at ingest and zstd is
	// storage, so this is what the client actually posted.
	Body []byte
}

// RawBatchBody reads one batch by its number. It returns nil when the project
// has no batch of that number — never issued, or taken by the sweeper or an
// erasure. Another project's batch of the same number is that project's, and
// not reachable from here at all.
func (s *Store) RawBatchBody(ctx context.Context, projectID string, number int64) (*RawBody, error) {
	var (
		out                  RawBody
		dialect, contentType sql.NullString
		scrubbed             sql.NullInt64
		stored               []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT n.number, n.received_at, b.dialect, b.content_type, b.scrubbed_at, b.body
		   FROM raw_batch_numbers n JOIN raw_batches b ON b.id = n.batch_id
		  WHERE n.project_id = ? AND n.number = ?`, projectID, number).
		Scan(&out.Number, &out.ReceivedAt, &dialect, &contentType, &scrubbed, &stored)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read raw batch %d: %w", number, err)
	}
	out.Dialect = dialect.String
	out.ContentType = rawContentType(contentType)
	out.ScrubbedAt = nullableTime(scrubbed)
	body, err := Decompress(CompressionZstd, stored)
	if err != nil {
		return nil, fmt.Errorf("read raw batch %d: %w", number, err)
	}
	out.Body = body
	return &out, nil
}

// RawSummary is what `GET /api/v1/system` reports about the archive
// (spec 019 #4): its size, its window, and the honest edge of the promise —
// how many traces are older than the window and so cannot be replayed.
type RawSummary struct {
	Batches int64
	// StoredBytes is what the archive occupies on disk, compressed. It is
	// the number an operator moving `raw_retention_days` is deciding about,
	// which is why it is the stored length and not the decoded one the
	// listing's rows carry (spec 019 Decision 10).
	StoredBytes int64
	// Oldest and Newest are nil when the table holds nothing for the
	// project.
	Oldest *int64
	Newest *int64
	// TracesBeforeWindow counts the traces whose earliest span started
	// before the oldest batch arrived (spec 019 #1). It is a lower bound and
	// named as one: a late export of an old trace lands after the batch
	// line.
	TracesBeforeWindow int64
}

// RawSummary reads the archive's counters for one project.
func (s *Store) RawSummary(ctx context.Context, projectID string) (RawSummary, error) {
	var (
		out             RawSummary
		bytes           sql.NullInt64
		oldest, newest  sql.NullInt64
		beforeThreshold any
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), SUM(length(body)), MIN(received_at), MAX(received_at)
		   FROM raw_batches WHERE project_id = ?`, projectID).
		Scan(&out.Batches, &bytes, &oldest, &newest); err != nil {
		return out, fmt.Errorf("summarise raw batches: %w", err)
	}
	out.StoredBytes = bytes.Int64
	if oldest.Valid {
		value := oldest.Int64
		out.Oldest = &value
		beforeThreshold = value
	}
	if newest.Valid {
		value := newest.Int64
		out.Newest = &value
	}
	if beforeThreshold == nil {
		// No archive at all: every trace is before the window, which is
		// exactly what a project with raw storage off has to be told
		// (spec 019, edge cases).
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM traces WHERE project_id = ?`, projectID).
			Scan(&out.TracesBeforeWindow); err != nil {
			return out, fmt.Errorf("count traces before the raw window: %w", err)
		}
		return out, nil
	}
	// `timestamp` is the trace's earliest span start, which is the start
	// time Decision 1 compares against the batch line.
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traces WHERE project_id = ? AND timestamp < ?`,
		projectID, beforeThreshold).Scan(&out.TracesBeforeWindow); err != nil {
		return out, fmt.Errorf("count traces before the raw window: %w", err)
	}
	return out, nil
}

// nullableTime is a nullable stamp as a pointer, nil for NULL.
func nullableTime(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	at := v.Int64
	return &at
}

// rawContentType resolves a stored column into the media type the body is in.
func rawContentType(stored sql.NullString) string {
	if !stored.Valid || stored.String == "" {
		return RawContentTypeProtobuf
	}
	return stored.String
}

// zstdHeaderPrefix is how many leading bytes of a frame the header parser can
// need: the magic number, the frame header descriptor, a window descriptor, a
// dictionary id and a frame content size, at their widest.
const zstdHeaderPrefix = 18

// decodedSize reads the decoded length out of a zstd frame header, reporting
// whether the header carried it. `EncodeAll` writes the frame content size for
// every body whose size is worth a listing column; the frames that omit it are
// the few-hundred-byte ones, where the caller's fallback costs nothing.
func decodedSize(prefix []byte) (int64, bool) {
	var header zstd.Header
	if err := header.Decode(prefix); err != nil {
		return 0, false
	}
	if header.HasFCS {
		return int64(header.FrameContentSize), true
	}
	// A single raw or RLE block says its own size, and a frame that is one
	// such block is the whole body.
	if header.FirstBlock.OK && header.FirstBlock.Last && header.FirstBlock.DecompressedSize > 0 {
		return int64(header.FirstBlock.DecompressedSize), true
	}
	return 0, false
}

// rawBodySize decompresses one body to measure it, for the frames whose header
// did not say.
func (s *Store) rawBodySize(ctx context.Context, projectID string, number int64) (int64, error) {
	var stored []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT b.body FROM raw_batch_numbers n JOIN raw_batches b ON b.id = n.batch_id
		  WHERE n.project_id = ? AND n.number = ?`, projectID, number).Scan(&stored)
	if err == sql.ErrNoRows {
		// Swept between the page and this read: a size of zero is wrong
		// by less than an error would be, and the body endpoint answers
		// 404 for it a moment later anyway.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("measure raw batch %d: %w", number, err)
	}
	body, err := Decompress(CompressionZstd, stored)
	if err != nil {
		return 0, fmt.Errorf("measure raw batch %d: %w", number, err)
	}
	return int64(len(body)), nil
}
