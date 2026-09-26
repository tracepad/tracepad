package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// `tracepad export --otlp` against a real server and a real receiver (spec
// 019, Testing). Nothing is mocked below the command: the archive is a real
// one, the listing and the body endpoint are the real endpoints, and the
// receiver is an HTTP server that records what arrived.

// seedArchive writes n batches into the harness's project, a millisecond
// apart, and answers what each one holds so a test can compare.
func seedArchive(t *testing.T, h *harness, n int) [][]byte {
	t.Helper()
	bodies := make([][]byte, 0, n)
	for i := range n {
		body := []byte(strings.Repeat(fmt.Sprintf("batch-%03d;", i), 30))
		err := h.writer.Submit(t.Context(), &store.IngestBatch{
			ProjectID: h.projectID(t),
			Raw: &store.RawBatch{
				ReceivedAt:  seedBase + int64(i)*ms,
				Dialect:     "langfuse",
				ContentType: "application/x-protobuf",
				Body:        body,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
	}
	return bodies
}

// stub is an OTLP receiver that records what it was sent and answers what the
// test tells it to.
type stub struct {
	server *httptest.Server

	mu       sync.Mutex
	bodies   [][]byte
	types    []string
	encoding []string
	headers  []http.Header
	// answer decides each request's fate by its ordinal, so a test can say
	// "503, 503, then 200" without a clock.
	answer func(n int) (status int, body []byte, contentType string)
	// onRequest runs before the answer, for a test that needs the world to
	// change while the export is walking it.
	onRequest func()
}

func newStub(t *testing.T, answer func(n int) (int, []byte, string)) *stub {
	s := &stub{answer: answer}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.types = append(s.types, r.Header.Get("Content-Type"))
		s.encoding = append(s.encoding, r.Header.Get("Content-Encoding"))
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()

		if s.onRequest != nil {
			s.onRequest()
		}
		status, answer, contentType := http.StatusOK, []byte(nil), ""
		if s.answer != nil {
			status, answer, contentType = s.answer(n)
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		w.Write(answer)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stub) received() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.bodies...)
}

// The archive is replayed in arrival order, under the content types it was
// stored with, and the summary says what went and what could not (spec 019 #5).
func TestExportReplaysInOrder(t *testing.T) {
	h := newHarness(t)
	bodies := seedArchive(t, h, 5)
	sink := newStub(t, nil)

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}

	received := sink.received()
	if len(received) != len(bodies) {
		t.Fatalf("the receiver got %d batches, want %d", len(received), len(bodies))
	}
	for i, body := range bodies {
		if !bytes.Equal(received[i], body) {
			t.Fatalf("batch %d arrived out of order or altered", i)
		}
		if sink.types[i] != "application/x-protobuf" {
			t.Errorf("batch %d posted under %q", i, sink.types[i])
		}
	}

	summary := decodeSummary(t, got.stdout)
	if summary.Sent != 5 || summary.Bytes == 0 {
		t.Errorf("summary = %+v, want the five batches", summary)
	}
	if summary.LastCursor == "" {
		t.Error("the summary carries no cursor; a next run has no place to start")
	}
	if summary.StoppedAt != nil {
		t.Errorf("stopped_at = %+v, want nothing", summary.StoppedAt)
	}
}

// A receiver asking for time gets it: 503 twice then 200 is one batch
// delivered once, on the third attempt (spec 019 #6).
func TestExportRetriesWhatTheReceiverAsksToRetry(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t)
			h.retryBackoff = time.Millisecond
			seedArchive(t, h, 1)
			sink := newStub(t, func(n int) (int, []byte, string) {
				if n < 2 {
					return status, []byte("come back later"), ""
				}
				return http.StatusOK, nil, ""
			})

			got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
			}
			if len(sink.received()) != 3 {
				t.Errorf("attempts = %d, want two refusals and one delivery", len(sink.received()))
			}
			if summary := decodeSummary(t, got.stdout); summary.Sent != 1 {
				t.Errorf("sent = %d, want the one batch counted once", summary.Sent)
			}
		})
	}
}

// A receiver describing the batch stops the export, and the cursor it leaves
// resumes at that batch rather than after it: nothing is skipped, and the order
// has no holes (spec 019 #6).
func TestExportStopsOnA400AndResumesAtThatBatch(t *testing.T) {
	h := newHarness(t)
	h.retryBackoff = time.Millisecond
	bodies := seedArchive(t, h, 4)

	// The third batch is refused; the first two go.
	refusing := newStub(t, func(n int) (int, []byte, string) {
		if n == 2 {
			return http.StatusBadRequest, []byte("span id is not 8 bytes"), ""
		}
		return http.StatusOK, nil, ""
	})
	got := h.run(t.Context(), false, "export", "--otlp", "--to", refusing.server.URL)
	if got.code != ExitFailure {
		t.Fatalf("exit = %d, want the stop (stderr: %s)", got.code, got.stderr)
	}
	if len(refusing.received()) != 3 {
		t.Errorf("the receiver got %d batches, want it to stop at the refused one",
			len(refusing.received()))
	}
	summary := decodeSummary(t, got.stdout)
	if summary.Sent != 2 {
		t.Errorf("sent = %d, want the two that were taken", summary.Sent)
	}
	if summary.StoppedAt == nil || summary.StoppedAt.Status != http.StatusBadRequest {
		t.Fatalf("stopped_at = %+v, want the receiver's refusal", summary.StoppedAt)
	}
	if !strings.Contains(summary.StoppedAt.Message, "span id") {
		t.Errorf("stopped_at.message = %q, want what the receiver said", summary.StoppedAt.Message)
	}
	if summary.LastCursor == "" {
		t.Fatal("a stop with no cursor cannot be resumed")
	}

	// The resume: the same command with the cursor, against a receiver that
	// takes everything. The first thing it sees is the batch that failed.
	taking := newStub(t, nil)
	resumed := h.run(t.Context(), false, "export", "--otlp",
		"--to", taking.server.URL, "--after", summary.LastCursor)
	if resumed.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", resumed.code, resumed.stderr)
	}
	received := taking.received()
	if len(received) != 2 {
		t.Fatalf("the resume sent %d batches, want the failed one and the one after", len(received))
	}
	if !bytes.Equal(received[0], bodies[2]) {
		t.Error("the resume did not start at the batch that failed")
	}
	if !bytes.Equal(received[1], bodies[3]) {
		t.Error("the resume did not continue past the batch that failed")
	}
}

// The retries running out stops the export exactly as a refusal does: a
// receiver that has been asking for time for the whole backoff is an outage,
// and skipping past it would leave a hole nobody would find.
func TestExportStopsWhenRetriesRunOut(t *testing.T) {
	h := newHarness(t)
	h.retryBackoff = time.Millisecond
	seedArchive(t, h, 2)
	sink := newStub(t, func(int) (int, []byte, string) {
		return http.StatusServiceUnavailable, []byte("still down"), ""
	})

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL)
	if got.code != ExitFailure {
		t.Fatalf("exit = %d, want the stop", got.code)
	}
	if len(sink.received()) != maxAttempts {
		t.Errorf("attempts = %d, want %d", len(sink.received()), maxAttempts)
	}
	if summary := decodeSummary(t, got.stdout); summary.Sent != 0 {
		t.Errorf("sent = %d, want nothing delivered", summary.Sent)
	}
}

// A batch the sweeper takes between the listing page and the body fetch is the
// one thing this command skips, and it says so rather than stopping: the
// archive moved, not the receiver (spec 019, edge cases).
func TestExportSkipsABatchSweptUnderIt(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 3)

	// The receiver sweeps the archive out from under the export as soon as
	// the first batch lands. The page was already fetched, so the walk goes
	// on to two ids that no longer resolve.
	var once sync.Once
	sink := newStub(t, nil)
	sink.onRequest = func() {
		once.Do(func() { sweepRawAway(t, h) })
	}

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, want the walk to finish (stderr: %s)", got.code, got.stderr)
	}
	summary := decodeSummary(t, got.stdout)
	if summary.Sent != 1 || summary.Swept != 2 {
		t.Errorf("summary = %+v, want one sent and two counted as swept", summary)
	}
	if !strings.Contains(got.stderr, "swept") {
		t.Errorf("stderr = %q, want the skipped ids named", got.stderr)
	}
}

// sweepRawAway shortens the raw window to nothing and runs one pass, which is
// what retention does to an archive while an export is walking it.
func sweepRawAway(t *testing.T, h *harness) {
	t.Helper()
	project, err := h.store.ProjectByName("test")
	if err != nil || project == nil {
		t.Errorf("project = %v, err = %v", project, err)
		return
	}
	one := 1
	update := &store.ProjectUpdate{
		ProjectID: project.ID,
		RawWindow: store.OptionalDays{Set: true, Value: &one},
		Confirm:   project.Name,
	}
	if err := h.writer.Submit(t.Context(), update); err != nil {
		t.Errorf("shorten the raw window: %v", err)
		return
	}
	if err := h.store.NewSweeper(h.writer, store.SweepOptions{}).Pass(t.Context()); err != nil {
		t.Errorf("sweep: %v", err)
	}
}

// `--gzip` compresses on the wire and says so; what arrives decompresses to
// the bytes the archive holds.
func TestExportGzipsWhenAsked(t *testing.T) {
	h := newHarness(t)
	bodies := seedArchive(t, h, 1)
	sink := newStub(t, nil)

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL,
		"--gzip", "--header", "authorization=Bearer receiver-key",
		"--header", "x-scope-orgid=acme")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if sink.encoding[0] != "gzip" {
		t.Fatalf("Content-Encoding = %q", sink.encoding[0])
	}
	if got := ungzip(t, sink.received()[0]); !bytes.Equal(got, bodies[0]) {
		t.Error("what arrived does not decompress to the archived body")
	}
	if sink.headers[0].Get("Authorization") != "Bearer receiver-key" {
		t.Errorf("the receiver's credentials did not ride along: %v", sink.headers[0])
	}
	if sink.headers[0].Get("X-Scope-Orgid") != "acme" {
		t.Errorf("a repeated --header was dropped: %v", sink.headers[0])
	}
}

// `OTEL_EXPORTER_OTLP_HEADERS` is not read (spec 019 #14). On a machine that
// sends traces here it holds this server's project key, and copying it into
// every POST handed a project admin's key to whatever receiver `--to` named.
func TestExportDoesNotReadTheOTLPHeadersVariable(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 1)
	sink := newStub(t, nil)
	h.env["OTEL_EXPORTER_OTLP_HEADERS"] = "authorization=Bearer " + testKey + ",x-tenant=acme"

	// A --header of another name: the note is said all the same, since the
	// receiver's credentials may have been in the variable.
	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL, "--header", "x-scope-orgid=acme")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if len(sink.headers) != 1 {
		t.Fatalf("the receiver saw %d requests, want 1", len(sink.headers))
	}
	for name, values := range sink.headers[0] {
		for _, value := range values {
			if strings.Contains(value, "tp-sk-") || name == "X-Tenant" {
				t.Errorf("the variable reached the receiver: %s: %s", name, value)
			}
		}
	}
	// Said, without the value, so a receiver answering 401 is not a mystery.
	if !strings.Contains(got.stderr, "OTEL_EXPORTER_OTLP_HEADERS is not read") ||
		strings.Contains(got.stderr, testKey) {
		t.Errorf("stderr = %q", got.stderr)
	}
}

// A `--header` value goes out as written: a base64 bearer token keeps its `+`.
func TestExportKeepsAHeaderValueAsWritten(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 1)
	sink := newStub(t, nil)
	const token = "Bearer YWJj+ZGVm/Z2hp=="

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL, "--header", "authorization="+token)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if len(sink.headers) != 1 {
		t.Fatalf("the receiver saw %d requests, want 1", len(sink.headers))
	}
	if header := sink.headers[0].Get("Authorization"); header != token {
		t.Errorf("Authorization = %q, want %q", header, token)
	}
}

// exportHeaders, as a table (spec 019 #14): one value per header whatever the
// case, the last flag winning; a name or value HTTP cannot carry refused at
// once; a Tracepad key refused wherever the receiver would get it — in a
// header, as Bearer or as Basic, or in `--to` itself — the source's own key
// even with --allow-tracepad-key.
func TestExportHeadersTable(t *testing.T) {
	const own = "admin-token-123"
	basic := func(pair string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair)) }
	for _, tc := range []struct {
		name  string
		to    string
		flags []string
		allow bool
		want  map[string]string
		// refused is a fragment of the error, empty when none is wanted.
		refused string
	}{
		{name: "one value whatever the case, the last flag's",
			flags: []string{"authorization=Bearer first", "AUTHORIZATION=Bearer second", "Authorization=Bearer last"},
			want:  map[string]string{"Authorization": "Bearer last"}},
		{name: "a value goes out as written",
			flags: []string{"x-scope-orgid=acme", "authorization=Bearer YWJj+ZGVm/Z2hp=="},
			want:  map[string]string{"X-Scope-Orgid": "acme", "Authorization": "Bearer YWJj+ZGVm/Z2hp=="}},
		{name: "a receiver's own Basic pair", flags: []string{"authorization=" + basic("pk-lf-1:sk-lf-2")},
			want: map[string]string{"Authorization": basic("pk-lf-1:sk-lf-2")}},
		{name: "a line break in a value", flags: []string{"authorization=Bearer x\r\nX-Evil: 1"},
			refused: "line break"},
		{name: "a space in a name", flags: []string{"x api=1"}, refused: "not an HTTP header name"},
		{name: "a tp-sk- bearer", flags: []string{"authorization=Bearer tp-sk-0123"}, refused: "Tracepad project key"},
		{name: "a tp-sk- in any case", flags: []string{"x-key=TP-SK-shouting"}, refused: "Tracepad project key"},
		{name: "a tp-sk- inside Basic", flags: []string{"authorization=" + basic("tp-pk-1:tp-sk-2")},
			refused: "Tracepad project key"},
		{name: "a tp-sk- the flag allows", flags: []string{"authorization=Bearer tp-sk-0123"}, allow: true,
			want: map[string]string{"Authorization": "Bearer tp-sk-0123"}},
		{name: "the command's own key as a bearer", flags: []string{"authorization=Bearer " + own},
			refused: "the key this command reads"},
		{name: "the command's own key inside Basic, flag or not", flags: []string{"authorization=" + basic("x:"+own)},
			allow: true, refused: "the key this command reads"},
		{name: "the own key is a token, not a substring", flags: []string{"x-env=" + own + "-and-more"},
			want: map[string]string{"X-Env": own + "-and-more"}},
		{name: "a tp-sk- in the url's user info", to: "https://pk:tp-sk-0123@collector.example/v1/traces",
			refused: "--to carries a Tracepad"},
		{name: "a tp-sk- in the url's query", to: "https://collector.example/v1/traces?key=tp-sk-0123",
			refused: "--to carries a Tracepad"},
		{name: "a collector's own Basic in user info", to: "https://user:secret@collector.example/v1/traces",
			want: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			r := newRun(Options{Stderr: &errOut, Env: func(string) string { return "" }})
			r.key = own
			to := tc.to
			if to == "" {
				to = "https://collector.example/v1/traces"
			}
			got, err := r.exportHeaders(to, tc.flags, tc.allow)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("err = %v, want one saying %q", err, tc.refused)
				}
				if strings.Contains(err.Error(), own) || strings.Contains(err.Error(), "tp-sk-0123") {
					t.Errorf("the refusal repeats the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("headers = %v, want %v", got, tc.want)
			}
			for name, value := range tc.want {
				if got[name] != value {
					t.Errorf("%s = %q, want %q", name, got[name], value)
				}
			}
		})
	}
}

// And end to end, once: each request carries one Authorization, the last
// flag's.
func TestExportHeadersAreOneValueWhateverTheCase(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 1)
	sink := newStub(t, nil)

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL,
		"--header", "authorization=Bearer first", "--header", "AUTHORIZATION=Bearer second",
		"--header", "Authorization=Bearer last")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if len(sink.headers) != 1 {
		t.Fatalf("the receiver saw %d requests, want 1", len(sink.headers))
	}
	if values := sink.headers[0].Values("Authorization"); len(values) != 1 || values[0] != "Bearer last" {
		t.Fatalf("Authorization = %q, want the last flag's value alone", values)
	}
}

// A Tracepad key is refused before the first POST, wherever it sits: the
// receiver would get admin access to the project (spec 019 #14).
func TestExportRefusesToSendATracepadKey(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 1)
	sink := newStub(t, nil)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("tp-pk-test:"+testKey))
	withUser := strings.Replace(sink.server.URL, "://", "://tp-pk-test:"+testKey+"@", 1)

	for _, args := range [][]string{
		{"--to", sink.server.URL, "--header", "authorization=Bearer tp-sk-0123456789abcdef"},
		{"--to", sink.server.URL, "--header", "x-api-key=" + testKey},
		{"--to", sink.server.URL, "--header", "authorization=" + basic},
		// The source's own key, which the flag does not let through.
		{"--to", sink.server.URL, "--header", "authorization=" + basic, "--allow-tracepad-key"},
		{"--to", withUser},
	} {
		got := h.run(t.Context(), false, append([]string{"export", "--otlp"}, args...)...)
		if got.code != ExitFailure || !strings.Contains(got.stderr, "admin access to your project") {
			t.Errorf("%q: exit = %d, stderr = %q, want a refusal", args, got.code, got.stderr)
		}
		if strings.Contains(got.stderr, "tp-sk-0123") || strings.Contains(got.stderr, testKey) {
			t.Errorf("%q: the refusal repeats the key: %q", args, got.stderr)
		}
	}
	if len(sink.received()) != 0 {
		t.Fatalf("the receiver got %d batches", len(sink.received()))
	}
}

// A 2xx that reports rejected spans is the receiver describing its own
// mapping: it has the bytes, so the batch is counted and the export goes on.
func TestExportCountsPartialSuccessWithoutRetrying(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 2)
	sink := newStub(t, func(int) (int, []byte, string) {
		return http.StatusOK, mapping.EncodeExportResponse(3, "two spans had no trace id"), ""
	})

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if len(sink.received()) != 2 {
		t.Errorf("attempts = %d, want one per batch and no retry", len(sink.received()))
	}
	summary := decodeSummary(t, got.stdout)
	if summary.Sent != 2 || summary.PartialSuccess != 2 {
		t.Errorf("summary = %+v, want both batches sent and both reported", summary)
	}
	if !strings.Contains(got.stderr, "no trace id") {
		t.Errorf("stderr = %q, want the receiver's own message", got.stderr)
	}
}

// `--dir` writes one file per batch, named so that `ls` is in replay order,
// beside a manifest that is the listing one row per line (spec 019 #5).
func TestExportToADirectory(t *testing.T) {
	h := newHarness(t)
	bodies := seedArchive(t, h, 3)
	dir := filepath.Join(t.TempDir(), "archive")

	got := h.run(t.Context(), false, "export", "--otlp", "--dir", dir)
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}

	names := batchFiles(t, dir)
	if len(names) != 3 {
		t.Fatalf("files = %v, want one per batch", names)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("files = %v, which is not replay order", names)
	}
	for i, name := range names {
		if !strings.HasSuffix(name, ".pb") {
			t.Errorf("%s: a protobuf batch should be a .pb", name)
		}
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, bodies[i]) {
			t.Errorf("%s does not hold the batch it is named for", name)
		}
	}

	rows := manifestRows(t, dir)
	if len(rows) != 3 {
		t.Fatalf("manifest rows = %d, want one per batch", len(rows))
	}
	for _, row := range rows {
		if row.ContentType == "" || row.ReceivedAt == "" || row.SizeBytes == 0 {
			t.Errorf("manifest row = %+v, want the listing's fields", row)
		}
	}
}

// A resume appends to the manifest it left and does not write a batch twice.
func TestExportToADirectoryResumes(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 4)
	dir := filepath.Join(t.TempDir(), "archive")

	first := h.run(t.Context(), false, "export", "--otlp", "--dir", dir, "--until",
		instantAfter(2))
	if first.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", first.code, first.stderr)
	}
	if rows := manifestRows(t, dir); len(rows) != 2 {
		t.Fatalf("the first half wrote %d rows, want 2", len(rows))
	}

	// A second export into the same directory without a cursor is refused,
	// so that two exports never interleave one manifest.
	refused := h.run(t.Context(), false, "export", "--otlp", "--dir", dir)
	if refused.code != ExitUsage {
		t.Fatalf("exit = %d, want the usage refusal (stderr: %s)", refused.code, refused.stderr)
	}

	cursor := decodeSummary(t, first.stdout).LastCursor
	second := h.run(t.Context(), false, "export", "--otlp", "--dir", dir, "--after", cursor)
	if second.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", second.code, second.stderr)
	}
	rows := manifestRows(t, dir)
	if len(rows) != 4 {
		t.Fatalf("manifest rows = %d, want all four exactly once", len(rows))
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			t.Errorf("batch %d appears in the manifest twice", row.ID)
		}
		seen[row.ID] = true
	}
	if len(batchFiles(t, dir)) != 4 {
		t.Errorf("files = %v, want one per batch", batchFiles(t, dir))
	}
}

// A dry run sends nothing and prints what there is to send.
func TestExportDryRun(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 3)
	sink := newStub(t, nil)

	got := h.run(t.Context(), false, "export", "--otlp", "--to", sink.server.URL, "--dry-run")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if len(sink.received()) != 0 {
		t.Errorf("a dry run sent %d batches", len(sink.received()))
	}
	summary := decodeSummary(t, got.stdout)
	if summary.MatchingWindow == nil || *summary.MatchingWindow != 3 {
		t.Errorf("matching_window = %v, want the three batches", summary.MatchingWindow)
	}
	if summary.Sent != 0 {
		t.Errorf("sent = %d, want nothing", summary.Sent)
	}
	if summary.Resuming {
		t.Error("resuming = true without --after")
	}
}

// The API's count is over the filters and never over the page (spec 009 #4),
// so `--after` does not narrow it. A dry run therefore reports the *window*,
// and says so — a number called "matching" beside a cursor would be read as
// what is left to send, and acted on.
func TestExportDryRunNamesTheWindowWhenResuming(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 4)

	// A cursor two batches in: the run would send two, the window holds four.
	first := h.run(t.Context(), false, "export", "--otlp", "--dir", t.TempDir(),
		"--until", instantAfter(2))
	if first.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", first.code, first.stderr)
	}
	cursor := decodeSummary(t, first.stdout).LastCursor

	got := h.run(t.Context(), false, "export", "--otlp", "--dir", t.TempDir(),
		"--after", cursor, "--dry-run")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	summary := decodeSummary(t, got.stdout)
	if summary.MatchingWindow == nil || *summary.MatchingWindow != 4 {
		t.Errorf("matching_window = %v, want the whole window's 4", summary.MatchingWindow)
	}
	if !summary.Resuming {
		t.Error("resuming = false with --after; nothing marks the number as more than will be sent")
	}

	// And a person reading the terminal is told the same thing.
	human := h.run(t.Context(), true, "export", "--otlp", "--dir", t.TempDir(),
		"--after", cursor, "--dry-run")
	if !strings.Contains(human.stdout, "in the window") ||
		!strings.Contains(human.stdout, "resume starts inside it") {
		t.Errorf("stdout = %q, want the number named as the window's", human.stdout)
	}
}

// With raw storage off and nothing archived, the export refuses with a reason
// and counts every trace as beyond its reach; a dry run says the same and
// exits 0, because asking was not a mistake (spec 019, edge cases).
func TestExportRefusesWhenRawStorageIsOff(t *testing.T) {
	h := newHarnessWithoutRaw(t)
	h.seed(t, &model.Trace{ID: fmt.Sprintf("%032x", 1)},
		&model.Observation{TraceID: fmt.Sprintf("%032x", 1), ID: fmt.Sprintf("%016x", 1),
			Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + ms})

	got := h.run(t.Context(), false, "export", "--otlp", "--to", "http://127.0.0.1:1")
	if got.code != ExitFailure {
		t.Fatalf("exit = %d, want the refusal", got.code)
	}
	if !strings.Contains(got.stderr, "raw storage is off") {
		t.Errorf("stderr = %q, want the reason", got.stderr)
	}
	if summary := decodeSummary(t, got.stdout); summary.TracesBeforeWindow != 1 {
		t.Errorf("traces_before_window = %d, want every trace", summary.TracesBeforeWindow)
	}

	dry := h.run(t.Context(), false, "export", "--otlp", "--to", "http://127.0.0.1:1", "--dry-run")
	if dry.code != ExitOK {
		t.Fatalf("dry run exit = %d, want 0", dry.code)
	}
}

// The usage refusals: one destination, and the flags that only make sense with
// a receiver (spec 019, CLI contract).
func TestExportUsageRefusals(t *testing.T) {
	h := newHarness(t)
	for name, args := range map[string][]string{
		"no --otlp":       {"export", "--to", "http://localhost:4318/v1/traces"},
		"no destination":  {"export", "--otlp"},
		"two of them":     {"export", "--otlp", "--to", "http://x/y", "--dir", "/tmp/x"},
		"header on a dir": {"export", "--otlp", "--dir", "/tmp/x", "--header", "a=b"},
		"gzip on a dir":   {"export", "--otlp", "--dir", "/tmp/x", "--gzip"},
		"a bad --to":      {"export", "--otlp", "--to", "not-a-url"},
		// A scheme that parses but is not one a receiver speaks. Left to
		// the transport it becomes an error the retry loop reads as "the
		// receiver is busy" and backs off half a minute over.
		"a non-http --to": {"export", "--otlp", "--to", "ftp://host/v1/traces"},
		"a file:// --to":  {"export", "--otlp", "--to", "file:///tmp/traces"},
		"an empty cursor": {"export", "--otlp", "--to", "http://x/y", "--after", ""},
		"a bad --since":   {"export", "--otlp", "--to", "http://x/y", "--since", "yesterday"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := h.run(t.Context(), false, args...); got.code != ExitUsage {
				t.Errorf("exit = %d, want %d (stderr: %s)", got.code, ExitUsage, got.stderr)
			}
		})
	}
}

// A `--dir` export that never wrote a batch leaves nothing behind. Creating
// the manifest up front made the directory non-empty before anything had been
// written to it, so a run that failed on its first batch — or matched none —
// left exactly enough to make the re-run refuse, while printing no cursor to
// resume from: a dead end with instructions that could not be followed.
func TestExportToADirectoryLeavesNothingWhenItSendsNothing(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 2)
	dir := filepath.Join(t.TempDir(), "archive")

	// A window that matches nothing: the walk finds no batch to write.
	empty := h.run(t.Context(), false, "export", "--otlp", "--dir", dir,
		"--until", instantAfter(0))
	if empty.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", empty.code, empty.stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the directory holds %v after an export that wrote nothing", entries)
	}

	// So the ordinary re-run is not refused.
	again := h.run(t.Context(), false, "export", "--otlp", "--dir", dir)
	if again.code != ExitOK {
		t.Fatalf("the re-run exit = %d, want it accepted (stderr: %s)", again.code, again.stderr)
	}
	if rows := manifestRows(t, dir); len(rows) != 2 {
		t.Errorf("manifest rows = %d, want both batches", len(rows))
	}
}

// A cursor that contradicts the window is a mistake in the command line, not a
// failure of the transfer, and exits 2 (spec 019, edge cases).
func TestExportCursorBeforeSinceIsAUsageError(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 3)
	cursor := encodeRawCursor(seedBase-day, 1)

	got := h.run(t.Context(), false, "export", "--otlp", "--dir", t.TempDir(),
		"--after", cursor, "--since", instantAfter(1))
	if got.code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "cursor") {
		t.Errorf("stderr = %q, want it to name the contradiction", got.stderr)
	}
}

// `tracepad system` prints the archive block, because the numbers an operator
// reads before shortening the raw window are the same ones the export reports
// at its end (spec 019 #4).
func TestSystemPrintsTheRawArchive(t *testing.T) {
	h := newHarness(t)
	seedArchive(t, h, 2)

	got := h.run(t.Context(), true, "system")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	for _, want := range []string{"raw archive", "batches", "on disk", "covering", "not covered"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("the system output does not mention %q:\n%s", want, got.stdout)
		}
	}
}

// TestExportRoundTrip is the whole promise in one test: a corpus ingested into
// one server, exported into a second one's `/v1/traces`, and the two read APIs
// compared field by field. What arrives at B is what A holds — ids included.
func TestExportRoundTrip(t *testing.T) {
	source := newHarness(t)
	destination := newHarness(t)

	for _, fixture := range otlptest.Fixtures() {
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			t.Fatal(err)
		}
		postOTLP(t, source.url, body)
	}

	// The receiver's own credentials, because that is what it is: a second
	// Tracepad, reached the way any OTLP receiver is — with a key of its own,
	// not the source's, which no flag lets out (spec 019 #14).
	minted := destination.run(t.Context(), false, "keys", "create", "--json")
	if minted.code != ExitOK {
		t.Fatalf("keys create: exit = %d, stderr = %s", minted.code, minted.stderr)
	}
	var key struct {
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal([]byte(minted.stdout), &key); err != nil || key.SecretKey == testKey {
		t.Fatalf("keys create = %q, %v", minted.stdout, err)
	}
	got := source.run(t.Context(), false, "export", "--otlp",
		"--to", destination.url+"/v1/traces",
		"--header", "authorization=Bearer "+key.SecretKey,
		// A Tracepad key, the destination's own — which is what this flag
		// is for.
		"--allow-tracepad-key")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	summary := decodeSummary(t, got.stdout)
	if summary.Sent != int64(len(otlptest.Fixtures())) {
		t.Fatalf("sent = %d, want one batch per fixture", summary.Sent)
	}

	before := readEverything(t, source)
	after := readEverything(t, destination)
	if before != after {
		t.Errorf("the replayed project is not the one that was exported.\n--- source ---\n%s\n--- destination ---\n%s",
			before, after)
	}
}

// readEverything renders a project through the read API: every trace, whole,
// with its payloads. The API rather than the store, because what a user gets
// back is what the promise is about.
func readEverything(t *testing.T, h *harness) string {
	t.Helper()
	listing := httpGet(t, h.url+"/api/v1/traces?limit=500&fields=id")
	var page struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}
	if err := json.Unmarshal([]byte(listing), &page); err != nil {
		t.Fatalf("the listing is not JSON: %v (%s)", err, listing)
	}
	ids := make([]string, 0, len(page.Traces))
	for _, row := range page.Traces {
		ids = append(ids, row.ID)
	}
	sort.Strings(ids)

	var out strings.Builder
	for _, id := range ids {
		out.WriteString(httpGet(t, h.url+"/api/v1/traces/"+id+"?expand=io&budget=1048576"))
		out.WriteString("\n")
	}
	return out.String()
}

// postOTLP sends one export body the way an exporter would.
func postOTLP(t *testing.T, base string, body []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set("Authorization", "Bearer "+testKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ingest answered %d", response.StatusCode)
	}
}

func decodeSummary(t *testing.T, stdout string) exportSummary {
	t.Helper()
	var summary exportSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("the summary is not JSON: %v (stdout: %s)", err, stdout)
	}
	return summary
}

// batchFiles is what an export wrote, manifest excluded, in name order.
func batchFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() != "manifest.jsonl" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func manifestRows(t *testing.T, dir string) []rawBatchRow {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []rawBatchRow
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if line == "" {
			continue
		}
		var row rawBatchRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("manifest line is not JSON: %v (%s)", err, line)
		}
		rows = append(rows, row)
	}
	return rows
}

// instantAfter is the RFC 3339 form of the nth seeded batch's arrival, for a
// window boundary a test can reason about.
func instantAfter(n int) string {
	return time.Unix(0, seedBase+int64(n)*ms).UTC().Format(time.RFC3339Nano)
}

const day = int64(24 * 60 * 60 * 1_000_000_000)

// ungzip is what a receiver does with a `--gzip` body.
func ungzip(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
