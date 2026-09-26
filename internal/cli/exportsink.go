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
func (r *run) destination(to, dir string, headers map[string]string, compress, resuming bool) (destination, error) {
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

func (r *run) receiver(target string, headers map[string]string, compress bool) (destination, error) {
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
	backoff := r.opt.retryBackoff
	if backoff <= 0 {
		backoff = defaultRetryBackoff
	}
	return &receiver{
		run:     r,
		url:     target,
		headers: headers,
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

// exportHeaders resolves what rides on every POST: the `--header` flags and
// nothing else (spec 019 #14).
//
// `OTEL_EXPORTER_OTLP_HEADERS` is not read. On a machine that sends traces to
// this server it holds this server's project key — the docs say to put it
// there — and an export that copied it into every POST handed that key, a
// project admin's, to whichever receiver `--to` named. The variable is for an
// application's exporter; a receiver's credentials are named on the command
// line, for that receiver.
//
// Names are canonicalized, so `authorization` and `Authorization` are one
// header, and the last flag naming it wins: which value goes out is decided by
// the order the command line gives, never by the order of a map. A name or a
// value HTTP cannot carry is a usage error here, not a transport error on the
// first POST that the retry loop would spend half a minute on.
//
// Everything on the way out — each header value, and `--to` itself — is
// checked for a Tracepad key before anything is sent (refuseTracepadKey).
func (r *run) exportHeaders(to string, flags headerList, allowKey bool) (map[string]string, error) {
	out := map[string]string{}
	var names []string
	for _, pair := range flags {
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, usageErrorf("--header takes name=value, got %q", pair)
		}
		if !validHeaderName(key) {
			return nil, usageErrorf("--header name %q is not an HTTP header name", key)
		}
		if !validHeaderValue(value) {
			return nil, usageErrorf("--header %s holds a line break or another control character, "+
				"which no HTTP header value may", key)
		}
		name := http.CanonicalHeaderKey(key)
		if reservedHeaders[name] {
			return nil, usageErrorf("--header %s is set by the export itself: each batch goes out "+
				"under the type and encoding it has, and --gzip is how to compress it", name)
		}
		if _, seen := out[name]; !seen {
			names = append(names, name)
		}
		out[name] = value
	}
	for _, name := range names {
		if err := r.refuseTracepadKey("--header "+name, out[name], allowKey); err != nil {
			return nil, err
		}
	}
	// The URL whole and raw — user info, path and query as they will go out:
	// Go turns user info into an Authorization header of its own, and sends
	// the path and query verbatim, pairs a query parser would drop included.
	// User info is not refused as such — a collector behind Basic auth is
	// reached that way — only a Tracepad key in it is.
	if err := r.refuseTracepadKey("--to", to, allowKey); err != nil {
		return nil, err
	}
	if r.opt.Env("OTEL_EXPORTER_OTLP_HEADERS") != "" {
		// Said whenever it is set, and without the value: its reader may
		// have kept the receiver's credentials there and passed some other
		// --header, and their first sign would otherwise be a 401.
		fmt.Fprintln(r.opt.Stderr, "tracepad: OTEL_EXPORTER_OTLP_HEADERS is not read by export; "+
			"pass the receiver's credentials with --header")
	}
	return out, nil
}

// reservedHeaders are the ones the export sets per batch. A --header naming
// one would relabel every body: protobuf announced as JSON, a plain body as
// gzip — and the receiver's 400 would stop the export.
var reservedHeaders = map[string]bool{
	"Content-Type": true, "Content-Encoding": true, "Content-Length": true, "Host": true,
}

// refuseTracepadKey refuses text bound for the receiver that holds a Tracepad
// key (spec 019 #14). It reads no authorization scheme — Bearer, Basic,
// Token, a query parameter, a path segment are all just text — so it is a
// superset of whatever the server takes for a credential, and nothing is left
// to keep in step with it. It looks at the text as given, percent-decoded,
// and with every run that could be base64 decoded, one level:
//
//   - one of the command's own keys — the one it reads the archive with and,
//     when set, TRACEPAD_ADMIN_TOKEN — is refused always; no receiver has a
//     use for the source's credentials, and --allow-tracepad-key does not
//     change that;
//   - `tp-sk-` in any case is refused unless --allow-tracepad-key says the
//     receiver is a Tracepad of the same owner and the key is its own.
//
// The error names where the key was, never the key.
func (r *run) refuseTracepadKey(where, text string, allowKey bool) error {
	own := []string{r.key, r.opt.Env("TRACEPAD_ADMIN_TOKEN")}
	for _, candidate := range readings(text) {
		for _, key := range own {
			if key != "" && holdsKey(candidate, key) {
				// A refusal, not a usage error: the message is the whole
				// point, and the synopsis printed under it would bury it.
				return fmt.Errorf("%s carries a key this command holds for your Tracepad "+
					"(TRACEPAD_API_KEY, --key or TRACEPAD_ADMIN_TOKEN); the receiver would get "+
					"admin access to your project. --allow-tracepad-key does not change that: give "+
					"the receiver its own credentials", where)
			}
		}
		if !allowKey && strings.Contains(strings.ToLower(candidate), "tp-sk-") {
			return fmt.Errorf("%s carries a Tracepad project key; the receiver would get admin "+
				"access to your project. Give it the receiver's own credentials, or pass "+
				"--allow-tracepad-key if the receiver is a Tracepad server of yours and the key "+
				"is its own", where)
		}
	}
	return nil
}

// readings is text as the receiver might read it: as given, percent-decoded,
// and every run of it that decodes as base64 — padded or not, standard or
// URL-safe — decoded. The runs are cut three ways, because `/` and `+` belong
// to the standard alphabet and `-` and `_` to the URL-safe one, and a path or
// a query puts `/` and `=` around a token.
func readings(text string) []string {
	out := []string{text}
	if decoded, err := url.PathUnescape(text); err == nil && decoded != text {
		out = append(out, decoded)
	}
	for _, surface := range append([]string(nil), out...) {
		for _, alphabet := range []string{"+/-_", "+/", "-_"} {
			runs := strings.FieldsFunc(surface, func(c rune) bool {
				return !isAlnum(c) && !strings.ContainsRune(alphabet, c)
			})
			for _, run := range runs {
				if decoded, ok := base64Text(run); ok {
					out = append(out, decoded)
				}
			}
		}
	}
	return out
}

// base64Text decodes a run that is long enough to hide a key, in the first
// encoding that takes it.
func base64Text(run string) (string, bool) {
	if len(run) < 8 {
		return "", false
	}
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(run); err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

// holdsKey reports whether text holds key. A key of 16 characters or more is
// found anywhere: nothing ordinary contains one by chance. A shorter one —
// an operator's admin token can be any string — is found only as a whole
// token, bounded by characters no key is made of, so `dev` is not found in
// `development`.
func holdsKey(text, key string) bool {
	if len(key) >= 16 {
		return strings.Contains(text, key)
	}
	for from := 0; ; {
		at := strings.Index(text[from:], key)
		if at < 0 {
			return false
		}
		at += from
		end := at + len(key)
		if (at == 0 || !isKeyByte(text[at-1])) && (end == len(text) || !isKeyByte(text[end])) {
			return true
		}
		from = at + 1
	}
}

func isAlnum(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// isKeyByte is what a key token is made of: letters, digits, `-`, `_`, `.`
// and `~`. Anything else — space, `:`, `=`, `/`, `&`, `;` — is where a token
// ends.
func isKeyByte(c byte) bool {
	return isAlnum(rune(c)) || c == '-' || c == '_' || c == '.' || c == '~'
}

// validHeaderName is RFC 7230's token: the characters a header name may hold.
func validHeaderName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return name != ""
}

// validHeaderValue refuses the control characters RFC 7230 keeps out of a
// field value — a line break above all, which would end the header — and
// allows the tab it lets in.
func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
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
