package store

import (
	"slices"
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
		`INSERT INTO raw_batches (project_id, number, received_at, dialect, content_encoding, body)
		 VALUES (?, 1, ?, ?, ?, ?)`, project.ID, int64(1), "langfuse", nil, body); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RawBatches(t.Context(), project.ID, RawFilter{Limit: 10})
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
	batch, err := s.RawBatchBody(t.Context(), project.ID, rows[0].Number)
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
	rows, err := s.RawBatches(t.Context(), project.ID, RawFilter{Limit: 10})
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
		got, err := s.CountRawBatches(t.Context(), project.ID, RawFilter{}, c.cap)
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
	got, err := s.CountRawBatches(t.Context(), project.ID, RawFilter{Since: &since, Until: &until}, 9)
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
	cursor := &RawCursor{ReceivedAt: 100, Number: 5}
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
		// The number is in the index beside the arrival time (spec 019
		// #17), so the page is read in order rather than sorted.
		if strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("backward=%v: the plan sorts the page:\n%s", backward, joined)
		}
	}
}

// A batch's number is its project's own (spec 019 #17): 1, 2, 3… whatever the
// other tenants send in between, and a number the sweep took stays spent.
func TestRawBatchesAreNumberedWithinTheirProject(t *testing.T) {
	s, mine := readStore(t)
	theirs, err := s.CreateProject("theirs", KeyPair{PublicKey: "tp-pk-theirs", Secret: "tp-sk-theirs"})
	if err != nil {
		t.Fatal(err)
	}
	for i, project := range []string{mine.ID, theirs.ID, theirs.ID, mine.ID, theirs.ID, mine.ID} {
		seedRawBatch(t, s, project, &RawBatch{ReceivedAt: int64(i + 1), Body: []byte("x")})
	}
	numbers := func(projectID string) []int64 {
		t.Helper()
		rows, err := s.RawBatches(t.Context(), projectID, RawFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, row := range rows {
			out = append(out, row.Number)
		}
		return out
	}
	if got := numbers(mine.ID); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("my numbers = %v, want 1 2 3: another tenant's batches are not gaps in mine", got)
	}
	if got := numbers(theirs.ID); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("their numbers = %v, want 1 2 3", got)
	}
	// The same number names each project's own batch, and only that.
	for _, project := range []*Project{mine, theirs} {
		body, err := s.RawBatchBody(t.Context(), project.ID, 2)
		if err != nil || body == nil || body.Number != 2 {
			t.Errorf("%s's batch 2 = %+v, %v", project.Name, body, err)
		}
	}

	// The newest batch goes, as the sweep or an erasure would take it; the
	// next one does not reuse its number.
	if _, err := s.db.Exec(`DELETE FROM raw_batches WHERE project_id = ? AND number = 3`, mine.ID); err != nil {
		t.Fatal(err)
	}
	seedRawBatch(t, s, mine.ID, &RawBatch{ReceivedAt: 10, Body: []byte("x")})
	if got := numbers(mine.ID); !slices.Equal(got, []int64{1, 2, 4}) {
		t.Errorf("numbers after the newest went = %v, want 1 2 4: a number names one batch, ever", got)
	}
}

// The upgrade numbers the archive it finds (spec 019 #17): within each
// project, in the order the batches were stored, with the counter where the
// numbers stop, so the next batch follows on.
func TestMigration0035NumbersTheArchiveWithinEachProject(t *testing.T) {
	s, path := openTemp(t)
	mine, err := s.CreateProject("mine", KeyPair{PublicKey: "tp-pk-1", Secret: "tp-sk-1"})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.CreateProject("theirs", KeyPair{PublicKey: "tp-pk-2", Secret: "tp-sk-2"})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	// The table as the previous release left it: one rowid sequence for
	// everyone, a gap where the sweep took a batch, and a batch that arrived
	// late — its received_at before the one stored ahead of it.
	exec(`DROP TRIGGER raw_batches_need_a_number`)
	exec(`DROP INDEX idx_raw_batches_number`)
	exec(`DROP INDEX idx_raw_batches_received`)
	exec(`CREATE INDEX idx_raw_batches_received ON raw_batches(project_id, received_at)`)
	exec(`ALTER TABLE raw_batches DROP COLUMN number`)
	exec(`ALTER TABLE projects DROP COLUMN raw_batches_numbered`)
	exec(`DELETE FROM schema_migrations WHERE filename = '0035_raw_batch_numbers.sql'`)
	for _, row := range []struct {
		id       int64
		project  string
		received int64
	}{{1, mine.ID, 10}, {2, theirs.ID, 20}, {3, theirs.ID, 30}, {5, mine.ID, 50}, {6, theirs.ID, 60}, {7, mine.ID, 40}} {
		exec(`INSERT INTO raw_batches (id, project_id, received_at, body) VALUES (?, ?, ?, ?)`,
			row.id, row.project, row.received, zstdEncoder.EncodeAll([]byte("x"), nil))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer upgraded.Close()
	type numbered struct{ id, number int64 }
	read := func(projectID string) []numbered {
		t.Helper()
		rows, err := upgraded.db.Query(`SELECT id, number FROM raw_batches WHERE project_id = ? ORDER BY id`, projectID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []numbered
		for rows.Next() {
			var n numbered
			if err := rows.Scan(&n.id, &n.number); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	if got, want := read(mine.ID), []numbered{{1, 1}, {5, 2}, {7, 3}}; !slices.Equal(got, want) {
		t.Errorf("mine = %v, want %v: numbered in the order stored", got, want)
	}
	if got, want := read(theirs.ID), []numbered{{2, 1}, {3, 2}, {6, 3}}; !slices.Equal(got, want) {
		t.Errorf("theirs = %v, want %v", got, want)
	}
	// The listing reads arrival order, the late batch where it arrived.
	rows, err := upgraded.RawBatches(t.Context(), mine.ID, RawFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var listed []int64
	for _, row := range rows {
		listed = append(listed, row.Number)
	}
	if !slices.Equal(listed, []int64{1, 3, 2}) {
		t.Errorf("listing = %v, want 1 3 2: by received_at, the number a name and not an order", listed)
	}
	seedRawBatch(t, upgraded, mine.ID, &RawBatch{ReceivedAt: 70, Body: []byte("x")})
	if got := read(mine.ID); got[len(got)-1].number != 4 {
		t.Errorf("the first batch after the upgrade = %v, want number 4", got[len(got)-1])
	}
}
