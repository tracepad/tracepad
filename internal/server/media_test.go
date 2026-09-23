package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/storetest"
)

// Media over HTTP (spec 041, Testing — API, raw, setting, bridge).

const probeTrace = "00112233445566778899aabbccddeeff"
const probeSpan = "0011223344556677"

func testPicture(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*13) ^ seed
	}
	return out
}

func hexSHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// imageExport is one span whose input is an OpenAI message with the picture
// as a data URL.
func imageExport(t *testing.T, picture []byte) []*tracepb.ResourceSpans {
	t.Helper()
	messages, err := json.Marshal([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is in this picture"},
		map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return otlptest.SpanWith("gen_ai.input.messages", string(messages))
}

func encodeExport(t *testing.T, export []*tracepb.ResourceSpans) []byte {
	t.Helper()
	body, err := mapping.EncodeExportRequest(export)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (h *harness) observationInput(t *testing.T) string {
	t.Helper()
	rec := h.get(t, "/api/v1/observations/"+probeSpan+"/io")
	expectStatus(t, rec, 200)
	var io struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &io); err != nil {
		t.Fatal(err)
	}
	return string(io.Input)
}

// Ingest takes the picture out, the trace carries the reference, the bytes
// come back by hash to this project and to no other, and the raw archive holds
// the body without the picture.
func TestMediaIngestAndRead(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(20000, 1)
	sha := hexSHA(picture)
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, imageExport(t, picture))), 200)

	input := h.observationInput(t)
	if strings.Contains(input, base64.StdEncoding.EncodeToString(picture)) {
		t.Fatal("the stored input still carries the picture")
	}
	want := `{"mime_type":"image/png","size":20000,"tracepad_media":"` + sha + `"}`
	if !strings.Contains(input, want) {
		t.Fatalf("input lacks the reference %s: %s", want, input)
	}

	rec := h.get(t, "/api/v1/media/"+sha)
	expectStatus(t, rec, 200)
	if !bytes.Equal(rec.Body.Bytes(), picture) {
		t.Error("the media endpoint answered other bytes")
	}
	for header, value := range map[string]string{
		"Content-Type":           "image/png",
		"Cache-Control":          "private, max-age=31536000, immutable",
		"X-Content-Type-Options": "nosniff",
		"Vary":                   "Authorization, Cookie, X-Tracepad-Project",
	} {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want a sandbox", csp)
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Error("an image is offered as a download")
	}

	h.second(t, "other", "tp-sk-other")
	expectStatus(t, h.call(t, "GET", "/api/v1/media/"+sha, nil, asKey("tp-sk-other")), 404)
	expectStatus(t, h.get(t, "/api/v1/media/"+strings.Repeat("0", 64)), 404)
	expectStatus(t, h.get(t, "/api/v1/media/not-a-hash"), 400)

	batches := archived(t, h)
	if len(batches) != 1 {
		t.Fatalf("%d raw batches, want 1", len(batches))
	}
	if bytes.Contains(batches[0].Body, []byte(base64.StdEncoding.EncodeToString(picture))) {
		t.Error("the raw body still carries the picture")
	}
	if !bytes.Contains(batches[0].Body, []byte(sha)) {
		t.Error("the raw body does not carry the reference")
	}

	rec = h.get(t, "/api/v1/system")
	expectStatus(t, rec, 200)
	system := decodeJSON[struct {
		Media struct {
			Setting string `json:"setting"`
			Count   int64  `json:"count"`
			Bytes   int64  `json:"bytes"`
		} `json:"media"`
	}](t, rec)
	if system.Media.Setting != "store" || system.Media.Count != 1 || system.Media.Bytes != 20000 {
		t.Errorf("system media = %+v", system.Media)
	}
}

// A non-image body is a download.
func TestMediaDownloadDisposition(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	pdf := testPicture(6000, 2)
	export := otlptest.SpanWith("langfuse.observation.input",
		"data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(pdf))
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, export)), 200)
	rec := h.get(t, "/api/v1/media/"+hexSHA(pdf))
	expectStatus(t, rec, 200)
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}
}

// The raw body endpoint — what the export replays — puts the picture back
// (#8), and posting what it answers into a Tracepad maps to the same
// observation (#5), in both encodings.
func TestMediaRawReplay(t *testing.T) {
	for _, encoding := range []string{"protobuf", "json"} {
		t.Run(encoding, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})
			picture := testPicture(15000, 3)
			export := imageExport(t, picture)
			var body []byte
			contentType := "application/x-protobuf"
			if encoding == "json" {
				var err error
				if body, err = otlptest.JSONBody(export); err != nil {
					t.Fatal(err)
				}
				contentType = "application/json"
			} else {
				body = encodeExport(t, export)
			}
			withType := func(r *http.Request) { r.Header.Set("Content-Type", contentType) }
			expectStatus(t, h.post(t, "/v1/traces", body, withType), 200)
			first := h.observationInput(t)

			batches := archived(t, h)
			rec := h.get(t, "/api/v1/raw/"+itoa(batches[0].ID))
			expectStatus(t, rec, 200)
			whole := rec.Body.Bytes()
			if !bytes.Contains(whole, []byte(base64.StdEncoding.EncodeToString(picture))) {
				t.Fatal("the raw body endpoint did not put the picture back")
			}

			// Into a second Tracepad, as `tracepad export --otlp` would.
			other := newHarness(t, nil, store.WriterOptions{})
			expectStatus(t, other.post(t, "/v1/traces", whole, withType), 200)
			if again := other.observationInput(t); again != first {
				t.Errorf("replayed input =\n%s\nwant\n%s", again, first)
			}
			expectStatus(t, other.get(t, "/api/v1/media/"+hexSHA(picture)), 200)
		})
	}
}

// The placeholder setting keeps no body and says so (#6).
func TestMediaPlaceholderSetting(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"media": "placeholder"})
	expectStatus(t, rec, 200)
	if got := decodeJSON[struct {
		Media string `json:"media"`
	}](t, rec); got.Media != "placeholder" {
		t.Fatalf("project media = %q", got.Media)
	}
	expectStatus(t, h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"media": "sometimes"}), 400)

	picture := testPicture(9000, 4)
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, imageExport(t, picture))), 200)
	input := h.observationInput(t)
	if !strings.Contains(input, `"stored":false`) || !strings.Contains(input, hexSHA(picture)) {
		t.Errorf("input = %s, want a reference with stored: false", input)
	}
	expectStatus(t, h.get(t, "/api/v1/media/"+hexSHA(picture)), 404)
	if summary, err := h.store.MediaSummary(h.project.ID); err != nil || summary.Count != 0 {
		t.Errorf("media under the placeholder setting = %+v, %v", summary, err)
	}
}

// mediaHeld reports whether the project can read a body — which, for a
// project that is the only one to have sent it, is whether it is stored.
func (h *harness) mediaHeld(t *testing.T, sha string) bool {
	t.Helper()
	file, err := h.store.MediaFor(h.project.ID, sha)
	if err != nil {
		t.Fatal(err)
	}
	return file != nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The Langfuse media channel (#9), call by call as the SDK makes them.
func TestLangfuseMediaChannel(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(30000, 5)
	sum := sha256.Sum256(picture)
	sha := hex.EncodeToString(sum[:])
	hash := base64.StdEncoding.EncodeToString(sum[:])
	mediaID := strings.NewReplacer("+", "-", "/", "_").Replace(hash)[:22]

	ask := func(t *testing.T, trace string, secret string) (string, *string) {
		return h.langfuseAsk(t, picture, trace, secret)
	}
	put := func(t *testing.T, upload string, body []byte) int {
		return h.langfusePut(t, upload, body, hash)
	}

	id, upload := ask(t, probeTrace, testSecret)
	if id != mediaID {
		t.Fatalf("mediaId = %q, want the SDK's derivation %q", id, mediaID)
	}
	if upload == nil || !strings.Contains(*upload, "/api/public/media/"+mediaID+"/upload?token=") {
		t.Fatalf("uploadUrl = %v, want a presigned URL on this server", upload)
	}

	// No token, a forged one, the wrong bytes: nothing is stored.
	if code := put(t, "/api/public/media/"+mediaID+"/upload", picture); code != 400 && code != 403 {
		t.Errorf("a PUT without a token = %d", code)
	}
	if code := put(t, *upload+"x", picture); code != 403 {
		t.Errorf("a PUT with a forged token = %d, want 403", code)
	}
	if code := put(t, *upload, testPicture(30000, 6)); code != 400 {
		t.Errorf("a PUT of other bytes = %d, want 400", code)
	}
	if h.mediaHeld(t, sha) {
		t.Fatal("a body was stored before a good upload")
	}
	if code := put(t, *upload, picture); code != 200 {
		t.Fatalf("the upload = %d, want 200", code)
	}
	expectStatus(t, h.send(t, "PATCH", "/api/public/media/"+mediaID, map[string]any{
		"uploadedAt": "2026-09-23T10:00:00Z", "uploadHttpStatus": 200, "uploadHttpError": nil, "uploadTimeMs": 12}), 204)

	// A second identical picture asks for no bytes.
	if _, again := ask(t, strings.Repeat("ab", 16), testSecret); again != nil {
		t.Errorf("the second identical upload got uploadUrl %q, want null", *again)
	}
	// Another project naming the same hash must prove it has the bytes.
	h.second(t, "other", "tp-sk-other")
	if _, theirs := ask(t, probeTrace, "tp-sk-other"); theirs == nil {
		t.Error("another project skipped the upload on the hash alone")
	}

	// The span arrives with the SDK's reference string, which is rewritten.
	reference := "@@@langfuseMedia:type=image/png|id=" + mediaID + "|source=base64_data_uri@@@"
	input, _ := json.Marshal([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}}}}})
	export := otlptest.SpanWith("langfuse.observation.input", string(input))
	expectStatus(t, h.post(t, "/api/public/otel/v1/traces", encodeExport(t, export)), 200)
	got := h.observationInput(t)
	want := `{"mime_type":"image/png","size":30000,"tracepad_media":"` + sha + `"}`
	if !strings.Contains(got, want) {
		t.Errorf("input = %s, want the reference rewritten to %s", got, want)
	}

	rec := h.get(t, "/api/public/media/"+mediaID)
	expectStatus(t, rec, 200)
	record := decodeJSON[map[string]any](t, rec)
	if record["contentType"] != "image/png" || record["contentLength"] != float64(30000) ||
		!strings.HasSuffix(record["url"].(string), "/api/v1/media/"+sha) {
		t.Errorf("media record = %v", record)
	}
	expectStatus(t, h.get(t, "/api/public/media/AAAAAAAAAAAAAAAAAAAAAA"), 404)
}

// langfuseAsk is the SDK's POST /api/public/media for a picture: the media id
// and the upload URL the server answers.
func (h *harness) langfuseAsk(t *testing.T, picture []byte, trace, secret string) (string, *string) {
	t.Helper()
	sum := sha256.Sum256(picture)
	rec := h.call(t, "POST", "/api/public/media", mustJSON(t, map[string]any{
		"traceId": trace, "observationId": probeSpan, "contentType": "image/png",
		"contentLength": len(picture), "sha256Hash": base64.StdEncoding.EncodeToString(sum[:]),
		"field": "input",
	}), asKey(secret))
	expectStatus(t, rec, 200)
	var answer struct {
		MediaID   string  `json:"mediaId"`
		UploadURL *string `json:"uploadUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	return answer.MediaID, answer.UploadURL
}

// langfusePut is the SDK's bare PUT of the upload URL, with no credential.
func (h *harness) langfusePut(t *testing.T, upload string, body []byte, hash string) int {
	t.Helper()
	parsed, err := url.Parse(upload)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", parsed.RequestURI(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("x-amz-checksum-sha256", hash)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// A span that overtakes its own upload is stored with the SDK's string; once
// the PUT lands, every read answers the reference in its place, while the
// stored payload and the raw archive keep the string as sent (Decision 21).
func TestLangfuseMediaSpanBeforeUpload(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(30000, 7)
	sum := sha256.Sum256(picture)
	sha := hex.EncodeToString(sum[:])
	hash := base64.StdEncoding.EncodeToString(sum[:])

	mediaID, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil {
		t.Fatal("the first upload was not asked for")
	}
	reference := "@@@langfuseMedia:type=image/png|id=" + mediaID + "|source=base64_data_uri@@@"
	input, _ := json.Marshal([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": reference}},
	}}})
	export := otlptest.SpanWith("langfuse.observation.input", string(input))
	expectStatus(t, h.post(t, "/api/public/otel/v1/traces", encodeExport(t, export)), 200)
	if got := h.observationInput(t); !strings.Contains(got, reference) || strings.Contains(got, sha) {
		t.Fatalf("before the upload the input = %s, want the SDK's string", got)
	}

	if code := h.langfusePut(t, *upload, picture, hash); code != 200 {
		t.Fatalf("the upload = %d, want 200", code)
	}
	got := h.observationInput(t)
	ref := `{"mime_type":"image/png","size":30000,"tracepad_media":"` + sha + `"}`
	for _, want := range []string{`"url":` + ref, `"data":` + ref} {
		if !strings.Contains(got, want) {
			t.Errorf("after the upload the input lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, reference) {
		t.Errorf("the SDK's string is still read after the upload: %s", got)
	}

	batches := archived(t, h)
	rec := h.get(t, "/api/v1/raw/"+itoa(batches[0].ID))
	expectStatus(t, rec, 200)
	if !bytes.Contains(rec.Body.Bytes(), []byte(reference)) {
		t.Error("the raw archive lost the SDK's string; it is kept as sent")
	}
}

// An upload URL issued before a restart still uploads after it: the key it is
// signed with is the database's, not the process's (Decision 22).
func TestLangfuseMediaUploadSurvivesRestart(t *testing.T) {
	captureLogs(t)
	path := storetest.Path(t)
	start := func() (*harness, func()) {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		project, err := st.ProjectByName("test")
		if err == nil && project == nil {
			project, err = st.CreateProject("test", store.KeyPair{PublicKey: testPublic, Secret: testSecret})
		}
		if err != nil {
			t.Fatal(err)
		}
		writer, err := st.NewWriter(storetest.Writes)
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes}
		h := &harness{server: New(cfg, "test", st, writer, st.NewSweeper(writer, store.SweepOptions{})),
			store: st, writer: writer, project: project}
		return h, func() {
			writer.Close()
			st.Close()
		}
	}

	picture := testPicture(20000, 8)
	sum := sha256.Sum256(picture)
	before, stop := start()
	_, upload := before.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil {
		t.Fatal("the upload was not asked for")
	}
	stop()

	after, stop := start()
	defer stop()
	if code := after.langfusePut(t, *upload, picture, base64.StdEncoding.EncodeToString(sum[:])); code != 200 {
		t.Fatalf("the upload after a restart = %d, want 200", code)
	}
	if !after.mediaHeld(t, hex.EncodeToString(sum[:])) {
		t.Error("the body uploaded after the restart was not stored")
	}
}

// Under the placeholder setting the channel asks for nothing, and the SDK's
// reference string is left as the client wrote it.
func TestLangfuseMediaPlaceholder(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"media": "placeholder"}), 200)
	sum := sha256.Sum256([]byte("anything"))
	rec := h.send(t, "POST", "/api/public/media", map[string]any{
		"traceId": probeTrace, "contentType": "image/png", "contentLength": 8,
		"sha256Hash": base64.StdEncoding.EncodeToString(sum[:]), "field": "input"})
	expectStatus(t, rec, 200)
	if answer := decodeJSON[map[string]any](t, rec); answer["uploadUrl"] != nil {
		t.Errorf("uploadUrl = %v under the placeholder setting", answer["uploadUrl"])
	}
	expectStatus(t, h.send(t, "POST", "/api/public/media", map[string]any{
		"contentType": "image/png", "contentLength": 8,
		"sha256Hash": base64.StdEncoding.EncodeToString(sum[:])}), 400)
	// A trace id in any other spelling than the one traces are stored
	// under could never be settled or resolved.
	for _, bad := range []string{strings.ToUpper(probeTrace), "not-a-trace", probeTrace + "00"} {
		expectStatus(t, h.send(t, "POST", "/api/public/media", map[string]any{
			"traceId": bad, "contentType": "image/png", "contentLength": 8,
			"sha256Hash": base64.StdEncoding.EncodeToString(sum[:]), "field": "input"}), 400)
	}
}

// A picture the project stored before switching to placeholder is not given
// a new trace to live for: under the setting, ingest resolves no Langfuse
// string, and the string is kept as sent (#6, #9).
func TestLangfuseMediaPlaceholderResolvesNothing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(20000, 9)
	sum := sha256.Sum256(picture)
	mediaID, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil || h.langfusePut(t, *upload, picture, base64.StdEncoding.EncodeToString(sum[:])) != 200 {
		t.Fatal("the upload under store did not land")
	}
	expectStatus(t, h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"media": "placeholder"}), 200)

	other := strings.Repeat("cd", 16)
	reference := "@@@langfuseMedia:type=image/png|id=" + mediaID + "|source=base64_data_uri@@@"
	input, _ := json.Marshal([]any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": reference}}})
	export := otlptest.SpanWith("langfuse.observation.input", string(input))
	export[0].ScopeSpans[0].Spans[0].TraceId, _ = hex.DecodeString(other)
	expectStatus(t, h.post(t, "/api/public/otel/v1/traces", encodeExport(t, export)), 200)

	rec := h.get(t, "/api/v1/observations/"+probeSpan+"/io?trace_id="+other)
	expectStatus(t, rec, 200)
	// Read as sent, too: a ref for the new trace would have been resolved
	// on read (Decision 21), so the string also says none was written.
	if !strings.Contains(rec.Body.String(), reference) {
		t.Errorf("input = %.300s, want the SDK's string as sent", rec.Body.String())
	}
}
