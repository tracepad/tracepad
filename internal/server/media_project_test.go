package server

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// What a project is answered about a body is its own (spec 041 #25, #27):
// another project having sent the same bytes first changes neither the type,
// nor the time, nor what a deletion counts.

// typedExport is one span carrying the picture as a data URL of the given
// type.
func typedExport(t *testing.T, picture []byte, mime string) []byte {
	t.Helper()
	return encodeExport(t, otlptest.SpanWith("langfuse.observation.input",
		"data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(picture)))
}

func TestLangfuseMediaGetIsTheProjects(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.second(t, "b", "tp-sk-b")
	h.second(t, "c", "tp-sk-c")
	picture := testPicture(9000, 30)
	sha := hexSHA(picture)
	mediaID := store.MediaIDFor(sha)

	expectStatus(t, h.post(t, "/v1/traces", typedExport(t, picture, "image/png")), 200)
	// Apart on the clock, so that B's hold cannot start when A's did.
	time.Sleep(2 * time.Millisecond)
	expectStatus(t, h.post(t, "/v1/traces", typedExport(t, picture, "image/webp"),
		asKey("tp-sk-b")), 200)
	expectStatus(t, h.post(t, "/v1/traces", typedExport(t, picture, "text/html"),
		asKey("tp-sk-c")), 200)

	record := func(secret string) map[string]any {
		t.Helper()
		rec := h.call(t, "GET", "/api/public/media/"+mediaID, nil, asKey(secret))
		expectStatus(t, rec, 200)
		return decodeJSON[map[string]any](t, rec)
	}
	a, b := record(testSecret), record("tp-sk-b")
	if a["contentType"] != "image/png" || b["contentType"] != "image/webp" {
		t.Errorf("contentType: A %v, B %v; want each its own", a["contentType"], b["contentType"])
	}
	if b["contentLength"] != float64(9000) {
		t.Errorf("contentLength = %v, want the body's size", b["contentLength"])
	}
	aAt, errA := time.Parse(time.RFC3339Nano, a["uploadedAt"].(string))
	bAt, errB := time.Parse(time.RFC3339Nano, b["uploadedAt"].(string))
	if errA != nil || errB != nil || !bAt.After(aAt) {
		t.Errorf("uploadedAt: A %v, B %v; want B's own, later than A's", a["uploadedAt"], b["uploadedAt"])
	}

	for _, c := range []struct {
		secret, mime string
		attachment   bool
	}{
		{testSecret, "image/png", false},
		{"tp-sk-b", "image/webp", false},
		{"tp-sk-c", "text/html", true},
	} {
		rec := h.call(t, "GET", "/api/v1/media/"+sha, nil, asKey(c.secret))
		expectStatus(t, rec, 200)
		if got := rec.Header().Get("Content-Type"); got != c.mime {
			t.Errorf("%s: Content-Type = %q, want %q", c.mime, got, c.mime)
		}
		if got := strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment"); got != c.attachment {
			t.Errorf("%s: attachment = %v, want %v", c.mime, got, c.attachment)
		}
	}
}

// B's deletion of its own trace counts the body B stops holding, whether or
// not A keeps it (#27): the dry run and the confirmed answer alike. Today
// both answer 0 because A keeps the bytes.
func TestMediaDeletionCountsAreTheProjectsOverHTTP(t *testing.T) {
	// No raw archive: a raw batch would keep B's hold past its trace (#12).
	cfg := &config.Config{Listen: ":0", StoreRaw: false, MaxBodyBytes: config.DefaultMaxBodyBytes}
	h := newHarness(t, cfg, store.WriterOptions{})
	h.second(t, "b", "tp-sk-b")
	picture := testPicture(7000, 31)
	sha := hexSHA(picture)
	expectStatus(t, h.post(t, "/v1/traces", typedExport(t, picture, "image/png")), 200)
	expectStatus(t, h.post(t, "/v1/traces", typedExport(t, picture, "image/png"),
		asKey("tp-sk-b")), 200)

	path := "/api/v1/traces/" + probeTrace
	rec := h.call(t, "DELETE", path, nil, asKey("tp-sk-b"))
	expectStatus(t, rec, 200)
	preview := decodeJSON[deletePreview](t, rec)
	if preview.WouldDelete["media"] != 1 || preview.WouldDelete["media_bytes"] != 7000 {
		t.Errorf("B's dry run = %v, want the body it stops holding", preview.WouldDelete)
	}
	rec = h.call(t, "DELETE", path+"?confirm="+probeTrace, nil, asKey("tp-sk-b"))
	expectStatus(t, rec, 200)
	answer := decodeJSON[deleteAnswer](t, rec)
	if answer.Deleted["media"] != 1 || answer.Deleted["media_bytes"] != 7000 {
		t.Errorf("B's deletion = %v, want the body it stopped holding", answer.Deleted)
	}
	expectStatus(t, h.call(t, "GET", "/api/v1/media/"+sha, nil, asKey("tp-sk-b")), 404)
	expectStatus(t, h.get(t, "/api/v1/media/"+sha), 200)
}
