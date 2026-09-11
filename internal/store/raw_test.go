package store

import (
	"strings"
	"testing"
)

// seedRawBatch writes one archived body through the same path ingest uses.
func seedRawBatch(t *testing.T, s *Store, projectID string, raw *RawBatch) {
	t.Helper()
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Submit(t.Context(), &IngestBatch{ProjectID: projectID, Raw: raw}); err != nil {
		t.Fatal(err)
	}
}

// The archive's readers (spec 019 #3). The listing's SQL is asserted against
// EXPLAIN, and the one column schema 0012 adds is asserted against a row
// written the way every pre-0012 row was.

// A pre-0012 row names no `content_type` at all. It has to read as the one
// encoding the endpoint accepted before this spec, because that is what its
// absence means — not as an empty string a replay would post under.
func TestRawContentTypeOfAPreMigrationRow(t *testing.T) {
	s, project := readStore(t)
	body := zstdEncoder.EncodeAll([]byte(strings.Repeat("pre-0012;", 100)), nil)
	if _, err := s.db.Exec(
		`INSERT INTO raw_batches (project_id, received_at, dialect, content_encoding, body)
		 VALUES (?, ?, ?, ?, ?)`, project.ID, int64(1), "langfuse", nil, body); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RawBatches(project.ID, RawFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ContentType != RawContentTypeProtobuf {
		t.Errorf("content_type = %q, want %q", rows[0].ContentType, RawContentTypeProtobuf)
	}
	if rows[0].SizeBytes != 900 {
		t.Errorf("size_bytes = %d, want the decoded 900", rows[0].SizeBytes)
	}
	batch, err := s.RawBatchBody(project.ID, rows[0].ID)
	if err != nil || batch == nil {
		t.Fatalf("body = %v, err = %v", batch, err)
	}
	if batch.ContentType != RawContentTypeProtobuf || len(batch.Body) != 900 {
		t.Errorf("body = %d bytes under %q", len(batch.Body), batch.ContentType)
	}
}

// The decoded size is what a fetch will return, whatever the frame header says.
// Small bodies are the ones zstd writes without a frame content size, so the
// two paths through `decodedSize` are both walked here.
func TestRawSizeIsTheDecodedLength(t *testing.T) {
	s, project := readStore(t)
	for i, size := range []int{1, 40, 300, 5000, 200000} {
		body := []byte(strings.Repeat("x", size))
		seedRawBatch(t, s, project.ID, &RawBatch{ReceivedAt: int64(i + 1), Body: body})
	}
	rows, err := s.RawBatches(project.ID, RawFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 40, 300, 5000, 200000}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, row := range rows {
		if row.SizeBytes != want[i] {
			t.Errorf("size_bytes[%d] = %d, want %d", i, row.SizeBytes, want[i])
		}
	}
}

// The count stops at its cap rather than counting the archive out, and the cap
// bounds the *answer* rather than the scan — the `LIMIT` inside the subquery
// ends the scan only once that many rows have matched (spec 009 #12). The
// endpoint's own cap is 100 000, which is too many rows to seed; what is under
// test here is the mechanism it uses.
func TestRawCountStopsAtItsCap(t *testing.T) {
	s, project := readStore(t)
	for i := range 5 {
		seedRawBatch(t, s, project.ID, &RawBatch{ReceivedAt: int64(i + 1), Body: []byte("x")})
	}

	for _, c := range []struct{ cap, want int }{{3, 3}, {5, 5}, {9, 5}} {
		got, err := s.CountRawBatches(project.ID, RawFilter{}, c.cap)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("count with cap %d = %d, want %d", c.cap, got, c.want)
		}
	}
	// And the window still bounds it, so a capped count is a count of what
	// an export would send rather than of what is stored.
	since, until := int64(2), int64(5)
	got, err := s.CountRawBatches(project.ID, RawFilter{Since: &since, Until: &until}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("count inside [2, 5) = %d, want 3", got)
	}
}

// The page seeks rather than scans, in both directions: a listing that read the
// project from one end every time would be a listing an export outgrows on its
// first real archive (the method of spec 003 #25).
func TestRawListingSeeksOnItsIndex(t *testing.T) {
	s, project := readStore(t)
	cursor := &RawCursor{ReceivedAt: 100, ID: 5}
	for _, backward := range []bool{false, true} {
		query, args := rawQuery(project.ID, RawFilter{Limit: 10, After: cursor, Backward: backward})
		plan, err := s.explainQueryPlan(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "idx_raw_batches_received") {
			t.Errorf("backward=%v: the plan does not use the arrival index:\n%s", backward, joined)
		}
		if strings.Contains(joined, "SCAN raw_batches") {
			t.Errorf("backward=%v: the plan scans the table:\n%s", backward, joined)
		}
	}
}
