package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/rawid"
	"github.com/tracepad/tracepad/internal/store"
)

// The raw archive as an API (spec 019 #2, #3). The listing is the one in this
// API that runs forward, the body endpoint answers what the client sent, and
// both are the project's own.

// rawRow is one row of the listing as a caller reads it.
type rawRow struct {
	ID              string `json:"id"`
	ReceivedAt      string `json:"received_at"`
	Dialect         string `json:"dialect"`
	ContentType     string `json:"content_type"`
	ContentEncoding string `json:"content_encoding"`
	SizeBytes       int64  `json:"size_bytes"`
}

type rawListing struct {
	Batches     []rawRow `json:"batches"`
	NextCursor  *string  `json:"next_cursor"`
	PrevCursor  *string  `json:"prev_cursor"`
	Total       int      `json:"total"`
	TotalCapped bool     `json:"total_capped"`
}

func (h *harness) listRaw(t *testing.T, query string) rawListing {
	t.Helper()
	rec := h.get(t, "/api/v1/raw"+query)
	expectStatus(t, rec, 200)
	return decodeJSON[rawListing](t, rec)
}

// seedRaw writes n batches a millisecond apart, so the keyset has both columns
// to work with and the window filters have boundaries to land on.
func seedRaw(t *testing.T, h *harness, projectID string, n int, contentType string) []string {
	t.Helper()
	numbers := make([]int64, 0, n)
	for i := range n {
		body := []byte(strings.Repeat(fmt.Sprintf("batch-%03d;", i), 40))
		err := h.writer.Submit(t.Context(), &store.IngestBatch{
			ProjectID: projectID,
			Raw: &store.RawBatch{
				ReceivedAt:  seedBase + int64(i)*ms,
				Dialect:     "langfuse",
				ContentType: contentType,
				Body:        body,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.store.RawBatches(t.Context(), projectID, store.RawFilter{Limit: n + 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		numbers = append(numbers, row.Number)
	}
	// Sorted here rather than trusted from the listing: the numbers are the
	// project's own count and the batches were written in arrival order, so
	// ascending *is* arrival order — and reading it off the listing would
	// make every order assertion below agree with whatever the listing did.
	slices.Sort(numbers)
	ids := make([]string, 0, n)
	for _, number := range numbers {
		ids = append(ids, rawid.ID(number))
	}
	return ids
}

func instantOf(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("received_at %q is not RFC 3339: %v", value, err)
	}
	return parsed
}

// The listing runs oldest first and pages forward across three pages, then
// back with `prev_cursor` onto the rows it came from (spec 019 #3).
func TestRawListingPagesForward(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	ids := seedRaw(t, h, h.project.ID, 7, "")

	var walked []string
	var arrivals []string
	cursor := ""
	for page := range 4 {
		query := "?limit=3"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		listing := h.listRaw(t, query)
		for _, row := range listing.Batches {
			walked = append(walked, row.ID)
			arrivals = append(arrivals, row.ReceivedAt)
		}
		if listing.NextCursor == nil {
			if page != 2 {
				t.Fatalf("the walk ended on page %d, want three pages of 3+3+1", page)
			}
			break
		}
		cursor = *listing.NextCursor
	}
	if len(walked) != 7 {
		t.Fatalf("walked %d batches, want all 7", len(walked))
	}
	for i, id := range walked {
		if id != ids[i] {
			t.Fatalf("page order = %v, want arrival order %v", walked, ids)
		}
	}
	// The order named in its own terms, so that a listing which reversed
	// itself consistently could not pass by agreeing with the ids above.
	//
	// Compared as instants, not as text: RFC 3339 omits an all-zero
	// fraction, and `…:00Z` sorts *after* `…:00.001Z` as a string.
	for i := 1; i < len(arrivals); i++ {
		if instantOf(t, arrivals[i]).Before(instantOf(t, arrivals[i-1])) {
			t.Fatalf("received_at across the walk = %v, want oldest first", arrivals)
		}
	}

	// And back. The last page's `prev_cursor` reads the page before it,
	// which is the middle three.
	last := h.listRaw(t, "?limit=3&cursor="+cursor)
	if last.PrevCursor == nil {
		t.Fatal("a page reached by a cursor must offer the way back")
	}
	back := h.listRaw(t, "?limit=3&direction=prev&cursor="+*last.PrevCursor)
	if len(back.Batches) != 3 || back.Batches[0].ID != ids[3] || back.Batches[2].ID != ids[5] {
		t.Errorf("the page back = %+v, want %v", back.Batches, ids[3:6])
	}
}

// `since` is inclusive and `until` exclusive, so walking a timeline a window at
// a time never replays a batch twice.
func TestRawListingWindowIsHalfOpen(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	ids := seedRaw(t, h, h.project.ID, 5, "")

	at := func(i int) string {
		return time.Unix(0, seedBase+int64(i)*ms).UTC().Format(time.RFC3339Nano)
	}
	listing := h.listRaw(t, "?since="+at(1)+"&until="+at(4))
	if len(listing.Batches) != 3 {
		t.Fatalf("batches = %d, want the three inside [1, 4)", len(listing.Batches))
	}
	if listing.Batches[0].ID != ids[1] || listing.Batches[2].ID != ids[3] {
		t.Errorf("window = %+v, want batches 1..3", listing.Batches)
	}
	// The two halves of a split window cover the whole of it, once each.
	first := h.listRaw(t, "?until="+at(2))
	second := h.listRaw(t, "?since="+at(2))
	if len(first.Batches)+len(second.Batches) != 5 {
		t.Errorf("a split window covered %d+%d batches, want 5 exactly once",
			len(first.Batches), len(second.Batches))
	}
}

// The count is over the filters and not over the page, and its cap is this
// listing's own: it answers "how much is this export about to send".
func TestRawListingCounts(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	other, err := h.store.CreateProject("other", store.KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	seedRaw(t, h, h.project.ID, 5, "")
	seedRaw(t, h, other.ID, 3, "")

	// Over the filters and not over the page: the cursor is where the
	// reader is, not what there is.
	listing := h.listRaw(t, "?limit=2&count=1")
	if len(listing.Batches) != 2 {
		t.Fatalf("batches = %d, want the page", len(listing.Batches))
	}
	if listing.Total != 5 || listing.TotalCapped {
		t.Errorf("total = %d capped = %v, want this project's 5", listing.Total, listing.TotalCapped)
	}

	// And bounded by the window, which is the number an export is about to
	// act on rather than the size of the archive.
	at := func(i int) string {
		return time.Unix(0, seedBase+int64(i)*ms).UTC().Format(time.RFC3339Nano)
	}
	windowed := h.listRaw(t, "?limit=2&count=1&since="+at(1)+"&until="+at(4))
	if windowed.Total != 3 {
		t.Errorf("total inside [1, 4) = %d, want 3", windowed.Total)
	}
}

// A project key reaches its own archive and nothing else: the batches are the
// project's data exactly as its traces are (spec 019 #9). And a batch's id is
// the project's own count (#17): another tenant's exports between two of mine
// leave no gap, and the id another tenant's batch has names my batch of that
// number, or nothing.
func TestRawIsScopedToTheProject(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	other, err := h.store.CreateProject("other", store.KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	ingest := func(projectID, marker string) {
		t.Helper()
		if err := h.writer.Submit(t.Context(), &store.IngestBatch{ProjectID: projectID,
			Raw: &store.RawBatch{ReceivedAt: seedBase, Body: []byte(marker)}}); err != nil {
			t.Fatal(err)
		}
	}
	ingest(h.project.ID, "mine-1")
	for i := range 3 {
		ingest(other.ID, fmt.Sprintf("theirs-%d", i+1))
	}
	ingest(h.project.ID, "mine-2")

	listing := h.listRaw(t, "")
	var ids []string
	for _, row := range listing.Batches {
		ids = append(ids, row.ID)
	}
	if !slices.Equal(ids, []string{"n1", "n2"}) {
		t.Fatalf("ids = %v, want 1 2: only this project's two, and the other's three are no gap", ids)
	}
	for id, want := range map[string]string{"n1": "mine-1", "n2": "mine-2"} {
		rec := h.get(t, "/api/v1/raw/"+id)
		expectStatus(t, rec, 200)
		if got := rec.Body.String(); got != want {
			t.Errorf("batch %s = %q, want this project's %q", id, got, want)
		}
	}
	// The other project's third batch has a number this one has not
	// reached: not found, as a number never issued is.
	expectError(t, h.get(t, "/api/v1/raw/n3"), 404, "raw batch n3 not found")
}

// The body comes back as the client sent it, under the type it was sent in,
// with the two headers a client needs to name a file without a second request.
func TestRawBodyIsWhatArrived(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	sent := fixtureBody(t, "001-langfuse-sdk-generation")
	if rec := h.post(t, "/v1/traces", sent); rec.Code != http.StatusOK {
		t.Fatalf("ingest status = %d", rec.Code)
	}

	listing := h.listRaw(t, "")
	if len(listing.Batches) != 1 {
		t.Fatalf("batches = %d", len(listing.Batches))
	}
	row := listing.Batches[0]
	if row.ContentType != "application/x-protobuf" {
		t.Errorf("content_type = %q", row.ContentType)
	}
	if row.SizeBytes != int64(len(sent)) {
		t.Errorf("size_bytes = %d, want the decoded length %d", row.SizeBytes, len(sent))
	}
	if row.Dialect != "langfuse" {
		t.Errorf("dialect = %q", row.Dialect)
	}

	rec := h.get(t, "/api/v1/raw/"+row.ID)
	expectStatus(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), sent) {
		t.Error("the body endpoint did not answer the bytes the client sent")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get(headerReceivedAt); got == "" {
		t.Error("the received-at header is missing")
	}
	if got := rec.Header().Get(headerDialect); got != "langfuse" {
		t.Errorf("%s = %q", headerDialect, got)
	}
}

// A body larger than the response budget comes back whole. The budget exists
// so a consumer's context window is not spent on payloads; a cut export body
// is not a smaller batch, it is a broken one (spec 019 #3).
func TestRawBodyIsExemptFromTheBudget(t *testing.T) {
	cfg := &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		ResponseBudgetBytes: config.MinResponseBudgetBytes}
	h := newHarness(t, cfg, store.WriterOptions{})

	body := bytes.Repeat([]byte("the archive is what arrived; "), 4000)
	if int64(len(body)) <= cfg.ResponseBudgetBytes {
		t.Fatalf("the fixture body is %d bytes, which does not exceed the budget", len(body))
	}
	err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: h.project.ID,
		Raw:       &store.RawBatch{ReceivedAt: seedBase, Body: body},
	})
	if err != nil {
		t.Fatal(err)
	}

	listing := h.listRaw(t, "")
	rec := h.get(t, "/api/v1/raw/"+listing.Batches[0].ID)
	expectStatus(t, rec, 200)
	if rec.Body.Len() != len(body) {
		t.Errorf("body = %d bytes, want the whole %d", rec.Body.Len(), len(body))
	}
}

// A batch written before schema 0012 has no content type, and reads as the one
// encoding the endpoint accepted then.
func TestRawContentTypeDefaultsToProtobuf(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// An empty content type is stored as NULL, which is the state every row
	// written before schema 0012 is in.
	seedRaw(t, h, h.project.ID, 1, "")

	listing := h.listRaw(t, "")
	if got := listing.Batches[0].ContentType; got != "application/x-protobuf" {
		t.Errorf("content_type of a pre-0012 row = %q, want the protobuf encoding", got)
	}
	rec := h.get(t, "/api/v1/raw/"+listing.Batches[0].ID)
	expectStatus(t, rec, 200)
	if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", got)
	}
}

// A swept batch and a malformed id are answered plainly rather than with a
// stack trace or an empty 200.
func TestRawBodyRefusals(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for _, c := range []struct {
		path   string
		status int
	}{
		{"/api/v1/raw/n999999", 404},
		{"/api/v1/raw/nope", 400},
		{"/api/v1/raw/n0", 400},
		{"/api/v1/raw/n-1", 400},
		{"/api/v1/raw/n01", 400},
		// A bare integer is an id from before the numbering (spec 019
		// #17): refused, never read as this project's batch of that number.
		{"/api/v1/raw/1", 400},
	} {
		if rec := h.get(t, c.path); rec.Code != c.status {
			t.Errorf("GET %s = %d, want %d (%s)", c.path, rec.Code, c.status, rec.Body)
		}
	}
}

// The system block is what the export reports at its end and what an operator
// reads before shortening the raw window (spec 019 #4).
func TestSystemReportsTheRawArchive(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// A trace that started before the archive begins, and one after: only
	// the first is beyond an export's reach.
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase - 5*ms, EndTime: seedBase - 4*ms})
	h.seed(t, &model.Trace{ID: traceHex(2)},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase + 10*ms, EndTime: seedBase + 11*ms})
	seedRaw(t, h, h.project.ID, 3, "")

	block := h.systemRaw(t)
	if !block.Enabled {
		t.Error("enabled = false with TRACEPAD_STORE_RAW on")
	}
	if block.Batches != 3 {
		t.Errorf("batches = %d, want 3", block.Batches)
	}
	if block.Bytes == 0 {
		t.Error("bytes = 0, want what the archive occupies")
	}
	if block.Oldest == nil || block.Newest == nil || *block.Oldest == *block.Newest {
		t.Errorf("window = %v..%v, want the archive's two ends", block.Oldest, block.Newest)
	}
	if block.TracesBeforeWindow != 1 {
		t.Errorf("traces_before_window = %d, want the one trace older than the archive",
			block.TracesBeforeWindow)
	}
}

// With raw storage off and the table empty, the block says so and every trace
// is beyond reach — which is what makes `export` able to refuse with a reason
// rather than with an empty result (spec 019, edge cases).
func TestSystemRawWithStorageOff(t *testing.T) {
	cfg := &config.Config{Listen: ":0", StoreRaw: false, MaxBodyBytes: config.DefaultMaxBodyBytes}
	h := newHarness(t, cfg, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	block := h.systemRaw(t)
	if block.Enabled || block.Batches != 0 || block.Bytes != 0 {
		t.Errorf("raw block = %+v, want an empty, disabled archive", block)
	}
	if block.Oldest != nil || block.Newest != nil {
		t.Errorf("window = %v..%v, want null on both ends", block.Oldest, block.Newest)
	}
	if block.TracesBeforeWindow != 1 {
		t.Errorf("traces_before_window = %d, want every trace", block.TracesBeforeWindow)
	}
}

type systemRawBlock struct {
	Enabled            bool    `json:"enabled"`
	Batches            int64   `json:"batches"`
	Bytes              int64   `json:"bytes"`
	Oldest             *string `json:"oldest_received_at"`
	Newest             *string `json:"newest_received_at"`
	TracesBeforeWindow int64   `json:"traces_before_window"`
}

func (h *harness) systemRaw(t *testing.T) systemRawBlock {
	t.Helper()
	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	var body struct {
		Raw json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var block systemRawBlock
	if err := json.Unmarshal(body.Raw, &block); err != nil {
		t.Fatalf("the raw block is not the documented shape: %v (%s)", err, body.Raw)
	}
	return block
}

// A cursor from before batches were numbered within their project names its
// batch by the table's rowid, which no longer leaves the server (spec 019
// #17). It is refused with where to start again, not read as a number — which
// would land the export at whatever batch of this project has that number.
func TestRawRefusesACursorFromBeforeTheNumbers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedRaw(t, h, h.project.ID, 3, "")
	at := seedBase + ms

	old := encodeCursor(strconv.FormatInt(at, 10), "2")
	instant := func(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339Nano) }
	// Each direction is told the window that walks the same way from the
	// cursor's instant, and doing what it says lands on the cursor's batch.
	for _, c := range []struct {
		direction, hint, follow string
		want                    []string
	}{
		{"", "since=" + instant(at), "?since=" + instant(at), []string{"n2", "n3"}},
		{"&direction=prev", "direction=prev&until=" + instant(at+1),
			"?direction=prev&until=" + instant(at+1), []string{"n1", "n2"}},
	} {
		rec := h.get(t, "/api/v1/raw?cursor="+old+c.direction)
		expectError(t, rec, http.StatusBadRequest, "from before raw batches were numbered within their project")
		refusal := decodeJSON[struct {
			Error string `json:"error"`
		}](t, rec).Error
		if !strings.Contains(refusal, c.hint) {
			t.Errorf("direction %q: the refusal does not say %s: %s", c.direction, c.hint, refusal)
		}
		var got []string
		for _, row := range h.listRaw(t, c.follow).Batches {
			got = append(got, row.ID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("direction %q: following the hint = %v, want %v", c.direction, got, c.want)
		}
	}
	// Anything else that is not this grammar is just not a cursor.
	for _, cursor := range []string{"nonsense", encodeCursor("n", "1"), encodeCursor("x", "1", "2"), encodeCursor("a", "b")} {
		expectError(t, h.get(t, "/api/v1/raw?cursor="+cursor), http.StatusBadRequest, "invalid cursor")
	}
}
