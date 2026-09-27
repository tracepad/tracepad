package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
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
// the key that asked for it and with any trace deletion after it, a second
// identical picture writes a pending ref no younger than its bytes, and a
// project's pending refs are capped. Every refusal of the PUT is decided
// before its body is read, and the body is then drained, up to a bound, so
// that the SDK reads the status rather than a reset.

// putRefused PUTs a body of `size` bytes — endless when negative — at an
// upload URL and answers the status. A refusal decided before the body drains
// it and nothing more: all of a body up to the bound, keeping the connection,
// and past it the bound's worth, closing it. An upload that read the body
// instead answers 413 or 400.
func (h *harness) putRefused(t *testing.T, upload string, size int64) int {
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
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusTooManyRequests {
		closed := rec.Header().Get("Connection") == "close"
		switch {
		case size < 0 && (counted.read != uploadDrainMax+1 || !closed):
			t.Errorf("an endless refused body: read %d, closed %v; want %d and closed",
				counted.read, closed, uploadDrainMax+1)
		case size >= 0 && (counted.read != size || closed):
			t.Errorf("a refused body of %d: read %d, closed %v; want all of it and kept", size, counted.read, closed)
		}
	}
	return rec.Code
}

// pictureOf is a picture with its hex and base64 SHA-256.
func pictureOf(seed byte) (picture []byte, sha, hash string) {
	picture = testPicture(20000, seed)
	sum := sha256.Sum256(picture)
	return picture, hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(sum[:])
}

// trace32 is a trace id of the n-th test trace.
func trace32(n int) string { return strings.Repeat("0", 30) + hex.EncodeToString([]byte{byte(n)}) }

// TestLangfuseMediaUploadDiesWithItsKey: a URL a second key asked for is
// refused once that key is revoked, before the body; one the first key asked
// for still uploads. A token from before grants named their key is refused
// the same way (#28).
func TestLangfuseMediaUploadDiesWithItsKey(t *testing.T) {
	h := newAdminHarness(t)
	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys", nil, asAdmin)
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

	// A token signed the old way — valid signature, no key.
	legacy, err := h.server.signUpload(uploadGrant{
		Project: h.project.ID, Trace: probeTrace, SHA256: sha, MimeType: "image/png",
		Length: int64(len(picture)), Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/public/media/" + store.MediaIDFor(sha) + "/upload?token=" + url.QueryEscape(legacy)
	if code := h.putRefused(t, path, -1); code != http.StatusForbidden {
		t.Errorf("a token without its key = %d, want 403", code)
	}
}

// TestLangfuseMediaUploadAfterDeletion: deleting a trace — or erasing its
// user — voids every upload URL the project issued before, before the body;
// a URL asked for afterwards, for the deleted id too, uploads. A retention
// sweep voids nothing (#29).
func TestLangfuseMediaUploadAfterDeletion(t *testing.T) {
	for _, how := range []string{"delete", "erase"} {
		t.Run(how, func(t *testing.T) {
			h := newAdminHarness(t)
			h.seed(t, &model.Trace{ID: probeTrace, UserID: "u1"},
				&model.Observation{TraceID: probeTrace, ID: probeSpan, Type: model.TypeSpan,
					Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

			picture, sha, hash := pictureOf(61)
			other, otherSHA, _ := pictureOf(62)
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

			for name, upload := range map[string]string{"the deleted trace's": *forDeleted, "another trace's": *forAnother} {
				if code := h.putRefused(t, upload, -1); code != http.StatusForbidden {
					t.Errorf("%s URL from before the %s = %d, want 403", name, how, code)
				}
			}
			if h.mediaHeld(t, sha) || h.mediaHeld(t, otherSHA) {
				t.Fatal("a voided URL stored its body")
			}

			// Asked for afterwards: new data, for any trace.
			_, after := h.langfuseAsk(t, picture, probeTrace, testSecret)
			if after == nil {
				t.Fatal("no upload URL after the deletion")
			}
			if code := h.langfusePut(t, *after, picture, hash); code != 200 {
				t.Errorf("a URL asked for after the %s = %d, want 200", how, code)
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
	refs := func(trace string) (pending, settled int) {
		t.Helper()
		pending, settled, err := h.store.MediaRefStates(h.project.ID, trace)
		if err != nil {
			t.Fatal(err)
		}
		return pending, settled
	}

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
	h.store.SetMaxPendingMediaRefs(3)

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
