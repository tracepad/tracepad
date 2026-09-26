package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/mapping"
)

// Where an export goes (spec 019 #5): an OTLP receiver, which is the promise's
// literal form, or a directory, which is the promise kept when there is no
// collector yet — the operator takes the files and the manifest and is not
// locked in by the absence of one.

// destination is one place a replayed batch lands. `send` answers what the
// receiver said about spans it would not keep, which is the receiver's
// business to explain and this command's to pass on.
type destination interface {
	send(ctx context.Context, row rawBatchRow, body []byte) (partial string, err error)
	close() error
}

// stopError ends the export. It carries what a resume needs to be typed from:
// which batch, what the receiver said, and — through the summary — the cursor
// that starts again at this batch rather than after it (spec 019 #6).
type stopError struct {
	id      int64
	status  int
	message string
}

func (e *stopError) Error() string {
	if e.status == 0 {
		return fmt.Sprintf("batch %d could not be delivered: %s", e.id, e.message)
	}
	return fmt.Sprintf("batch %d was refused: %d %s", e.id, e.status, e.message)
}

// destination builds the one the flags name.
func (r *run) destination(to, dir string, headers headerList, compress, resuming bool) (destination, error) {
	if to != "" {
		return r.receiver(to, headers, compress)
	}
	return r.directory(dir, resuming)
}

// receiver posts each body to an OTLP/HTTP endpoint.
type receiver struct {
	run      *run
	url      string
	headers  map[string]string
	gzip     bool
	http     *http.Client
	backoff  time.Duration
	attempts int
}

func (r *run) receiver(target string, headers headerList, compress bool) (destination, error) {
	// The scheme is checked and not merely required to be present: a
	// receiver is an OTLP/HTTP endpoint, and `ftp://host/x` parses into a
	// scheme and a host perfectly well. Left to the transport it would
	// become an error the retry loop reads as "the receiver is busy" and
	// spends half a minute backing off before failing obscurely.
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, usageErrorf("--to %q is not an http(s) url", target)
	}
	resolved, err := exportHeaders(r.opt.Env("OTEL_EXPORTER_OTLP_HEADERS"), headers)
	if err != nil {
		return nil, err
	}
	backoff := r.opt.retryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	return &receiver{
		run:     r,
		url:     target,
		headers: resolved,
		gzip:    compress,
		// No client-side timeout: a receiver ingesting a large batch is
		// doing work, and the retry loop below is what bounds a hang
		// that is a failure rather than a wait. The context still ends
		// it when the user does.
		http:     &http.Client{},
		backoff:  backoff,
		attempts: maxAttempts,
	}, nil
}

func (s *receiver) close() error { return nil }

// send posts one batch, retrying what a receiver asks to have retried.
//
// `429`, `5xx` and a transport error are a receiver asking for time; any other
// `4xx` is a receiver describing the batch, and sending the next one would
// leave a hole the operator would not find until they looked for the trace
// (spec 019 #6).
func (s *receiver) send(ctx context.Context, row rawBatchRow, body []byte) (string, error) {
	wait := s.backoff
	var last error
	for attempt := 1; attempt <= s.attempts; attempt++ {
		partial, retryable, err := s.attempt(ctx, row, body)
		if err == nil {
			return partial, nil
		}
		last = err
		if !retryable {
			return "", err
		}
		if attempt == s.attempts {
			break
		}
		// The batch and the attempt first, the reason after them as a block
		// of its own: a reason that runs to several lines cannot carry the
		// retry away from the batch it is about (#35).
		fmt.Fprintf(s.run.opt.Stderr,
			"tracepad: batch %d: retrying in %s (attempt %d of %d): %s\n",
			row.ID, wait, attempt+1, s.attempts, block(err.Error(), "          "))
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
		if wait *= 2; wait > maxRetryBackoff {
			wait = maxRetryBackoff
		}
	}
	// The retries ran out, which stops the export exactly as an unretryable
	// refusal does: a receiver that has been asking for time for half a
	// minute is an outage, and skipping past it would leave a hole.
	var stop *stopError
	if !asStop(last, &stop) {
		stop = &stopError{id: row.ID, message: last.Error()}
	}
	return "", stop
}

// attempt is one delivery. It reports whether the failure is one the receiver
// asked to have repeated.
func (s *receiver) attempt(ctx context.Context, row rawBatchRow, body []byte) (
	partial string, retryable bool, err error) {
	payload := body
	if s.gzip {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(body); err != nil {
			return "", false, &stopError{id: row.ID, message: err.Error()}
		}
		if err := writer.Close(); err != nil {
			return "", false, &stopError{id: row.ID, message: err.Error()}
		}
		payload = compressed.Bytes()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(payload))
	if err != nil {
		return "", false, &stopError{id: row.ID, message: err.Error()}
	}
	// The type it was received in, because the receiver accepted it once
	// under that type and a conversion here would make the archive lie
	// (spec 019 #8).
	request.Header.Set("Content-Type", row.ContentType)
	if s.gzip {
		request.Header.Set("Content-Encoding", "gzip")
	}
	for key, value := range s.headers {
		request.Header.Set(key, value)
	}

	response, err := s.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", true, err
	}
	defer response.Body.Close()
	answer, readErr := io.ReadAll(io.LimitReader(response.Body, maxReceiverResponse))
	if readErr != nil {
		return "", true, readErr
	}

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return partialSuccess(response.Header.Get("Content-Type"), answer), false, nil
	case response.StatusCode == http.StatusTooManyRequests, response.StatusCode >= 500:
		return "", true, fmt.Errorf("the receiver answered %d %s",
			response.StatusCode, firstLine(answer))
	default:
		return "", false, &stopError{id: row.ID, status: response.StatusCode,
			message: firstLine(answer)}
	}
}

// maxReceiverResponse bounds what is read back from a receiver: the response
// is an ExportTraceServiceResponse or an error message, and neither is large.
// A receiver that streams a gigabyte at us is not one to be believed.
const maxReceiverResponse = 1 << 20

// directory writes one file per batch beside a manifest, so that `ls` is in
// replay order and a script can replay the files with `curl` (spec 019 #5).
type directory struct {
	path     string
	manifest *os.File
}

func (r *run) directory(path string, resuming bool) (destination, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %w", path, err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	// A resume appends to the manifest it left; a fresh export into a
	// directory that already holds one would interleave two exports in one
	// manifest, and nothing downstream could tell them apart.
	if len(entries) > 0 && !resuming {
		return nil, usageErrorf("%s is not empty; export into an empty directory, "+
			"or pass --after to resume the export that filled it", path)
	}
	// The manifest is opened on the first batch, not here. Creating it up
	// front makes the directory non-empty before anything has been written
	// to it, so an export that fails on its first batch — or matches none —
	// leaves behind exactly enough to make the emptiness check above refuse
	// the re-run, while printing no cursor to pass as `--after`. The user
	// would be told to resume an export that never started.
	return &directory{path: path}, nil
}

func (d *directory) close() error {
	if d.manifest == nil {
		return nil
	}
	return d.manifest.Close()
}

// open resolves the manifest on first use.
func (d *directory) open() error {
	if d.manifest != nil {
		return nil
	}
	manifest, err := os.OpenFile(filepath.Join(d.path, "manifest.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open the manifest: %w", err)
	}
	d.manifest = manifest
	return nil
}

// send writes the body and then the manifest line, in that order: a line in
// the manifest is a promise that the file beside it is whole.
func (d *directory) send(_ context.Context, row rawBatchRow, body []byte) (string, error) {
	if err := d.open(); err != nil {
		return "", &stopError{id: row.ID, message: err.Error()}
	}
	name, err := batchFileName(row)
	if err != nil {
		return "", &stopError{id: row.ID, message: err.Error()}
	}
	if err := os.WriteFile(filepath.Join(d.path, name), body, 0o644); err != nil {
		return "", &stopError{id: row.ID, message: err.Error()}
	}
	line, err := json.Marshal(row)
	if err != nil {
		return "", &stopError{id: row.ID, message: err.Error()}
	}
	if _, err := d.manifest.Write(append(line, '\n')); err != nil {
		return "", &stopError{id: row.ID, message: err.Error()}
	}
	return "", nil
}

// batchFileName is `<received_at_ms>-<id>.<ext>`: milliseconds first so that a
// plain `ls` is in replay order, the id after it so that two batches of one
// millisecond are two files, and the extension from the content type so that
// what a file holds is visible without opening it.
func batchFileName(row rawBatchRow) (string, error) {
	at, err := time.Parse(time.RFC3339Nano, row.ReceivedAt)
	if err != nil {
		return "", fmt.Errorf("unreadable received_at %q", row.ReceivedAt)
	}
	extension := ".pb"
	if strings.HasPrefix(row.ContentType, "application/json") {
		extension = ".json"
	}
	return fmt.Sprintf("%013d-%d%s", at.UnixMilli(), row.ID, extension), nil
}

// exportHeaders resolves what rides on every POST. `OTEL_EXPORTER_OTLP_HEADERS`
// is honoured because an operator who has already configured an exporter has
// already put the receiver's credentials there; `--header` wins over it,
// because a flag on this command line is the more specific statement.
func exportHeaders(environment string, flags headerList) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(environment, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || key == "" {
			continue
		}
		// The OTLP specification percent-encodes the values in this
		// variable; a value with nothing to decode survives unchanged.
		//
		// PathUnescape and not QueryUnescape: the latter is the
		// *form* encoding, where `+` means a space. These values are
		// RFC 3986 percent-encoding, where `+` is a literal — and a
		// bearer token is base64, whose alphabet contains `+`. Reading
		// it as a space corrupts the credential into a 401 the export
		// then treats as fatal.
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		out[strings.TrimSpace(key)] = value
	}
	for _, pair := range flags {
		key, value, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(key) == "" {
			return nil, usageErrorf("--header takes name=value, got %q", pair)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}

// partialSuccess renders what a receiver reported about spans it would not
// keep, in whichever encoding it answered in. Empty when it reported none.
func partialSuccess(contentType string, body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var (
		rejected int64
		message  string
	)
	if strings.HasPrefix(contentType, "application/json") {
		rejected, message = mapping.DecodeExportResponseJSON(body)
	} else {
		rejected, message = mapping.DecodeExportResponse(body)
	}
	switch {
	case rejected > 0 && message != "":
		return fmt.Sprintf("%d rejected spans: %s", rejected, message)
	case rejected > 0:
		return fmt.Sprintf("%d rejected spans", rejected)
	case message != "":
		return message
	}
	return ""
}

// firstLine trims a receiver's error body to something that fits on a line of
// a terminal: the status and the first sentence are what a person acts on.
func firstLine(body []byte) string {
	text := strings.TrimSpace(string(body))
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		// At a character boundary: half a character is a byte the
		// terminal would be shown as `\xNN`.
		// A character is at most utf8.UTFMax bytes, so a start is never
		// further back than that — and a body that is not text at all is
		// cut where it is rather than walked back to nothing.
		cut := 200
		for back := 0; back < utf8.UTFMax && cut > 0 && !utf8.RuneStart(text[cut]); back++ {
			cut--
		}
		if !utf8.RuneStart(text[cut]) {
			cut = 200
		}
		text = text[:cut] + "…"
	}
	return text
}

// asStop unwraps a stop, for the retry loop's last error.
func asStop(err error, target **stopError) bool {
	stop, ok := err.(*stopError)
	if ok {
		*target = stop
	}
	return ok
}

// encodeRawCursor rebuilds the archive listing's cursor. The grammar is the
// server's, and it is stated once here so that a resume point and a page's
// cursor cannot mean different things.
func encodeRawCursor(receivedAt, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(receivedAt, 10) + ":" + strconv.FormatInt(id, 10)))
}
