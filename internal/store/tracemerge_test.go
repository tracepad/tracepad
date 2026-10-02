package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// A key the later delivery does not name is written back exactly as it was
// stored (spec 002 #32): an integer past 2^53, decoded as a float64 on its
// way through the merge, would come back rounded.
func TestTraceMetadataMergeKeepsTheStoredNumbers(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const id = "abcdefabcdefabcdefabcdefabcdef01"
	for _, metadata := range []map[string]any{
		{"big": json.Number("9007199254740993")},
		{"added": "late"},
	} {
		batch := &IngestBatch{ProjectID: project.ID, Traces: []*model.Trace{{ID: id, Metadata: metadata}}}
		if err := writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}

	var metadataID sql.NullInt64
	if err := s.db.QueryRow(`SELECT metadata_id FROM traces WHERE project_id = ? AND id = ?`, project.ID, id).
		Scan(&metadataID); err != nil {
		t.Fatal(err)
	}
	raw, err := rawPayload(t.Context(), s.db, metadataID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"big":9007199254740993`) || !strings.Contains(raw, `"added":"late"`) {
		t.Errorf("stored metadata = %s, want both keys, the integer as it was sent", raw)
	}
}

// storedMetadata reads a trace's metadata payload back as stored.
func storedMetadata(t *testing.T, s *Store, projectID, traceID string) map[string]any {
	t.Helper()
	var metadataID sql.NullInt64
	if err := s.db.QueryRow(`SELECT metadata_id FROM traces WHERE project_id = ? AND id = ?`, projectID, traceID).
		Scan(&metadataID); err != nil {
		t.Fatal(err)
	}
	raw, err := rawPayload(t.Context(), s.db, metadataID)
	if err != nil {
		t.Fatal(err)
	}
	return decodeStoredObject(raw)
}

// Stored metadata that is a JSON null — no ingest writes one, but a row may
// hold it — reads as no keys, and a merge onto it stores the delivery's
// rather than failing the batch on every retry.
func TestTraceMetadataMergeOntoANull(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const id = "abcdefabcdefabcdefabcdefabcdef02"
	submit := func(metadata map[string]any) error {
		return writer.Submit(t.Context(), &IngestBatch{ProjectID: project.ID,
			Traces: []*model.Trace{{ID: id, Metadata: metadata}}})
	}
	if err := submit(map[string]any{"first": "x"}); err != nil {
		t.Fatal(err)
	}
	compression, body, size := compress([]byte("null"))
	if _, err := s.db.Exec(`UPDATE payloads SET compression = ?, body = ?, size_raw = ?
	    WHERE id = (SELECT metadata_id FROM traces WHERE project_id = ? AND id = ?)`,
		compression, body, size, project.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := submit(map[string]any{"second": "y"}); err != nil {
		t.Fatalf("a merge onto a null failed: %v", err)
	}
	if got := storedMetadata(t, s, project.ID, id); len(got) != 1 || got["second"] != "y" {
		t.Errorf("metadata = %v, want the delivery's key alone", got)
	}
}

// The bounds keep keys out and nothing else (spec 002 #32): a new key is added
// while there are fewer than TraceMetadataMaxKeys and it fits in
// TraceMetadataMaxBytes; a stored key always takes a later value that keeps
// the whole within the bytes; the first delivery is bounded too; and the batch
// reports a trace it kept a key out of.
func TestTraceMetadataMergeIsBounded(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	submit := func(id string, metadata map[string]any) *IngestBatch {
		t.Helper()
		batch := &IngestBatch{ProjectID: project.ID, Traces: []*model.Trace{{ID: id, Metadata: metadata}}}
		if err := writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		return batch
	}
	keys := func(from, n int, value string) map[string]any {
		out := make(map[string]any, n)
		for i := from; i < from+n; i++ {
			out[fmt.Sprintf("k%04d", i)] = value
		}
		return out
	}

	const piecemeal = "abcdefabcdefabcdefabcdefabcdef03"
	for i := range 5 {
		if batch := submit(piecemeal, keys(i*100, 100, "v")); len(batch.MetadataCapped) != 0 {
			t.Fatalf("delivery %d capped at %d keys", i, i*100)
		}
	}
	// The sixth brings a hundred new keys: twelve fit.
	if batch := submit(piecemeal, keys(500, 100, "v")); len(batch.MetadataCapped) != 1 {
		t.Errorf("the delivery past the bound reported %v, want the trace", batch.MetadataCapped)
	}
	got := storedMetadata(t, s, project.ID, piecemeal)
	if len(got) != TraceMetadataMaxKeys || got["k0511"] != "v" || got["k0512"] != nil {
		t.Errorf("metadata holds %d keys, want k0000 … k0511", len(got))
	}
	// Full, a stored key is still updated beside new ones that are not
	// added.
	update := keys(600, 100, "v")
	update["k0000"] = "updated"
	submit(piecemeal, update)
	got = storedMetadata(t, s, project.ID, piecemeal)
	if len(got) != TraceMetadataMaxKeys || got["k0000"] != "updated" {
		t.Errorf("metadata = %d keys, k0000 = %v; want the stored key updated and no new one", len(got), got["k0000"])
	}
	// A value that would carry the whole past the bytes is not taken, and
	// the delivery's other key is.
	huge := submit(piecemeal, map[string]any{"k0001": strings.Repeat("x", TraceMetadataMaxBytes), "k0002": "small"})
	got = storedMetadata(t, s, project.ID, piecemeal)
	if len(huge.MetadataCapped) != 1 || got["k0001"] != "v" || got["k0002"] != "small" {
		t.Errorf("k0001 = %.20v, k0002 = %v, capped %v; want the large value kept out and the small one in",
			got["k0001"], got["k0002"], huge.MetadataCapped)
	}

	// The first delivery is bounded the same way.
	const wide = "abcdefabcdefabcdefabcdefabcdef04"
	if batch := submit(wide, keys(0, 600, "v")); len(batch.MetadataCapped) != 1 {
		t.Errorf("a first delivery of 600 keys reported %v, want the trace", batch.MetadataCapped)
	}
	if got := storedMetadata(t, s, project.ID, wide); len(got) != TraceMetadataMaxKeys {
		t.Errorf("a first delivery stored %d keys, want %d", len(got), TraceMetadataMaxKeys)
	}

	// Metadata stored past the bytes — before the bound, or under another
	// build — is still updated by a value that does not grow it.
	const legacy = "abcdefabcdefabcdefabcdefabcdef05"
	submit(legacy, map[string]any{"a": "x"})
	compression, body, size := compress([]byte(`{"a":"x","big":"` + strings.Repeat("y", TraceMetadataMaxBytes) + `"}`))
	if _, err := s.db.Exec(`UPDATE payloads SET compression = ?, body = ?, size_raw = ?
	    WHERE id = (SELECT metadata_id FROM traces WHERE project_id = ? AND id = ?)`,
		compression, body, size, project.ID, legacy); err != nil {
		t.Fatal(err)
	}
	submit(legacy, map[string]any{"a": "z", "new": "kept out"})
	if got := storedMetadata(t, s, project.ID, legacy); got["a"] != "z" || got["new"] != nil {
		t.Errorf("over the bound: a = %v, new = %v; want a updated and no new key", got["a"], got["new"])
	}
}

// A delivery that changes nothing — a span sent again, the same tags and the
// same metadata — writes no payload and leaves the row's tags as they were.
func TestTraceMergeThatChangesNothingWritesNothing(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const id = "abcdefabcdefabcdefabcdefabcdef06"
	trace := func() *model.Trace {
		return &model.Trace{ID: id, Tags: []string{"a", "b"},
			Metadata: map[string]any{"n": json.Number("42"), "s": "x"}}
	}
	payloads := func() (n int) {
		if err := s.db.QueryRow(`SELECT count(*) FROM payloads`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := writer.Submit(t.Context(), &IngestBatch{ProjectID: project.ID, Traces: []*model.Trace{trace()}}); err != nil {
		t.Fatal(err)
	}
	before := payloads()
	again := trace()
	again.Tags = []string{"b"}
	again.Metadata = map[string]any{"n": float64(42), "s": "x"}
	for _, tr := range []*model.Trace{trace(), again} {
		if err := writer.Submit(t.Context(), &IngestBatch{ProjectID: project.ID, Traces: []*model.Trace{tr}}); err != nil {
			t.Fatal(err)
		}
	}
	if after := payloads(); after != before {
		t.Errorf("payloads %d → %d: a delivery that changed nothing wrote one", before, after)
	}
	var tags string
	s.db.QueryRow(`SELECT tags FROM traces WHERE project_id = ? AND id = ?`, project.ID, id).Scan(&tags)
	if tags != `["a","b"]` {
		t.Errorf("tags = %s, want them as stored", tags)
	}
}
