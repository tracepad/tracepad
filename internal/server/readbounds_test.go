package server

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The read bounds of spec 043 (#15–#17, #21): a deadline on every read, a
// limit on how many run at once, and a cap on the one filter whose cost grew
// with its length.

// newReadHarness is a harness whose reads have the given deadline and slots.
func newReadHarness(t *testing.T, timeout time.Duration, slots int) *harness {
	t.Helper()
	return newHarness(t, &config.Config{
		Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		ReadTimeout: timeout, ReadConcurrency: slots,
	}, store.WriterOptions{})
}

// holdKey marks a request the readAdmitted seam holds in its slot until the
// channel it carries is closed.
type holdKey struct{}

// stallKey marks a request whose read runs until its deadline stops it.
type stallKey struct{}

type held struct {
	entered chan struct{}
	release chan struct{}
}

// holdReads installs the seam for the duration of the test.
func holdReads(t *testing.T) {
	t.Helper()
	previous := readAdmitted
	readAdmitted = func(ctx context.Context) {
		if hold, ok := ctx.Value(holdKey{}).(*held); ok {
			hold.entered <- struct{}{}
			<-hold.release
		}
		if _, ok := ctx.Value(stallKey{}).(bool); ok {
			// A query that runs until the deadline stops it.
			<-ctx.Done()
		}
	}
	t.Cleanup(func() { readAdmitted = previous })
}

// holdRead starts a read that sits in its slot until release is closed, and
// returns once it is in; done closes when it has been answered.
func (h *harness) holdRead(t *testing.T, path, secret string, release chan struct{}) (done chan *httptest.ResponseRecorder) {
	t.Helper()
	hold := &held{entered: make(chan struct{}), release: release}
	done = make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		req = req.WithContext(context.WithValue(req.Context(), holdKey{}, hold))
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-hold.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the held read never got a slot")
	}
	return done
}

// slowTags seeds traces whose tag filter is a scan the planner cannot cut
// short: every trace carries the first nine of ten asked-for tags and forty
// others, and none carries the tenth, so each row is tested against all ten.
func (h *harness) seedSlowTags(t *testing.T, traces int) string {
	t.Helper()
	tags := make([]string, 0, 49)
	for n := 1; n <= 9; n++ {
		tags = append(tags, fmt.Sprintf("wanted-%d", n))
	}
	for n := 0; n < 40; n++ {
		tags = append(tags, fmt.Sprintf("other-%02d", n))
	}
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{ProjectID: h.project.ID,
		Traces: []*model.Trace{{ID: traceHex(1), Environment: "production", Tags: tags}}}); err != nil {
		t.Fatal(err)
	}
	// The rest are copies of the first, made in the file: the writer would
	// spend most of a minute on what the filter reads from one column.
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for {
		var held int
		if err := db.QueryRow(`SELECT count(*) FROM traces`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held >= traces {
			break
		}
		for _, statement := range []string{
			`CREATE TEMP TABLE copies AS SELECT * FROM traces LIMIT ` + fmt.Sprint(traces-held),
			`UPDATE copies SET id = printf('%032x', rowid + ` + fmt.Sprint(held) + `)`,
			`INSERT INTO traces SELECT * FROM copies`,
			`DROP TABLE copies`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
	}
	query := url.Values{}
	for n := 1; n <= 10; n++ {
		query.Add("tag", fmt.Sprintf("wanted-%d", n))
	}
	return "/api/v1/traces?" + query.Encode()
}

// A read past its deadline is stopped and answered `503` with the setting in
// the message, not left to scan to its end; the connection it held serves the
// next read; and a client that hangs up ends its read (spec 043 #15).
func TestReadDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	h := newReadHarness(t, timeout, 4)
	h.store.BoundPool(1)
	seeding := time.Now()
	slow := h.seedSlowTags(t, 50_000)
	t.Logf("seeded in %s", time.Since(seeding))

	start := time.Now()
	rec := h.get(t, slow)
	elapsed := time.Since(start)
	expectError(t, rec, 503, "the read took longer than 200ms and was stopped; narrow the time range or the filters")
	if rec.Header().Get("Retry-After") != "" {
		t.Error("the deadline's 503 carries Retry-After; the same request would be stopped again")
	}
	if got := rec.Header().Get("Cache-Control"); got != callerCacheControl {
		t.Errorf("Cache-Control = %q, want the caller's %q", got, callerCacheControl)
	}
	if elapsed > timeout+time.Second {
		t.Errorf("the stopped read answered after %s, want the deadline of %s and a small margin", elapsed, timeout)
	}
	for range 12 {
		// More stopped reads than the pool has connections: each gave
		// its back, or the last of these would wait for one.
		expectStatus(t, h.get(t, slow), 503)
	}
	expectStatus(t, h.get(t, "/api/v1/traces?limit=1"), 200)

	t.Run("a client that hangs up ends its read", func(t *testing.T) {
		h.server.readTimeout = time.Minute
		start := time.Now()
		expectStatus(t, h.get(t, slow), 200)
		whole := time.Since(start)
		t.Logf("the whole scan takes %s", whole)

		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequest("GET", slow, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		returned := make(chan time.Time, 1)
		go func() {
			h.server.Handler().ServeHTTP(rec, req)
			returned <- time.Now()
		}()
		time.Sleep(whole / 10)
		hungUp := time.Now()
		cancel()
		after := (<-returned).Sub(hungUp)
		// A ratio, not a number: the scan to its end is the yardstick.
		if after > whole/3 {
			t.Errorf("the read went on for %s after its client left; the whole scan takes %s", after, whole)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("a read whose client left was answered: %s", rec.Body)
		}
	})
}

// No slot before the deadline is `503` busy with Retry-After, while ingest and
// the key lookup behind it take none (spec 043 #16).
func TestReadSlots(t *testing.T) {
	const timeout = 300 * time.Millisecond
	h := newReadHarness(t, timeout, 2)
	seedCorpus(t, h)
	holdReads(t)

	release := make(chan struct{})
	first := h.holdRead(t, "/api/v1/traces", testSecret, release)
	second := h.holdRead(t, "/api/v1/sessions", testSecret, release)

	start := time.Now()
	rec := h.get(t, "/api/v1/traces?limit=1")
	elapsed := time.Since(start)
	expectError(t, rec, 503, "the server is busy; retry shortly")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if elapsed < timeout {
		t.Errorf("the refused read answered after %s; it waits for a slot until its deadline, %s", elapsed, timeout)
	}
	// Ingest, and the key lookup its guard makes, are not reads.
	expectStatus(t, h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation")), 200)

	close(release)
	<-first
	<-second
	expectStatus(t, h.get(t, "/api/v1/traces?limit=1"), 200)
}

// A slot is given back when the status is written, so a client that reads a
// large body slowly holds its socket and not the one slot (spec 043 #16).
func TestReadSlotIsNotHeldByASlowDownload(t *testing.T) {
	h := newReadHarness(t, 2*time.Second, 1)
	picture := testPicture(10<<20, 7)
	sha := hexSHA(picture)
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, imageExport(t, picture))), 200)

	server := httptest.NewServer(h.server.Handler())
	t.Cleanup(server.Close)

	// A client that asks for the body, reads the headers and stops.
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /api/v1/media/%s HTTP/1.1\r\nHost: tracepad\r\nAuthorization: Bearer %s\r\n\r\n", sha, testSecret)
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("media status line = %q, %v", status, err)
	}

	req, _ := http.NewRequest("GET", server.URL+"/api/v1/traces?limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("the next read = %d while a client sat on a download; want 200", res.StatusCode)
	}
}

// `?tag=` takes at most 50 distinct values, on every route that reads the
// listing's filters (spec 043 #17).
func TestTagFilterCap(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	tags := func(n int, distinct bool) string {
		query := url.Values{}
		for i := range n {
			if distinct {
				query.Add("tag", fmt.Sprintf("t%d", i))
			} else {
				query.Add("tag", "beta")
			}
		}
		return query.Encode()
	}

	expectStatus(t, h.get(t, "/api/v1/traces?"+tags(50, true)), 200)
	expectError(t, h.get(t, "/api/v1/traces?"+tags(51, true)), 400, "tag: at most 50 values")
	listed := listedIDs(t, h.get(t, "/api/v1/traces?"+tags(60, false)))
	if len(listed) != 1 || listed[0] != traceHex(1) {
		t.Errorf("sixty copies of one tag listed %v, want the one trace that carries it", listed)
	}
	expectError(t, h.get(t, "/api/v1/traces?"+tags(1000, true)), 400, "tag: at most 50 values")

	expectError(t, h.get(t, "/api/v1/traces/last?"+tags(51, true)), 400, "tag: at most 50 values")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to=2030-01-01T00:00:00Z&"+tags(51, true), nil),
		400, "tag: at most 50 values")
	h.putConfigs(t, "quality")
	expectStatus(t, h.putQueue(t, "review", "quality"), 201)
	expectError(t, h.call(t, "POST", "/api/v1/queues/review/items/from-traces?"+tags(51, true), []byte(`{}`)),
		400, "tag: at most 50 values")
}

// The system endpoint shows the read slots in use and, per project, the reads
// each bound refused (spec 043 #21).
func TestSystemReportsReadBounds(t *testing.T) {
	const timeout = 300 * time.Millisecond
	h := newReadHarness(t, timeout, 2)
	h.second(t, "other", "tp-sk-other")
	holdReads(t)

	type system struct {
		ReadSlots struct {
			Busy     int `json:"busy"`
			Capacity int `json:"capacity"`
		} `json:"read_slots"`
		Counters struct {
			TimedOut    int64 `json:"reads_timed_out"`
			RefusedBusy int64 `json:"reads_refused_busy"`
		} `json:"counters"`
	}
	read := func(secret string) system {
		t.Helper()
		rec := h.call(t, "GET", "/api/v1/system", nil, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+secret)
		})
		return decodeJSON[system](t, rec)
	}

	idle := read(testSecret)
	if idle.ReadSlots.Busy != 0 || idle.ReadSlots.Capacity != 2 {
		t.Errorf("read_slots = %+v, want none of 2 busy: the system read takes no slot", idle.ReadSlots)
	}

	// Both slots held past their deadline: the gauge still answers and shows
	// them, a third read is refused, and the held ones are stopped by the
	// deadline.
	release := make(chan struct{})
	first := h.holdRead(t, "/api/v1/traces", testSecret, release)
	second := h.holdRead(t, "/api/v1/traces", testSecret, release)
	if full := read(testSecret); full.ReadSlots.Busy != 2 {
		t.Errorf("read_slots = %+v while both are held, want 2 busy", full.ReadSlots)
	}
	expectStatus(t, h.get(t, "/api/v1/traces"), 503)
	time.Sleep(timeout)
	close(release)
	for _, done := range []chan *httptest.ResponseRecorder{first, second} {
		expectError(t, <-done, 503, "was stopped")
	}

	mine := read(testSecret)
	if mine.Counters.RefusedBusy != 1 || mine.Counters.TimedOut != 2 {
		t.Errorf("counters = %+v, want 1 refused busy and 2 timed out", mine.Counters)
	}
	// The gauge reads in a lane of its own: one at a time, so a flood of
	// them cannot take the connections the rest of the server needs.
	lane := make(chan struct{})
	gauge := h.holdRead(t, "/api/v1/system", testSecret, lane)
	expectError(t, h.get(t, "/api/v1/system"), 503, "the server is busy; retry shortly")
	close(lane)
	<-gauge

	theirs := read("tp-sk-other")
	if theirs.Counters.RefusedBusy != 0 || theirs.Counters.TimedOut != 0 {
		t.Errorf("another project's counters = %+v, want none of this project's refusals", theirs.Counters)
	}
}

// A raw body's media are put back before its status is written: they are
// reads under the deadline and in the slot like any other, and a body the
// deadline cut short is not sent as the batch the client sent (spec 043 #15,
// #16; spec 041 #8).
func TestRawBodyMediaAreReadInsideTheGate(t *testing.T) {
	const timeout = 200 * time.Millisecond
	h := newReadHarness(t, timeout, 4)
	picture := testPicture(15000, 3)
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, imageExport(t, picture))), 200)
	batches := archived(t, h)

	previous := mediaFor
	mediaFor = func(st *store.Store, ctx context.Context, projectID, sha string) (*store.MediaFile, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { mediaFor = previous })

	rec := h.get(t, "/api/v1/raw/"+itoa(batches[0].ID))
	expectError(t, rec, 503, "the read took longer than 200ms and was stopped")
}

// A read stopped by the deadline after spending most of it waiting for a slot
// was slow because the server was busy: it is told so, with Retry-After, and
// counted as refused rather than stopped (spec 043, Edge cases; #26).
func TestReadThatWaitedForItsSlotIsBusy(t *testing.T) {
	const timeout = 400 * time.Millisecond
	h := newReadHarness(t, timeout, 1)
	holdReads(t)

	release := make(chan struct{})
	first := h.holdRead(t, "/api/v1/traces", testSecret, release)
	go func() {
		time.Sleep(timeout * 3 / 4)
		close(release)
	}()
	req := httptest.NewRequest("GET", "/api/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req = req.WithContext(context.WithValue(req.Context(), stallKey{}, true))
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	<-first

	expectError(t, rec, 503, "the server is busy; retry shortly")
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	counters := decodeJSON[struct {
		Counters struct {
			TimedOut    int64 `json:"reads_timed_out"`
			RefusedBusy int64 `json:"reads_refused_busy"`
		} `json:"counters"`
	}](t, h.get(t, "/api/v1/system")).Counters
	if counters.RefusedBusy != 1 {
		t.Errorf("counters = %+v, want the read counted as refused busy", counters)
	}

	// The same read with nothing to wait for is stopped, and told to narrow.
	req = httptest.NewRequest("GET", "/api/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	req = req.WithContext(context.WithValue(req.Context(), stallKey{}, true))
	rec = httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	expectError(t, rec, 503, "the read took longer than 400ms and was stopped")
}

// Handing out the next queue item is a GET that writes: it takes no read slot,
// so a full house of reads does not refuse a claim, and no read deadline can
// answer a claim that then commits (spec 024 #5; spec 043 #26).
func TestQueueClaimIsNotAGatedRead(t *testing.T) {
	h := newReadHarness(t, 200*time.Millisecond, 1)
	h.putConfigs(t, "quality")
	expectStatus(t, h.putQueue(t, "review", "quality"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})
	holdReads(t)

	release := make(chan struct{})
	held := h.holdRead(t, "/api/v1/traces", testSecret, release)
	defer func() { close(release); <-held }()
	if item := h.next(t, "review", "ann"); item.Item == nil {
		t.Fatal("the claim handed out nothing while the one read slot was held")
	}
}

// A read that fails after the request's own write committed is `500`, never
// the `503` that asks for a retry: retrying an account's creation would be a
// conflict, and its one invitation link would be lost (spec 043 #28). The
// same failure on a plain read is the condition's `503`.
func TestAReadAfterACommittedWriteIsNotRetried(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	previous := membershipsOf
	membershipsOf = func(*store.Store, context.Context, string) ([]store.Membership, error) {
		return nil, fmt.Errorf("read memberships: %w", codedError{5}) // SQLITE_BUSY
	}
	t.Cleanup(func() { membershipsOf = previous })

	rec := h.call(t, "POST", "/api/v1/accounts", mustJSON(t, map[string]any{
		"email": "helper@example.com",
	}), asSession(owner))
	expectStatus(t, rec, http.StatusInternalServerError)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q after a committed write; a retry would repeat it", got)
	}
	rec = h.call(t, "GET", "/api/v1/accounts/"+owner.account.ID, nil, asSession(owner))
	expectError(t, rec, http.StatusServiceUnavailable, "storage is temporarily unavailable")
}

// The read deadline's ceiling is under the time a response has to be written,
// or the transport would cut a read before the deadline could answer it
// (spec 043 #28).
func TestReadTimeoutCeilingIsUnderTheWriteTimeout(t *testing.T) {
	if config.MaxReadTimeout >= writeTimeout {
		t.Errorf("TRACEPAD_READ_TIMEOUT may reach %s, but a response has %s to be written",
			config.MaxReadTimeout, writeTimeout)
	}
}

// The reads a write route makes — a dry run's counts, the selection a bulk act
// works through — take a read slot and run under the read deadline, as a
// listing does: uncounted, enough of them held every connection of the
// bounded pool for as long as their scans took (spec 043 #29).
func TestReadsInsideWritesAreBounded(t *testing.T) {
	const timeout = 200 * time.Millisecond
	h := newReadHarness(t, timeout, 1)
	seedCorpus(t, h)
	h.putConfigs(t, "quality")
	expectStatus(t, h.putQueue(t, "review", "quality"), 201)
	holdReads(t)

	requests := map[string]func() *httptest.ResponseRecorder{
		"the bulk delete's dry run": func() *httptest.ResponseRecorder {
			return h.call(t, "DELETE", "/api/v1/traces?to=2030-01-01T00:00:00Z", nil)
		},
		"the bulk delete's selection": func() *httptest.ResponseRecorder {
			return h.call(t, "DELETE", "/api/v1/traces?to=2030-01-01T00:00:00Z&confirm=test", nil)
		},
		"the queue fill's count": func() *httptest.ResponseRecorder {
			return h.call(t, "POST", "/api/v1/queues/review/items/from-traces", []byte(`{}`))
		},
		"a shrinking window's dry run": func() *httptest.ResponseRecorder {
			return h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": 1})
		},
		"an erasure's dry run": func() *httptest.ResponseRecorder {
			return h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/u1/data", nil)
		},
	}

	release := make(chan struct{})
	held := h.holdRead(t, "/api/v1/traces", testSecret, release)
	for name, request := range requests {
		t.Run(name+" waits for a slot", func(t *testing.T) {
			expectError(t, request(), 503, "the server is busy; retry shortly")
		})
	}
	close(release)
	<-held

	// Stopped by the deadline while it runs, it is told to narrow.
	previous := readAdmitted
	readAdmitted = func(ctx context.Context) { <-ctx.Done() }
	t.Cleanup(func() { readAdmitted = previous })
	for name, request := range requests {
		t.Run(name+" is stopped by the deadline", func(t *testing.T) {
			expectError(t, request(), 503, "the read took longer than 200ms and was stopped")
		})
	}
}
