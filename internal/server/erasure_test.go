package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// An erasure reaches every copy the store holds (spec 044): what the API
// answers, and the byte scan over the file itself.

// randomWord is a lowercase word nothing else in a test database contains:
// one token to the search index, one run of bytes to a scan.
func randomWord(t *testing.T, prefix string) string {
	t.Helper()
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = 'a' + raw[i]%26
	}
	return prefix + string(raw)
}

// markedSpan is one span of a user's trace with a marker in the trace name,
// in an input under the compression threshold, in an output above it, and —
// when picture is set — in a picture sent as a data URL.
func markedSpan(t *testing.T, trace, span int, user, session, marker string, picture []byte) *tracepb.Span {
	t.Helper()
	traceID, _ := hex.DecodeString(traceHex(trace))
	spanID, _ := hex.DecodeString(spanHex(span))
	attr := func(key, value string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
			Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
	}
	output := strings.Repeat("the answer is ", 8) + marker + strings.Repeat(" and so on", 8)
	attrs := []*commonpb.KeyValue{
		attr("langfuse.user.id", user),
		attr("langfuse.session.id", session),
		attr("langfuse.trace.name", "chat "+marker),
		attr("langfuse.observation.input", `{"q":"`+marker+`"}`),
		attr("langfuse.observation.output", `{"a":"`+output+`"}`),
	}
	if picture != nil {
		attrs = append(attrs, attr("langfuse.observation.metadata",
			`{"image":"data:image/png;base64,`+base64.StdEncoding.EncodeToString(picture)+`"}`))
	}
	return &tracepb.Span{TraceId: traceID, SpanId: spanID, Name: "generation",
		StartTimeUnixNano: uint64(seedBase), EndTimeUnixNano: uint64(seedBase + ms), Attributes: attrs}
}

// postExport sends spans in one ResourceSpans, in either encoding.
func (h *harness) postExport(t *testing.T, asJSON bool, spans ...*tracepb.Span) {
	t.Helper()
	export := []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}
	encode, contentType := mapping.EncodeExportRequest, "application/x-protobuf"
	if asJSON {
		encode, contentType = mapping.EncodeExportRequestJSON, "application/json"
	}
	body, err := encode(export)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.post(t, "/v1/traces", body, func(r *http.Request) { r.Header.Set("Content-Type", contentType) })
	expectStatus(t, rec, http.StatusOK)
}

// scanFiles answers where needle is in the database's files: in their bytes,
// or inside a zstd frame carved out of them — every offset of the frame magic,
// decoded by a size-limited decoder, failures skipped.
func scanFiles(t *testing.T, path, needle string) []string {
	t.Helper()
	magic := []byte{0x28, 0xb5, 0x2f, 0xfd}
	var found []string
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(file)
		if bytes.Contains(data, []byte(needle)) {
			found = append(found, name)
			continue
		}
		for offset := 0; ; {
			at := bytes.Index(data[offset:], magic)
			if at < 0 {
				break
			}
			offset += at
			if carved := carve(data[offset:]); bytes.Contains(carved, []byte(needle)) {
				found = append(found, fmt.Sprintf("%s (a frame at %d)", name, offset))
				break
			}
			offset++
		}
	}
	return found
}

// carve decodes what it can of a zstd frame starting at the first byte: at
// most a megabyte, and whatever came out before an error.
func carve(data []byte) []byte {
	decoder, err := zstd.NewReader(bytes.NewReader(data), zstd.WithDecoderMaxMemory(1<<20),
		zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil
	}
	defer decoder.Close()
	out, _ := io.ReadAll(io.LimitReader(decoder, 1<<20))
	return out
}

// The byte scan (spec 044, Testing): after an erasure and one sweeper pass,
// neither the erased user's marker nor their id is anywhere in the database,
// its log or its shared-memory index — in the bytes or in any zstd frame
// carved from them — while the other user's marker is still read through the
// archive and the observations.
func TestTheByteScan(t *testing.T) {
	h := newAdminHarness(t)
	userA, userB := randomWord(t, "usera"), randomWord(t, "userb")
	m, n := randomWord(t, "zq"), randomWord(t, "zx")
	sessionA := "session-" + userA
	picture := bytes.Repeat([]byte(m+" "), 4096/len(m)+2)

	h.postExport(t, false,
		markedSpan(t, 1, 1, userA, sessionA, m, picture),
		markedSpan(t, 2, 2, userB, "session-b", n, nil))
	h.postExport(t, true,
		markedSpan(t, 3, 3, userA, sessionA, m, nil),
		markedSpan(t, 4, 4, userB, "session-b", n, nil))
	// A verdict on the user's session, and a case cut from their trace.
	h.postScore(t, map[string]any{"session_id": sessionA, "name": "helpful", "value": 1, "comment": m})
	h.postItems(t, "golden", map[string]any{"id": strings.Repeat("c", 32),
		"input": json.RawMessage(`{"q":"` + m + `"}`), "source_trace_id": traceHex(1)})

	// The positive control: before the erasure the scan finds the marker.
	if found := scanFiles(t, h.dbPath, m); len(found) == 0 {
		t.Fatal("the scan finds the marker nowhere before the erasure; it would pass vacuously")
	}

	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/"+userA+"/data?wait=30&confirm="+userA, nil)
	expectStatus(t, rec, http.StatusOK)
	if err := h.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, needle := range []string{m, userA} {
		if found := scanFiles(t, h.dbPath, needle); len(found) > 0 {
			t.Errorf("%q is still in %v after the erasure and a pass", needle, found)
		}
	}

	// The other user is intact: in the archive, and in the observations.
	listing := h.listRaw(t, "")
	readable := false
	for _, batch := range listing.Batches {
		body := h.get(t, "/api/v1/raw/"+batch.ID)
		expectStatus(t, body, http.StatusOK)
		readable = readable || bytes.Contains(body.Body.Bytes(), []byte(n))
	}
	if !readable {
		t.Error("the other user's marker is not readable through the archive any more")
	}
	io := h.get(t, "/api/v1/observations/"+spanHex(2)+"/io")
	expectStatus(t, io, http.StatusOK)
	if !bytes.Contains(io.Body.Bytes(), []byte(n)) {
		t.Errorf("the other user's observation lost its input: %s", io.Body.String())
	}
}

// The erasure's answers carry every count of the API contract (spec 044): the
// dry run the session scores, the dataset items, the datasets and the raw
// block; the confirmed answer those and what the raw archive lost.
func TestTheErasureAnswersWithEveryCount(t *testing.T) {
	h := newAdminHarness(t)
	h.postExport(t, false,
		markedSpan(t, 1, 1, "user-a", "session-a", "a", nil),
		markedSpan(t, 2, 2, "user-b", "session-b", "b", nil))
	h.postExport(t, true, markedSpan(t, 1, 3, "user-a", "session-a", "a", nil))
	h.postScore(t, map[string]any{"session_id": "session-a", "name": "helpful", "value": 1})
	h.postItems(t, "golden", map[string]any{"id": strings.Repeat("c", 32), "input": json.RawMessage(`{}`),
		"source_trace_id": traceHex(1)})
	path := "/api/v1/projects/" + h.project.ID + "/users/user-a/data"

	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		WouldDelete map[string]int64 `json:"would_delete"`
		Datasets    []struct {
			Dataset string `json:"dataset"`
			Items   int64  `json:"items"`
		} `json:"affected_datasets"`
		Raw struct {
			BatchesToScan         *int64 `json:"batches_to_scan"`
			UnattributableBatches *int64 `json:"unattributable_batches"`
		} `json:"raw"`
		Confirm string `json:"confirm"`
		Note    string `json:"note"`
	}](t, rec)
	if preview.WouldDelete["session_scores"] != 1 || preview.WouldDelete["dataset_items"] != 1 {
		t.Errorf("would_delete = %v, want one session score and one dataset item", preview.WouldDelete)
	}
	if len(preview.Datasets) != 1 || preview.Datasets[0].Dataset != "golden" || preview.Datasets[0].Items != 1 {
		t.Errorf("affected_datasets = %+v, want golden losing one", preview.Datasets)
	}
	if preview.Raw.BatchesToScan == nil || *preview.Raw.BatchesToScan != 2 ||
		preview.Raw.UnattributableBatches == nil || *preview.Raw.UnattributableBatches != 0 {
		t.Errorf("raw = %+v, want two batches to scan and none unattributable", preview.Raw)
	}
	if preview.Confirm != "user-a" || !strings.Contains(preview.Note, "raw batches") {
		t.Errorf("confirm %q, note %q", preview.Confirm, preview.Note)
	}

	rec = h.call(t, "DELETE", path+"?wait=30&confirm=user-a", nil)
	expectStatus(t, rec, http.StatusOK)
	answer := decodeJSON[struct {
		Deleted map[string]int64 `json:"deleted"`
	}](t, rec)
	for key, want := range map[string]int64{"traces": 1, "session_scores": 1, "dataset_items": 1,
		"raw_spans": 2, "raw_batches_rewritten": 1, "raw_batches_deleted": 1} {
		if got, ok := answer.Deleted[key]; !ok || got != want {
			t.Errorf("deleted[%s] = %d (present %v), want %d", key, got, ok, want)
		}
	}
}

// A rewritten batch says so on the listing and on the body (spec 044 #2);
// one as received says null and sends no header.
func TestARewrittenBatchIsMarked(t *testing.T) {
	h := newAdminHarness(t)
	h.postExport(t, false,
		markedSpan(t, 1, 1, "user-a", "s", "a", nil),
		markedSpan(t, 2, 2, "user-b", "s", "b", nil))
	h.postExport(t, false, markedSpan(t, 2, 3, "user-b", "s", "b", nil))
	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/user-a/data?confirm=user-a&wait=30", nil)
	expectStatus(t, rec, http.StatusOK)

	rec = h.get(t, "/api/v1/raw")
	expectStatus(t, rec, http.StatusOK)
	listing := decodeJSON[struct {
		Batches []struct {
			ID         string     `json:"id"`
			ScrubbedAt *time.Time `json:"scrubbed_at"`
		} `json:"batches"`
	}](t, rec)
	if len(listing.Batches) != 2 || listing.Batches[0].ScrubbedAt == nil || listing.Batches[1].ScrubbedAt != nil {
		t.Fatalf("listing = %+v, want the first batch marked and the second not", listing.Batches)
	}
	if !strings.Contains(rec.Body.String(), `"scrubbed_at":null`) {
		t.Errorf("a batch as received does not say null: %s", rec.Body.String())
	}
	first := h.get(t, "/api/v1/raw/"+listing.Batches[0].ID)
	at, err := time.Parse(time.RFC3339Nano, first.Header().Get(headerScrubbedAt))
	if err != nil || !at.Equal(*listing.Batches[0].ScrubbedAt) {
		t.Errorf("%s = %q, want the listing's %v", headerScrubbedAt, first.Header().Get(headerScrubbedAt),
			listing.Batches[0].ScrubbedAt)
	}
	second := h.get(t, "/api/v1/raw/"+listing.Batches[1].ID)
	if got := second.Header().Get(headerScrubbedAt); got != "" {
		t.Errorf("a batch as received sends %s: %q", headerScrubbedAt, got)
	}
}

// Erasing a user whose trace no span has reached — no start time — answers
// 200, as deleting that trace does.
func TestErasingATraceWithNoStartTimeAnswers200(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "empty"})
	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/empty/data?confirm=empty&wait=30", nil)
	expectStatus(t, rec, http.StatusOK)
	if got := h.countTraces(t); got != 0 {
		t.Errorf("%d traces left, want none", got)
	}
}

// The retention dry run counts the session-only scores the pass would take
// (spec 044 #8).
func TestTheRetentionDryRunCountsSessionScores(t *testing.T) {
	h := newAdminHarness(t)
	h.postScore(t, map[string]any{"session_id": "gone", "name": "helpful", "value": 1})
	rec := h.call(t, "PATCH", "/api/v1/projects/"+h.project.ID, []byte(`{"retention_days": 1}`))
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		WouldDelete map[string]int64 `json:"would_delete"`
	}](t, rec)
	if _, ok := preview.WouldDelete["session_scores"]; !ok {
		t.Errorf("would_delete = %v, want session_scores", preview.WouldDelete)
	}
	_ = store.BackupLifetime
}

// The note names the batches older than the trace window as what the erasure
// may not find, not as batches it leaves: those holding a pinned trace of the
// user are scrubbed like any other (spec 044 #5 a).
func TestTheNoteDoesNotPromiseTheOldBatchesAreLeft(t *testing.T) {
	note := erasureNote(store.RawErasure{BatchesToScan: 3, UnattributableBatches: 7}, false)
	if !strings.Contains(note, "7 raw batches are older than the trace window and may hold spans") ||
		strings.Contains(note, "not scrubbed") {
		t.Errorf("note = %q", note)
	}
}
