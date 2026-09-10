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
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
	err := s.writer.Submit(r.Context(), job)
	if err == nil {
		return true
	}
	s.submitFailure(w, err)
	return false
}

// submitFailure renders an outcome the writer already answered with. It is the
// tail of submit, split out for the handlers that recognise one error of their
// own before falling back to the shared shapes (spec 028: a wrong current
// password, a spent invitation).
func (s *Server) submitFailure(w http.ResponseWriter, err error) {
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
		// The same backpressure ingest gives an exporter (spec 002 #15).
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "write queue is full, retry shortly")
	case errors.Is(err, store.ErrWriterClosed):
		writeError(w, http.StatusServiceUnavailable, "server is shutting down")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The client hung up; there is nobody left to answer.
	default:
		slog.Error("write failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to store the write")
	}
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

// readAPIBody reads and size-caps a request body (spec 003, API contract).
func (s *Server) readAPIBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "cannot read request body")
		return nil, false
	}
	return body, true
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
	if decoder.More() {
		return errors.New("the body must carry exactly one JSON value")
	}
	return nil
}

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
	return errors.New("malformed JSON body")
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
	seen := make(map[string]bool, len(items))
	list := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("%s: empty item in list", name)
		}
		if seen[item] {
			continue
		}
		seen[item] = true
		list = append(list, item)
	}
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
	if len(list) > facetCap {
		return nil, fmt.Errorf("%s: at most %d values in a list", name, facetCap)
	}
	return list, nil
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

// writeJSON renders a response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("failed to write response", "err", err)
	}
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
