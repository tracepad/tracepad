package server

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// The upload channel's lifecycle (spec 041 #28–#31): an upload URL dies with
// the key that asked for it and with its trace's deletion or erasure, a second
// identical picture writes a pending ref no younger than its bytes, and a
// project's pending refs are capped. Every refusal of the PUT is decided
// before its body is read; a refusal of a live grant is answered and the body
// then drained, up to the length the grant declared, so that the SDK reads the
// status rather than a reset.

// put PUTs a body of `size` bytes — endless when negative — at an upload URL
// and answers the status and how much of the body the server read.
func (h *harness) put(t *testing.T, upload string, size int64) (int, int64) {
	t.Helper()
	parsed, err := url.Parse(upload)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingReader{}
	var body io.Reader = counted
	if size >= 0 {
		body = io.LimitReader(counted, size)
	}
	req := httptest.NewRequest("PUT", parsed.RequestURI(), body)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "60" {
		t.Errorf("a 429 without Retry-After: 60 (%q)", rec.Header().Get("Retry-After"))
	}
	return rec.Code, counted.read
}

// putRefused PUTs a live grant the stored state refuses and answers the
// status: the body is drained after the refusal, all of it up to the length
// the grant declared and that length's worth past it. An upload that read the
// body instead answers 413 or 400.
func (h *harness) putRefused(t *testing.T, upload string, size int64) int {
	t.Helper()
	parsed, err := url.Parse(upload)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := h.server.verifyUpload(parsed.Query().Get("token"))
	if err != nil {
		t.Fatal(err)
	}
	code, read := h.put(t, upload, size)
	want := min(size, grant.Length)
	if size < 0 {
		want = grant.Length
	}
	if (code == http.StatusForbidden || code == http.StatusTooManyRequests) && read != want {
		t.Errorf("a refused body of %d: read %d, want %d", size, read, want)
	}
	return code
}

// sqlOf opens the harness's database beside its store, for what a test reads
// or seeds that no exported call does.
func (h *harness) sqlOf(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// refStates counts one trace's media refs: pending, and settled.
func (h *harness) refStates(t *testing.T, trace string) (pending, settled int) {
	t.Helper()
	if err := h.sqlOf(t).QueryRow(`SELECT COALESCE(SUM(pending), 0), COALESCE(SUM(1 - pending), 0)
	   FROM media_refs WHERE project_id = ? AND trace_id = ?`, h.project.ID, trace).Scan(&pending, &settled); err != nil {
		t.Fatal(err)
	}
	return pending, settled
}

// fillPending writes n pending refs of a body the project holds, for traces
// that have not come: the cap, reached without ten thousand uploads.
func (h *harness) fillPending(t *testing.T, sha string, n int) {
	t.Helper()
	tx, err := h.sqlOf(t).Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := tx.Exec(`INSERT INTO media_refs (sha256, project_id, trace_id, created_at, pending)
		                       VALUES (?, ?, ?, ?, 1)`, sha, h.project.ID, fmt.Sprintf("%032x", 1<<20+i),
			time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// pictureOf is a picture with its hex and base64 SHA-256.
func pictureOf(seed byte) (picture []byte, sha, hash string) {
	picture = testPicture(20000, seed)
	sum := sha256.Sum256(picture)
	return picture, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:])
}

// trace32 is a trace id of the n-th test trace.
func trace32(n int) string { return strings.Repeat("0", 30) + hex.EncodeToString([]byte{byte(n)}) }

// seedTrace stores a trace of user u1 starting at `start`.
func (h *harness) seedTrace(t *testing.T, id string, start int64) {
	t.Helper()
	h.seed(t, &model.Trace{ID: id, UserID: "u1"},
		&model.Observation{TraceID: id, ID: id[16:], Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: start, EndTime: start + ms})
}

// TestLangfuseMediaUploadDiesWithItsKey: a URL a second key asked for is
// refused once that key is revoked, before the body; one the first key asked
// for still uploads (#28).
func TestLangfuseMediaUploadDiesWithItsKey(t *testing.T) {
	h := newAdminHarness(t)
	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys", mustJSON(t, map[string]any{"scopes": allScopes}), asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	second := decodeJSON[struct {
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}](t, rec)

	picture, sha, hash := pictureOf(60)
	_, theirs := h.langfuseAsk(t, picture, probeTrace, second.SecretKey)
	_, ours := h.langfuseAsk(t, picture, trace32(2), testSecret)
	if theirs == nil || ours == nil {
		t.Fatal("an upload was not asked for")
	}
	expectStatus(t, h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/keys/"+second.PublicKey,
		nil, asAdmin), 200)

	if code := h.putRefused(t, *theirs, -1); code != http.StatusForbidden {
		t.Errorf("the revoked key's URL = %d, want 403", code)
	}
	if h.mediaHeld(t, sha) {
		t.Fatal("the revoked key's URL stored its body")
	}
	if code := h.langfusePut(t, *ours, picture, hash); code != 200 {
		t.Errorf("the live key's URL = %d, want 200", code)
	}
}

// TestLangfuseMediaDeadTokensReadNothing: a token no retry can make good — one
// from before grants named their key, one past its hour, one for another id —
// is refused having read nothing, like a forged one (#14, #28).
func TestLangfuseMediaDeadTokensReadNothing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture, sha, _ := pictureOf(65)
	_, otherSHA, _ := pictureOf(66)
	grant := uploadGrant{Project: h.project.ID, Trace: probeTrace, SHA256: sha, MimeType: "image/png",
		Length: int64(len(picture)), Expires: time.Now().Add(time.Hour).Unix(), Key: testPublic}
	sign := func(g uploadGrant, mediaID string) string {
		token, err := h.server.signUpload(g)
		if err != nil {
			t.Fatal(err)
		}
		return "/api/public/media/" + mediaID + "/upload?token=" + url.QueryEscape(token)
	}
	legacy, expired := grant, grant
	legacy.Key = ""
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	for name, path := range map[string]string{
		"a token without its key": sign(legacy, store.MediaIDFor(sha)),
		"an expired token":        sign(expired, store.MediaIDFor(sha)),
		"another id's token":      sign(grant, store.MediaIDFor(otherSHA)),
	} {
		if code, read := h.put(t, path, -1); code != http.StatusForbidden || read != 0 {
			t.Errorf("%s = %d after reading %d bytes, want 403 and none", name, code, read)
		}
	}
}

// TestLangfuseMediaRefusalBeforeTheBody: over a real connection, a refused
// upload hears its status before it has sent a byte of its body, and the
// connection is kept once the body is drained; a client that sent
// `Expect: 100-continue` hears the refusal and no `100` (#31).
func TestLangfuseMediaRefusalBeforeTheBody(t *testing.T) {
	h := newAdminHarness(t)
	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys", mustJSON(t, map[string]any{"scopes": allScopes}), asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	second := decodeJSON[struct {
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}](t, rec)
	// Larger than any fixed bound short of the API's body limit: the drain
	// reads what the grant declared.
	const size = 12 << 20
	_, upload := h.langfuseAsk(t, testPicture(size, 67), probeTrace, second.SecretKey)
	expectStatus(t, h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/keys/"+second.PublicKey,
		nil, asAdmin), 200)
	parsed, err := url.Parse(*upload)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h.server.Handler())
	t.Cleanup(server.Close)
	refusedOnTheWire(t, server, parsed.RequestURI(), size, http.StatusForbidden)

	waiting, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	fmt.Fprintf(waiting, "PUT %s HTTP/1.1\r\nHost: x\r\nContent-Type: image/png\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n",
		parsed.RequestURI(), size)
	waiting.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(waiting).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 403") {
		t.Errorf("an Expect: 100-continue PUT heard %q (%v), want 403 and no 100", line, err)
	}
}

// refusedOnTheWire PUTs `size` bytes to uri on a raw connection: the refusal
// comes before the body, on a connection kept open; it ends once the body is
// sent and drained; and the same connection answers the next request.
func refusedOnTheWire(t *testing.T, server *httptest.Server, uri string, size int, want int) {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: x\r\nContent-Type: image/png\r\nContent-Length: %d\r\n\r\n",
		uri, size)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	refusal, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("no answer before the body: %v", err)
	}
	if refusal.StatusCode != want || refusal.Close {
		t.Fatalf("the answer before the body = %d, closing %v; want %d on a kept connection",
			refusal.StatusCode, refusal.Close, want)
	}
	if _, err := conn.Write(make([]byte, size)); err != nil {
		t.Fatalf("sending the body after the refusal: %v", err)
	}
	// The refusal ends once its body is drained.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, refusal.Body); err != nil {
		t.Fatalf("the refusal did not end after the body: %v", err)
	}
	// The same connection answers the next request: the body was drained,
	// not left to close it.
	fmt.Fprintf(conn, "GET /api/v1/projects HTTP/1.1\r\nHost: x\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := http.ReadResponse(reader, nil); err != nil {
		t.Errorf("the connection after a drained refusal: %v", err)
	}
}

// TestLangfuseMediaLookupFailureDrains: a PUT whose grant the store cannot
// check hears the failure before its body, and the body is drained like a
// refusal's: the SDK reads a status it can retry, not a reset (#31).
func TestLangfuseMediaLookupFailureDrains(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const size = 1 << 20
	_, upload := h.langfuseAsk(t, testPicture(size, 68), probeTrace, testSecret)
	parsed, err := url.Parse(*upload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sqlOf(t).Exec(`DROP TABLE media_voided`); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h.server.Handler())
	t.Cleanup(server.Close)
	refusedOnTheWire(t, server, parsed.RequestURI(), size, http.StatusInternalServerError)
}

// TestLangfuseMediaUploadAfterDeletion: deleting a trace — or erasing its
// user — refuses its upload URLs issued before, before the body, and the asks
// for it within the hour after, for a picture the project holds or not; a URL
// for another trace, from before or after, uploads. A retention sweep voids
// nothing (#29).
func TestLangfuseMediaUploadAfterDeletion(t *testing.T) {
	for _, how := range []string{"delete", "erase"} {
		t.Run(how, func(t *testing.T) {
			h := newAdminHarness(t)
			h.seedTrace(t, probeTrace, seedBase)

			picture, sha, hash := pictureOf(61)
			other, _, otherHash := pictureOf(62)
			_, forDeleted := h.langfuseAsk(t, picture, probeTrace, testSecret)
			_, forAnother := h.langfuseAsk(t, other, trace32(3), testSecret)
			if forDeleted == nil || forAnother == nil {
				t.Fatal("an upload was not asked for")
			}

			switch how {
			case "delete":
				expectStatus(t, h.call(t, "DELETE", "/api/v1/traces/"+probeTrace+"?confirm="+probeTrace, nil), 200)
			case "erase":
				expectStatus(t, h.call(t, "DELETE",
					"/api/v1/projects/"+h.project.ID+"/users/u1/data?confirm=u1", nil), 200)
			}

			if code := h.putRefused(t, *forDeleted, -1); code != http.StatusForbidden {
				t.Errorf("the deleted trace's URL from before the %s = %d, want 403", how, code)
			}
			ask := func(body []byte, hash string) *httptest.ResponseRecorder {
				return h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
					"traceId": probeTrace, "contentType": "image/png", "contentLength": len(body),
					"sha256Hash": hash, "field": "input",
				}))
			}
			expectError(t, ask(picture, hash), http.StatusForbidden, "deleted or erased within the hour")
			if h.mediaHeld(t, sha) {
				t.Fatal("a voided URL stored its body")
			}
			if code := h.langfusePut(t, *forAnother, other, otherHash); code != 200 {
				t.Errorf("another trace's URL from before the %s = %d, want 200", how, code)
			}
			// A picture the project holds is refused for the removed trace
			// too, and no ref is written for it.
			expectError(t, ask(other, otherHash), http.StatusForbidden, "deleted or erased within the hour")
			if pending, settled := h.refStates(t, probeTrace); pending+settled != 0 {
				t.Errorf("the ask for a removed trace wrote %d refs", pending+settled)
			}
			// The trace sent again under its id is here: its upload is taken.
			h.seedTrace(t, probeTrace, seedBase)
			_, again := h.langfuseAsk(t, picture, probeTrace, testSecret)
			if code := h.langfusePut(t, *again, picture, hash); code != 200 {
				t.Errorf("the URL of a trace sent again after the %s = %d, want 200", how, code)
			}
		})
	}

	t.Run("retention", func(t *testing.T) {
		h := newAdminHarness(t)
		picture, _, hash := pictureOf(63)
		_, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
		if err := h.sweeper.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		if code := h.langfusePut(t, *upload, picture, hash); code != 200 {
			t.Errorf("a URL across a retention sweep = %d, want 200", code)
		}
	})
}

// TestLangfuseMediaDeletionInRounds: a deletion or an erasure over three hours
// runs three chunks, and a URL for a trace a later chunk takes — asked for
// before the request or while it ran — is refused, while a URL for a trace it
// leaves uploads (#29).
func TestLangfuseMediaDeletionInRounds(t *testing.T) {
	for _, how := range []string{"delete", "erase"} {
		t.Run(how, func(t *testing.T) {
			h := newAdminHarness(t)
			for i := range 3 {
				h.seedTrace(t, trace32(40+i), seedBase+int64(i)*3600*1000*ms)
			}
			picture, sha, _ := pictureOf(90)
			other, _, otherHash := pictureOf(91)
			_, taken := h.langfuseAsk(t, picture, trace32(40), testSecret)
			_, left := h.langfuseAsk(t, other, trace32(50), testSecret)
			switch how {
			case "delete":
				to := url.QueryEscape(formatTime(seedBase + 4*3600*1000*ms))
				rec := h.call(t, "DELETE", "/api/v1/traces?to="+to+"&confirm="+url.QueryEscape(h.project.Name), nil)
				expectStatus(t, rec, 200)
				if got := decodeJSON[deleteAnswer](t, rec); got.Deleted["traces"] != 3 {
					t.Fatalf("deleted = %+v, want three traces", got)
				}
			case "erase":
				expectStatus(t, h.call(t, "DELETE",
					"/api/v1/projects/"+h.project.ID+"/users/u1/data?confirm=u1", nil), 200)
			}
			var voided int
			if err := h.sqlOf(t).QueryRow(`SELECT COUNT(*) FROM media_voided WHERE project_id = ?`,
				h.project.ID).Scan(&voided); err != nil || voided != 3 {
				t.Fatalf("voided traces = %d (%v), want the three", voided, err)
			}
			if code := h.putRefused(t, *taken, -1); code != http.StatusForbidden || h.mediaHeld(t, sha) {
				t.Errorf("a URL for a trace the %s took = %d, want 403 and nothing kept", how, code)
			}
			if code := h.langfusePut(t, *left, other, otherHash); code != 200 {
				t.Errorf("a URL for a trace the %s left = %d, want 200", how, code)
			}
		})
	}
}

// TestLangfuseMediaCapNeverReachesTheWriter: at the cap, the ask — for an
// upload URL or the null answer — and the PUT are refused by reads of the
// pool, before any job is submitted — with the
// writer closed they still answer 429, where a job would have met a writer
// shutting down (#31).
func TestLangfuseMediaCapNeverReachesTheWriter(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	held, sha, heldHash := pictureOf(92)
	_, first := h.langfuseAsk(t, held, trace32(70), testSecret)
	if code := h.langfusePut(t, *first, held, heldHash); code != 200 {
		t.Fatalf("the first upload = %d", code)
	}
	picture, _, hash := pictureOf(93)
	_, upload := h.langfuseAsk(t, picture, trace32(71), testSecret)
	h.fillPending(t, sha, store.MaxPendingMediaRefs-1)
	if err := h.writer.Close(); err != nil {
		t.Fatal(err)
	}
	rec := h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
		"traceId": trace32(72), "contentType": "image/png", "contentLength": len(picture),
		"sha256Hash": hash, "field": "input",
	}))
	expectError(t, rec, http.StatusTooManyRequests, "waiting for their traces")
	if code := h.putRefused(t, *upload, 1000); code != http.StatusTooManyRequests {
		t.Errorf("the PUT at the cap with the writer closed = %d, want 429", code)
	}
	// The null answer for a held picture and a trace not here: the same
	// read, before any job.
	rec = h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
		"traceId": trace32(73), "contentType": "image/png", "contentLength": len(held),
		"sha256Hash": heldHash, "field": "input",
	}))
	expectError(t, rec, http.StatusTooManyRequests, "waiting for their traces")
}

// TestLangfuseMediaNullAnswerThenSpans: the project holds X; asked for X for a
// trace that is not here, it answers null and writes a pending ref; the
// trace's spans, carrying the SDK's string, resolve it and settle the ref
// (#30).
func TestLangfuseMediaNullAnswerThenSpans(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture, sha, hash := pictureOf(64)
	_, upload := h.langfuseAsk(t, picture, trace32(4), testSecret)
	if code := h.langfusePut(t, *upload, picture, hash); code != 200 {
		t.Fatalf("the first upload = %d", code)
	}
	refs := func(trace string) (pending, settled int) { return h.refStates(t, trace) }

	mediaID, again := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if again != nil {
		t.Fatalf("a second identical picture was asked for: %s", *again)
	}
	if p, s := refs(probeTrace); p != 1 || s != 0 {
		t.Fatalf("the null answer wrote %d pending and %d settled refs for a trace not here, want one pending", p, s)
	}

	reference := "@@@langfuseMedia:type=image/png|id=" + mediaID + "|source=base64_data_uri@@@"
	input, _ := json.Marshal([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}}}}})
	expectStatus(t, h.post(t, "/api/public/otel/v1/traces",
		encodeExport(t, otlptest.SpanWith("langfuse.observation.input", string(input)))), 200)
	if got := h.observationInput(t); !strings.Contains(got, `"tracepad_media":"`+sha+`"`) {
		t.Errorf("the spans' input = %s, want the SDK's string resolved", got)
	}
	if p, s := refs(probeTrace); p != 0 || s != 1 {
		t.Errorf("after the spans the trace has %d pending and %d settled refs, want one settled", p, s)
	}
}

// TestLangfuseMediaPendingCap: at the cap, an ask for a trace the project does
// not have — with an upload URL or the null answer — and the PUT of a URL
// whose ref would be pending answer 429 with Retry-After, the PUT before its
// body; an ask for a trace the project has is not refused, nor the retry of a
// PUT that was stored, and once a trace arrives there is room again (#31).
func TestLangfuseMediaPendingCap(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	// The first for the trace the test's span will carry, so that its
	// arrival settles that ref.
	traces := []string{probeTrace, trace32(11), trace32(12), trace32(13)}
	var uploads, mediaIDs []string
	for i := range 4 {
		picture, _, _ := pictureOf(byte(70 + i))
		mediaID, upload := h.langfuseAsk(t, picture, traces[i], testSecret)
		if upload == nil {
			t.Fatalf("ask %d got no URL", i)
		}
		uploads, mediaIDs = append(uploads, *upload), append(mediaIDs, mediaID)
	}
	for i := range 3 {
		picture, _, hash := pictureOf(byte(70 + i))
		if code := h.langfusePut(t, uploads[i], picture, hash); code != 200 {
			t.Fatalf("upload %d = %d", i, code)
		}
	}
	// The cap, less the three: refs of the first picture for traces that
	// have not come.
	_, filler, _ := pictureOf(70)
	h.fillPending(t, filler, store.MaxPendingMediaRefs-3)

	picture, _, _ := pictureOf(80)
	sum := sha256.Sum256(picture)
	rec := h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
		"traceId": trace32(20), "contentType": "image/png", "contentLength": len(picture),
		"sha256Hash": base64.StdEncoding.EncodeToString(sum[:]), "field": "input",
	}))
	expectError(t, rec, http.StatusTooManyRequests, "waiting for their traces")
	if rec.Header().Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", rec.Header().Get("Retry-After"))
	}
	if code := h.putRefused(t, uploads[3], -1); code != http.StatusTooManyRequests {
		t.Errorf("the fourth PUT at the cap = %d, want 429", code)
	}
	// A picture of the SDK's size is drained whole, and the connection kept.
	if code := h.putRefused(t, uploads[3], 2<<20); code != http.StatusTooManyRequests {
		t.Errorf("the fourth PUT of 2 MB at the cap = %d, want 429", code)
	}
	// The null answer for a trace not here would be a pending ref too.
	held, _, heldHash := pictureOf(70)
	rec = h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
		"traceId": trace32(22), "contentType": "image/png", "contentLength": len(held),
		"sha256Hash": heldHash, "field": "input",
	}))
	expectError(t, rec, http.StatusTooManyRequests, "waiting for their traces")
	// The SDK's retry of an upload that was stored: its ref is there.
	if code := h.langfusePut(t, uploads[0], held, heldHash); code != 200 {
		t.Errorf("a repeated PUT at the cap = %d, want 200", code)
	}

	// A trace the project has settles its ref, so it is never refused.
	h.seed(t, &model.Trace{ID: trace32(21)})
	if _, upload := h.langfuseAsk(t, picture, trace32(21), testSecret); upload == nil {
		t.Error("an ask for a stored trace at the cap got no URL")
	}

	// One of the three traces arrives with its picture: its ref settles,
	// and there is room.
	reference := "@@@langfuseMedia:type=image/png|id=" + mediaIDs[0] + "|source=base64_data_uri@@@"
	input, _ := json.Marshal([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}}}}})
	expectStatus(t, h.post(t, "/api/public/otel/v1/traces",
		encodeExport(t, otlptest.SpanWith("langfuse.observation.input", string(input)))), 200)
	fourth, _, hash := pictureOf(73)
	if code := h.langfusePut(t, uploads[3], fourth, hash); code != 200 {
		t.Errorf("the fourth PUT once there is room = %d, want 200", code)
	}
}
