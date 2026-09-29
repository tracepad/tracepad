package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/store"
)

// The native JSON API (spec 003): endpoints an application, an agent or a
// human with curl calls directly, as opposed to the OTLP surface an exporter
// speaks. Shared plumbing lives here — authentication, strict decoding,
// RFC 3339, pagination — so each endpoint file carries only its own rules.
//
// No `Content-Type` is required on these routes (Decision 19, 2026-08-27):
// `curl -d '{…}'` sends `application/x-www-form-urlencoded`, and Decision 1
// measures this surface by exactly that command. A body that is not JSON
// fails at the parser with a 400 either way.

// Pagination bounds (#18).
const (
	defaultPageSize = 50
	maxPageSize     = 500
)

// nameGrammar is the shape of a prompt or label name: one URL path segment,
// starting with a letter or a digit (spec 003, API contract).
var nameGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hexID is the shape of a score id — the same 32 lower-case hex characters
// the server generates when a client sends none (#3).
var hexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

const maxNameLength = 200

// apiProject is which project a data-plane request is about.
//
// It asks nothing and refuses nothing: the guard resolved the credential, the
// policy and the project before the handler ran (spec 028 Decision 7), so by
// here there is one — a key's own, or the one a session named with
// `X-Tracepad-Project`. The remaining branch is a handler reached without the
// guard, which is a programming error rather than a request, and a 500 says so
// without taking the process down.
func (s *Server) apiProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	c := callerFrom(r.Context())
	if c == nil || c.project == nil {
		slog.Error("a data-plane handler ran without a project", "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "the request was not authorized")
		return nil, false
	}
	return c.project, true
}

// submit hands a job to the group-commit writer and renders every outcome but
// success, so a handler's happy path is the line after it (#9).
func (s *Server) submit(w http.ResponseWriter, r *http.Request, job store.WriteJob) bool {
	if s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "writes are not available")
		return false
	}
	err := s.writer.Submit(writeContext(r), job)
	if err == nil {
		return true
	}
	submitFailure(w, err, apiWrite)
	return false
}

// writeKind is what submitFailure's answers call one kind of write: the JSON
// API's, or an ingest batch's.
type writeKind struct {
	// full is the `429` for a full queue; failed the `500` for a write that
	// failed on its own, and logged the line that goes with it, with attrs.
	full, failed, logged string
	attrs                []any
}

var apiWrite = writeKind{
	full:   "write queue is full, retry shortly",
	failed: "failed to store the write",
	logged: "write failed",
}

// fullRetryAfter is how long a refusal at a bound the project has reached asks
// the client to wait (spec 041 #31): long enough for the spans in flight to
// settle the refs that fill it.
const fullRetryAfter = "60"

// submitFailure renders an outcome the writer already answered with. It is the
// tail of submit, split out for the handlers that recognise one error of their
// own before falling back to the shared shapes (spec 028: a wrong current
// password, a spent invitation), and for ingest, whose writes fail the same
// ways (spec 043 #24 u).
func submitFailure(w http.ResponseWriter, err error, kind writeKind) {
	var rejection *store.Rejection
	switch {
	case errors.As(err, &rejection):
		// A refusal from inside the write transaction: the caller asked
		// for something the stored state does not allow, which is a 4xx
		// however deep in the pipeline it was detected.
		status := http.StatusBadRequest
		switch rejection.Kind {
		case store.RejectNotFound:
			status = http.StatusNotFound
		case store.RejectConflict:
			status = http.StatusConflict
		case store.RejectForbidden:
			status = http.StatusForbidden
		case store.RejectFull:
			// Room comes back as the refs in flight settle (spec 041
			// #31); the SDK retries a 429 after this long.
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", fullRetryAfter)
		}
		if len(rejection.Details) == 0 {
			writeError(w, status, rejection.Message)
			return
		}
		// A refusal the caller has to act on carries what it needs to
		// (spec 021 #14). Keys in order, so one refusal is one body.
		body := object{}.put("error", rejection.Message)
		for _, key := range slices.Sorted(maps.Keys(rejection.Details)) {
			body = body.put(key, rejection.Details[key])
		}
		writeJSON(w, status, body)
	case errors.Is(err, store.ErrWriterBusy):
		// Backpressure a client can act on: OTLP exporters retry 429 with
		// backoff natively (spec 002 #15).
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, kind.full)
	case errors.Is(err, store.ErrWriterClosed):
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client hung up; there is nobody left to answer.
	default:
		if _, ok := store.Condition(err); ok {
			// A lock that did not clear, a full disk, an I/O error: the
			// database's condition, which passes, so the answer is one
			// a client retries (spec 043 #2). Everything else is the
			// write's own and would fail again on every retry — a retry
			// loop over a poison batch is worse than the loss, so it
			// stays a 500. The writer has logged it, once a minute.
			retryLater(w, storageUnavailable)
			return
		}
		slog.Error(kind.logged, slices.Concat(kind.attrs, []any{"err", err})...)
		writeError(w, http.StatusInternalServerError, kind.failed)
	}
}

// storageUnavailable is the answer for a write a database condition failed
// (spec 043 #2): a lock that did not clear, a full disk, an I/O error.
const storageUnavailable = "storage is temporarily unavailable; retry shortly"

// retryLater is the one `503` that asks to be retried (spec 043 #1, #2): a
// credential that could not be checked and a write a database condition
// failed both pass on their own, so the status is one a client retries.
func retryLater(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, message)
}

// readJSON reads the request body under the configured cap and decodes it
// into v, answering the client itself on every failure.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return false
	}
	if err := decodeStrict(body, v); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// maxItemsPerWrite bounds the rows of one array write (spec 043 #36). The
// body cap bounds bytes, and half a million minimal scores fit in it; each
// array is one transaction, so its count is how long it holds the only
// writer. 10,000 commits in under a second, and is a hundred times the batch
// the SDKs send scores in.
const maxItemsPerWrite = store.MaxItemsPerWrite

// decodeBatch reads a write's body as one object or an array of them — the
// shape every batch write takes — with the strictness of a single object in
// both (spec 003 #17). What an array is judged on before any value is
// decoded into a T is one scan of its bytes (scanArray): an array that never
// closes is malformed, a value after it is more than one JSON value, and one
// of more than limit values is an *overItemCap. So the answer to such a body
// is by its shape and its count, whatever the values hold, and costs no
// memory per value. Only a body that can be over the limit is scanned: one of
// at least minBodyOver(limit) bytes with at least limit commas, since more
// than that many values are separated by at least that many. Any other body —
// the batches an SDK sends, and every body of another shape — goes to the
// strict decode, whose cost is bounded by the limit. noun names one value in
// the messages: "score", "item", "target". The limit's answer is the
// caller's: 413 for scores and items (writeBatchError), 400 for a queue add.
func decodeBatch[T any](body []byte, noun string, limit int) ([]*T, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '[' {
		var one T
		if err := decodeStrict(body, &one); err != nil {
			return nil, err
		}
		return []*T{&one}, nil
	}
	if len(trimmed) >= minBodyOver(limit) && bytes.Count(trimmed, []byte{','}) >= limit {
		switch scan := scanBody(trimmed); {
		case !scan.closed:
			return nil, errMalformedBody
		case scan.trailing:
			return nil, errTrailingValue
		case scan.count > limit:
			return nil, &overItemCap{count: scan.count, limit: limit, kind: noun + "s"}
		}
	}
	var batch []*T
	if err := decodeStrict(body, &batch); err != nil {
		return nil, err
	}
	for i, value := range batch {
		if value == nil {
			return nil, fmt.Errorf("%s at index %d is null", noun, i)
		}
	}
	return batch, nil
}

// scanBody is scanArray, a variable so that a test can count the scans: that
// an ordinary batch is not scanned is a promise of spec 043 #36, and the
// answers are the same whether it was or not.
var scanBody = scanArray

// minBodyOver is the length below which an array cannot hold more than limit
// values: n values take at least 2n+1 bytes — n one-byte values, n−1 commas
// and two brackets — so a shorter body is not scanned. An SDK's batch of 100
// scores is most often under it, and pays nothing.
func minBodyOver(limit int) int { return 2*(limit+1) + 1 }

// arrayScan is what scanArray says of the first array of a body.
type arrayScan struct {
	count    int  // its top-level values: the commas between them plus one, 0 for none — exact for a well-formed array
	closed   bool // its closing bracket was reached
	trailing bool // something but white space follows the closing bracket
}

// scanArray reads a body that starts with '[' once, byte by byte, holding
// nothing: it follows strings — a bracket, a comma or an escaped quote inside
// one is not structure — and the depth of brackets and braces, counts the
// commas of the first level, and stops at the bracket that closes the array.
// It is not a validator: an array whose middle is malformed is counted all
// the same, and a count over the cap wins over the malformedness (spec 003
// #28). A 20 MiB array of ten million zeros is scanned in about 55 ms and a
// few dozen bytes (spec 043 #36).
func scanArray(body []byte) arrayScan {
	var scan arrayScan
	depth, commas := 1, 0
	sawValue, inString, escaped := false, false, false
	for i := 1; i < len(body); i++ {
		c := body[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			continue
		}
		if depth == 1 && c != ',' && c != ']' {
			sawValue = true
		}
		switch c {
		case '"':
			inString = true
		case '[', '{':
			depth++
		case ']', '}':
			if depth--; depth == 0 {
				scan.closed = true
				scan.trailing = len(bytes.TrimLeft(body[i+1:], " \t\r\n")) > 0
				if sawValue {
					scan.count = commas + 1
				}
				return scan
			}
		case ',':
			if depth == 1 {
				commas++
			}
		}
	}
	return scan
}

// overItemCap is an array write over the limit it was read with: a 413 for
// scores and items, and for a queue add the 400 that says what to use instead.
type overItemCap struct {
	count int
	limit int
	kind  string
}

func (e *overItemCap) Error() string {
	return fmt.Sprintf("this request carries %d %s; the server takes at most %d per request — send them in batches",
		e.count, e.kind, e.limit)
}

// writeBatchError answers a body decodeBatch refused, and counts it in the
// project's counters when it was refused for its number of values (spec 043
// #40).
func (s *Server) writeBatchError(w http.ResponseWriter, projectID string, kind batchKind, err error) {
	status := http.StatusBadRequest
	if s.countOverBatchCap(projectID, kind, err) {
		status = http.StatusRequestEntityTooLarge
	}
	writeError(w, status, err.Error())
}

// readAPIBody reads and size-caps a request body (spec 003, API contract).
func (s *Server) readAPIBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := readBody(w, r, s.maxBodyBytes)
	return body, s.bodyRead(w, r, err)
}

// bodyRead answers the client itself when reading the body failed: 429 for
// the body budget (spec 043 #13), 413 for the cap, 400 for anything else. A
// gzip body that fit on the wire and expanded past the cap is also logged,
// because to an OTLP exporter a 413 is final — the batch is dropped, and this
// line is where the operator finds out why (spec 002 #27).
func (s *Server) bodyRead(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, errBodyBudget) {
		s.refuseBodyForBudget(w, r, callerProject(r), s.maxBodyBytes)
		return false
	}
	var inflated *inflatedTooLarge
	if errors.As(err, &inflated) {
		if skipped, ok := s.inflatedLog.Allow("", time.Now()); ok {
			slog.Warn("a gzip body expanded past TRACEPAD_MAX_BODY_BYTES after decompression and was refused with 413",
				"path", r.URL.Path, "wire_bytes_read", inflated.wire, "limit", inflated.limit,
				"not_logged_since_last", skipped.SameKey)
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "cannot read request body")
	return false
}

// writeContext is the context a write waits for its commit under. A request
// that read a body waits whether or not its client stays, so that the body it
// holds stays counted in the budget until the job is written (spec 043 #31):
// the writer commits a queued job all the same. One that read none — a
// deletion, a bulk round of them — keeps its client's cancellation, and a
// client that hangs up stops the round at the chunk in progress (spec 043
// #33).
func writeContext(r *http.Request) context.Context {
	if hold := holdFrom(r.Context()); hold != nil && hold.reserved > 0 {
		return context.WithoutCancel(r.Context())
	}
	return r.Context()
}

// refuseBodyForBudget answers a body the budget could not hold (spec 043 #13)
// and counts it for the project it was about, if any (#21). What the request
// had reserved is given back first, and the answer is written before the rest
// of the body — up to `length` — is read and dropped, as a refused media
// upload's is (spec 041 #31, spec 043 #31): a client still sending would
// otherwise meet a closed connection instead of the `429` it retries, and the
// drain costs the budget nothing.
func (s *Server) refuseBodyForBudget(w http.ResponseWriter, r *http.Request, projectID string, length int64) {
	if hold := holdFrom(r.Context()); hold != nil {
		hold.releaseAll()
	}
	s.counters.observeBodyRefused(projectID)
	answerThenDrain(w, r, length, func() {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, bodyBusy)
	})
}

// maxPublicBodyBytes caps the body of a route anyone can call (spec 028
// Decision 26). The largest such body is a setup — a token, an email, a
// password and a name — at most about 4.7 KiB with every character escaped
// (a name of astral characters as surrogate pairs is 12 bytes a character),
// so 8 KiB is room for any client and nothing for an attacker.
const maxPublicBodyBytes = 8 << 10

// publicBody is what the guard puts in front of every public route that can
// carry a body (spec 028 Decisions 26 and 29). Those routes run before anything
// knows who is calling, and every refusal here comes before the handler, and
// so before a byte of the body is read:
//
//   - a browser request from another origin is 403, read the way the cookie
//     routes read it: the `Origin`, or the `Referer` when that is `null`
//     (Decision 30). Only a request that carries an `Origin` is checked, and
//     a browser sends one on every POST. One without passes whatever its
//     `Referer` says — the CLI, an SDK, curl — because the check is about a
//     page someone else wrote making a browser sign in (Decision 29);
//   - a body that is not declared `application/json` is 415, because the one
//     body a plain HTML form can send without a preflight that is also valid
//     JSON is `text/plain`, and no client of these routes sends that;
//   - a compressed body is 415 and a body over a few KiB is 413 (Decision
//     26): decompressing is work done for a caller nobody has identified,
//     and the configured cap is sized for trace batches, not for an email and
//     a password.
func (s *Server) publicBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" && !s.ownOrigin(r, statedOrigin(r)) {
			s.refuseOrigin(w, r)
			return
		}
		if !plainEncoding(r) {
			writeError(w, http.StatusUnsupportedMediaType, "this route takes an uncompressed body")
			return
		}
		if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
			mediaType != contentTypeJSON {
			writeError(w, http.StatusUnsupportedMediaType, "this route takes Content-Type: application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxPublicBodyBytes)
		// These are the routes that check a password and then open a
		// session, and both ask where the request comes from: it is
		// worked out once (spec 046 #16).
		next(w, withClientMemo(r))
	}
}

// plainEncoding reports whether a request declares no content coding but
// `identity` — in every Content-Encoding header it carries, and in every
// comma-separated token of each, since a coding hidden behind the first one
// is still a coding.
func plainEncoding(r *http.Request) bool {
	for _, value := range r.Header.Values("Content-Encoding") {
		for _, coding := range strings.Split(value, ",") {
			coding = strings.TrimSpace(coding)
			if coding != "" && !strings.EqualFold(coding, "identity") {
				return false
			}
		}
	}
	return true
}

// decodeStrict decodes one JSON value, refusing fields the target does not
// declare. Agent-first cuts both ways (#17): an unattended agent that typos
// `commet` must get an error rather than silently lose its comment.
func decodeStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return decodeError(err)
	}
	// Anything after the value but white space is more than one value. Not
	// decoder.More(): it answers whether an array or object being read has
	// another element, and takes a `]` or `}` for the end of one nobody
	// opened, so `{"input":1}]` passed.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errTrailingValue
	}
	return nil
}

// What a body that is not one JSON value of the shape asked for is answered.
var (
	errTrailingValue = errors.New("the body must carry exactly one JSON value")
	errMalformedBody = errors.New("malformed JSON body")
)

// decodeError turns encoding/json's internal wording into a message written
// for whoever sent the body.
func decodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("the request body is empty")
	}
	const unknownField = "json: unknown field "
	if message := err.Error(); strings.HasPrefix(message, unknownField) {
		return fmt.Errorf("unknown field %s", strings.TrimPrefix(message, unknownField))
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && typeError.Field != "" {
		return fmt.Errorf("field %q is a %s, want %s", typeError.Field, typeError.Value, typeError.Type)
	}
	return errMalformedBody
}

// queryParams returns the request's query after refusing any parameter the
// endpoint does not know. A mistyped filter that silently widened a listing
// would mislead exactly the unattended caller #17 protects, so query strings
// are as strict as bodies (Decision 21, 2026-08-27).
func queryParams(r *http.Request, known ...string) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("malformed query string")
	}
	for key, given := range values {
		if !slices.Contains(known, key) {
			return nil, fmt.Errorf("unknown query parameter %q (accepted: %s)",
				key, strings.Join(known, ", "))
		}
		// A parameter present with no value is a client whose template
		// left a variable unset. Reading it as "not given" is how
		// `?label=$LABEL` silently returns the latest prompt instead of
		// the released one (#23).
		for _, value := range given {
			if value == "" {
				return nil, fmt.Errorf("query parameter %q was given without a value", key)
			}
		}
	}
	return values, nil
}

// lookupLabel is a label a request looks something up by — a filter, a user or
// session in the path, the user an erasure names, the session a score is
// given — cut as ingest cuts what it stores (spec 043 #14, #34). Looked up
// whole, a value longer than the bound would match nothing: a filter that
// comes back empty, an erasure that erases nothing.
func lookupLabel(value string) string { return mapping.CutLabel(value) }

// filterList reads a filter that takes a comma-separated list — *any of* its
// items (spec 027 #1) — and answers nothing at all when the parameter is
// absent.
//
// One parameter reads as one filter in a URL, a shell and a chip, which is
// where these values are typed and shown; `?environment=production,staging` is
// the string a person writes unprompted. `tag` keeps its repeatable form and
// its AND, because two spellings for two semantics is clearer than one
// spelling for both.
//
// Items are trimmed, so `a, b` is the pair it looks like. An empty item is a
// `400` on spec 003 #23's rule and for its reason: `?environment=$ENV,staging`
// with the variable unset is a template that did not fill in, and reading it
// as "just staging" would answer a broken request with a well-formed listing.
// Duplicates collapse, which costs the SQL one placeholder and the reader
// nothing.
//
// A value with a comma in it is not expressible this way, and `docs/api.md`
// says so: an identifier with a comma in it is a choice its owner made against
// every tool that will ever list it.
//
// Every such list is a list of labels, and each item is cut as ingest cuts
// what it stores (lookupLabel).
func filterList(values url.Values, name string) ([]string, error) {
	given := values[name]
	if len(given) == 0 {
		return nil, nil
	}
	// Repeating the parameter is a `400` rather than a silent first-wins.
	// `tag` beside it *is* repeatable and *is* an AND, so `?environment=a&
	// environment=b` is a client that reached for the wrong spelling of a
	// filter this endpoint advertises as many-valued — and answering it with
	// the traces of `a` alone is a listing narrower than the one that was
	// asked for, which is the failure spec 003 #23 refuses everywhere else.
	if len(given) > 1 {
		return nil, fmt.Errorf("%s was given more than once; pass one comma-separated list", name)
	}
	raw := given[0]
	if raw == "" {
		return nil, nil
	}
	items := strings.Split(raw, ",")
	for i, item := range items {
		items[i] = lookupLabel(strings.TrimSpace(item))
		if items[i] == "" {
			return nil, fmt.Errorf("%s: empty item in list", name)
		}
	}
	list, over := distinctCapped(items, facetCap)
	// A list longer than this is a `400`, not a `500` (spec 027 #20). Each
	// item becomes one bound parameter, and SQLite's limit on those is a few
	// tens of thousands: without the cap a long enough list reached the
	// driver and came back as `too many SQL variables`, which is an internal
	// error answering a malformed request. The number is `facetCap`: the
	// endpoint that fills these boxes in never offers more than a hundred
	// values, so a longer list is not something the interface can produce.
	// Out of range is an error rather than a silent truncation for the reason
	// `?limit=5000` is (#18): a client reasoning about a filter it will not
	// get should be told.
	if over {
		return nil, fmt.Errorf("%s: at most %d values in a list", name, facetCap)
	}
	return list, nil
}

// distinctCapped keeps the first appearance of each item, in order, and
// reports whether there are more than limit distinct ones — stopping as soon
// as there are, so a list of thousands costs what its first limit+1 do. The
// list filters and `?tag=` share it (spec 027 #20, spec 043 #17).
func distinctCapped(items []string, limit int) (list []string, over bool) {
	list = make([]string, 0, min(len(items), limit))
	seen := make(map[string]bool, cap(list))
	for _, item := range items {
		if seen[item] {
			continue
		}
		if len(list) == limit {
			return nil, true
		}
		seen[item] = true
		list = append(list, item)
	}
	return list, false
}

// pageSize reads `?limit` (#18). Out of range is an error rather than a silent
// clamp: a client asking for 5000 rows is reasoning about a page size it will
// not get.
func pageSize(values url.Values) (int, error) {
	raw := values.Get("limit")
	if raw == "" {
		return defaultPageSize, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxPageSize {
		return 0, fmt.Errorf("limit must be a whole number between 1 and %d", maxPageSize)
	}
	return limit, nil
}

// The listing page parameters spec 009 adds beside `limit` and `cursor`.
const (
	// countCap bounds how many rows a count will *count* — not how many the
	// query reads to find them. `LIMIT` ends a scan early only once that
	// many rows have matched (spec 009 #12, correcting #4). Above the cap
	// the answer is "1000+", which is the question the reader was asking.
	//
	// What that buys and what it does not: on a filter with many matches the
	// count stops almost at once, and on a selective filter over a column no
	// index covers it scans — but so does the listing beside it, and by more
	// (measured on 500k rows: 260 ms against the listing's 864 ms). A count
	// is not what makes such a screen slow.
	countCap = 1000
)

// pageDirection reads `?direction`. `next` walks towards older rows, `prev`
// towards newer ones; with no cursor they are the newest and the oldest page,
// which is how both end anchors are a direction rather than an offset (#2).
func pageDirection(values url.Values) (bool, error) {
	switch raw := values.Get("direction"); raw {
	case "", "next":
		return false, nil
	case "prev":
		return true, nil
	default:
		return false, fmt.Errorf("direction must be next or prev, not %q", raw)
	}
}

// wantsCount reads `?count`. Off by default: the count changes with the
// filters and not with the page, so a client turns it on when the filters move
// and pages without it (#4).
func wantsCount(values url.Values) (bool, error) {
	switch raw := values.Get("count"); raw {
	case "":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("count must be 1 or true, not %q", raw)
	}
}

// capped turns a raw count taken with a limit of countCap+1 into the pair the
// response carries: the number, and whether it stopped short of the truth.
func capped(raw int) (int, bool) {
	if raw > countCap {
		return countCap, true
	}
	return raw, false
}

/*
trimPage drops the probe row a listing asks for and names the pages on either
side of the one that is left.

A listing fetches one row more than it means to show. Which end that extra row
lands on is the whole of the bookkeeping here: scanning towards older rows it
is the oldest, and scanning towards newer ones — where the store hands back a
reversed page — it is the newest. Its presence means there is more *in the
direction of the scan*; a cursor having been given means there is something in
the other direction, because that is where the request came from.

The two cursors are then the edges of what survives: `prev` the newest row on
the page, `next` the oldest.
*/
func trimPage[T any](rows []T, limit int, backward bool, from string, key func(T) string) (
	kept []T, prev *string, next *string,
) {
	fromCursor := from != ""
	more := len(rows) > limit
	kept = rows
	if more {
		if backward {
			kept = rows[len(rows)-limit:]
		} else {
			kept = rows[:limit]
		}
	}
	if len(kept) == 0 {
		// A page reached by a cursor and found empty — its rows swept by
		// retention while somebody sat on it — still has a way back: the
		// cursor it came from, read the other way, is the page before it.
		// Answering with nothing at all would strand the caller on a URL
		// whose only escape is editing it (PR #11 review).
		if !fromCursor {
			return kept, nil, nil
		}
		if backward {
			return kept, nil, &from
		}
		return kept, &from, nil
	}
	hasPrev, hasNext := fromCursor, fromCursor
	if backward {
		hasPrev = more
	} else {
		hasNext = more
	}
	if hasPrev {
		edge := key(kept[0])
		prev = &edge
	}
	if hasNext {
		edge := key(kept[len(kept)-1])
		next = &edge
	}
	return kept, prev, next
}

// encodeCursor renders a sort key as the opaque string clients pass back.
// Opaque is the point: the columns behind a cursor are ours to change.
func encodeCursor(parts ...string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}

// decodeCursor restores a cursor of the expected width.
func decodeCursor(value string, parts int) ([]string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid cursor")
	}
	fields := strings.SplitN(string(raw), ":", parts)
	if len(fields) != parts {
		return nil, errors.New("invalid cursor")
	}
	return fields, nil
}

// Unix nanoseconds in an int64 span 1678–2262; outside that window
// time.Time.UnixNano() is undefined and wraps silently, which would turn a
// typo'd year into a plausible-looking timestamp three centuries off (#23).
var (
	minTimestamp = time.Unix(0, math.MinInt64).UTC()
	maxTimestamp = time.Unix(0, math.MaxInt64).UTC()
)

// parseTime reads an RFC 3339 timestamp into Unix nanoseconds (#16).
func parseTime(field, value string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an RFC 3339 timestamp, got %q", field, value)
	}
	if parsed.Before(minTimestamp) || parsed.After(maxTimestamp) {
		return 0, fmt.Errorf("%s must be between %s and %s, got %q",
			field, minTimestamp.Format(time.RFC3339), maxTimestamp.Format(time.RFC3339), value)
	}
	return parsed.UnixNano(), nil
}

// formatTime renders Unix nanoseconds as RFC 3339 UTC. Storage stays in
// nanoseconds; the API boundary speaks ISO (#16).
func formatTime(nanoseconds int64) string {
	return time.Unix(0, nanoseconds).UTC().Format(time.RFC3339Nano)
}

// validName checks a prompt or label name against the grammar that keeps it a
// single, unescaped URL path segment.
func validName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if len(value) > maxNameLength {
		return fmt.Errorf("%s must be at most %d characters", kind, maxNameLength)
	}
	if !nameGrammar.MatchString(value) {
		return fmt.Errorf("%s %q must match %s", kind, value, nameGrammar)
	}
	return nil
}

// writeJSON renders a response body. The body is encoded before the status is
// written (spec 043 #3): a value that does not encode — a nesting limit, a
// number JSON cannot spell — is a `500` that says so, never a `200` with an
// empty body, which a client cannot tell from an empty answer and a cache may
// keep. The bytes of a body that encodes are what `json.Encoder` wrote,
// trailing newline included.
func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("failed to render the response", "type", fmt.Sprintf("%T", body), "err", err)
		// The handler may have set how long its answer keeps — a prompt
		// sets a minute — and a failure must not be kept at all.
		header := w.Header()
		header.Del("ETag")
		header.Del("Last-Modified")
		header.Set("Cache-Control", "no-store")
		writeError(w, http.StatusInternalServerError, "failed to render the response")
		return
	}
	writeEncoded(w, status, encoded)
}

var newline = []byte{'\n'}

// writeEncoded writes a body already encoded — by writeJSON, or by a handler
// that renders in a pass of its own, as the trace tree does (spec 043 #18).
func writeEncoded(w http.ResponseWriter, status int, encoded []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The newline json.Encoder ended every answer with before (spec 043 #3),
	// written on its own: appended, it could copy a body of tens of
	// megabytes to add one byte.
	if _, err := w.Write(encoded); err != nil {
		// A write the deadline stopped is the transport's line, once a
		// minute per route (reportCutResponses).
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			slog.Error("failed to write response", "err", err)
		}
		return
	}
	w.Write(newline)
}

// jsonValue reports whether raw carries a JSON value at all: an absent field
// and an explicit `null` both mean "nothing was sent".
func jsonValue(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// compactJSON strips insignificant whitespace from a value that is stored
// verbatim, so what comes back out is not padded by how it was sent.
//
// It cannot fail on anything a handler passes it: the input is a
// json.RawMessage lifted out of a body the decoder already accepted. If that
// ever stopped being true, the bytes as sent are the honest thing to store.
func compactJSON(raw json.RawMessage) json.RawMessage {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return raw
	}
	return buffer.Bytes()
}
