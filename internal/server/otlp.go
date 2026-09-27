package server

import (
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/store"
)

// OTLP/HTTP trace ingest (spec 002, widened by spec 019 #7). Two encodings
// now: the protobuf both SDK families we target speak, and the JSON every
// OpenTelemetry SDK can emit with `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`.
// gRPC is still out — it is a second transport rather than a second spelling.

// contentTypeProtobuf is what the OTLP spec prescribes. `application/protobuf`
// is accepted too — some exporters send it, and refusing a body we can decode
// only teaches users to distrust the endpoint.
const (
	contentTypeProtobuf    = "application/x-protobuf"
	contentTypeProtobufAlt = "application/protobuf"
	contentTypeJSON        = mapping.ContentTypeJSON
)

// handleTraces serves both the canonical OTLP route and the Langfuse-SDK
// alias. They are the same endpoint: one key pair serves both wire formats,
// so a client that guessed the wrong path still works (spec 002 #2).
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "ingest is not available")
		return
	}

	// The guard resolved the key and refused everything that is not one:
	// `ingest` is the one policy where a person's session and the admin
	// token are not credentials at all (spec 028 Decision 3).
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}

	mediaType, ok := acceptableContentType(r.Header.Get("Content-Type"))
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType,
			"expected Content-Type "+contentTypeProtobuf+" or "+contentTypeJSON)
		return
	}
	// The encoding is transport: it decides how the bytes are read and how
	// the answer is written, and nothing between those two points
	// (spec 019 #7).
	jsonEncoding := mediaType == contentTypeJSON

	// Early drift signal, never enforcement: refusing an unknown SDK
	// version would break users on newer SDKs for nothing, and the raw
	// body means we can always catch up retroactively (spec 002 #17). The
	// counter half of #17 is `GET /api/v1/system` (spec 004 #10).
	if v := r.Header.Get("x-langfuse-ingestion-version"); v != "" {
		s.counters.observeSDKVersion(project.ID, v)
		slog.Info("langfuse ingestion version", "version", v, "project", project.Name)
	}

	encoding := r.Header.Get("Content-Encoding")
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}

	decoded, err := mapping.DecodeExportBody(body, jsonEncoding)
	if err != nil {
		s.counters.observeRejectedBatch(project.ID)
		slog.Warn("undecodable OTLP body", "project", project.Name, "err", err)
		// The JSON path names the field, because it can: a body that is
		// protobuf-JSON rather than OTLP/JSON differs in one place, and
		// saying which one is the difference between a fixable answer
		// and a shrug (spec 019, API contract).
		writeError(w, http.StatusBadRequest, decodeFailure(jsonEncoding, err))
		return
	}
	resourceSpans, unreadable := decoded.ResourceSpans, decoded.Unreadable
	if len(resourceSpans) == 0 && unreadable == 0 {
		// Per the OTLP spec an empty batch is a successful no-op; there
		// is nothing to store and nothing to replay later.
		writeExportResponse(w, nil, jsonEncoding)
		return
	}
	if unreadable > 0 {
		// Worth a log line even though the export still succeeds: it
		// means an exporter is emitting something we cannot read, and
		// the raw body below is the only way to find out what.
		slog.Warn("skipped unreadable resource spans",
			"project", project.Name, "count", unreadable)
	}

	// Media comes out before the mapper sees the export (spec 041 #1): the
	// walk rewrites the attributes in place, so the payloads the mapper
	// writes carry references, and the raw body below is this same export
	// with the media factored out (#5).
	received := body
	prepare := func(decoded *mapping.ExportBody, opts mapping.MediaOptions) (*store.IngestBatch, *mapping.Result) {
		resourceSpans := decoded.ResourceSpans
		media := mapping.ExtractMedia(resourceSpans, opts)
		result := mapping.Map(resourceSpans)
		result.NoteUnreadable(unreadable)
		batch := &store.IngestBatch{
			ProjectID:    project.ID,
			Traces:       result.Traces,
			Observations: result.Observations,
			Resolved:     media.Resolved,
		}
		archived, rawMedia := received, false
		if s.storeRaw && media.Any() {
			factored, err := decoded.Encode(media.Rewrites)
			if err != nil {
				// The body as it arrived is still a true archive, only a
				// heavier one; losing the batch over it would not be.
				slog.Error("could not factor media out of the raw body; keeping it as received",
					"project", project.Name, "err", err)
			} else {
				archived, rawMedia = factored, true
			}
		}
		batch.Media, batch.MediaRefs, batch.RawMedia = mediaRows(media, result.Traces, rawMedia)
		if s.storeRaw {
			batch.Raw = &store.RawBatch{
				ReceivedAt: time.Now().UnixNano(),
				Dialect:    result.Dialect,
				// As received, never converted (spec 019 #8): a JSON batch
				// is kept as JSON, and the column is what tells a replay
				// which it is holding. The parsed media type rather than
				// the header verbatim, so a charset parameter does not
				// end up on the wire of a replay.
				ContentType:     mediaType,
				ContentEncoding: encoding,
				Body:            archived,
			}
		}
		return batch, result
	}
	batch, result := prepare(decoded, s.mediaOptions(r.Context(), project))
	if batch.Empty() {
		// Every span was skipped and raw storage is off: there is
		// nothing to commit, and the export is still a success. The
		// skipped spans are still counted — a client whose every span
		// is unmappable is exactly what the counters exist to surface.
		s.counters.observeBatch(project.ID, result.Dialect, 0, result.Skipped, int64(unreadable))
		writeExportResponse(w, result, jsonEncoding)
		return
	}

	err = s.writer.Submit(r.Context(), batch)
	if errors.Is(err, store.ErrMediaGone) {
		// A Langfuse upload the walk rewrote a string to was collected
		// before the write (spec 041 #9). Taken again without resolving,
		// the batch keeps the SDK's strings as sent — the evidence of the
		// picture — instead of a reference to nothing.
		again, decodeErr := mapping.DecodeExportBody(received, jsonEncoding)
		if decodeErr == nil {
			opts := s.mediaOptions(r.Context(), project)
			opts.Resolve = nil
			batch, result = prepare(again, opts)
			err = s.writer.Submit(r.Context(), batch)
		}
	}
	if err != nil {
		submitFailure(w, err, writeKind{
			full:   "ingest queue is full, retry shortly",
			failed: "failed to store spans",
			logged: "ingest write failed",
			attrs:  []any{"project", project.Name},
		})
		return
	}

	// 200 only now: the transaction is committed and fsynced, so this
	// answer means the spans are on disk (spec 002 #15).
	s.counters.observeBatch(project.ID, result.Dialect,
		int64(len(result.Observations)), result.Skipped, int64(unreadable))
	// A trace that named a run the project does not have was stored all
	// the same; it is counted, and each unknown id is logged once per
	// process (spec 014 #3). `GET /api/v1/system` carries the count.
	if len(batch.UnknownRuns) > 0 {
		for _, id := range s.counters.observeOrphanRuns(project.ID, batch.UnknownRuns) {
			slog.Warn("trace names a run that does not exist; it is stored but belongs to no run",
				"project", project.Name, "run_id", id)
		}
	}
	writeExportResponse(w, result, jsonEncoding)
}

// credential extracts the secret from either scheme. Basic carries
// public:secret — the Langfuse SDK's shape — and only the secret identifies
// the project.
func credential(header string) (string, bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	switch strings.ToLower(scheme) {
	case "bearer":
		value = strings.TrimSpace(value)
		return value, value != ""
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return "", false
		}
		_, secret, found := strings.Cut(string(decoded), ":")
		return secret, found && secret != ""
	}
	return "", false
}

// acceptableContentType resolves the request's declared encoding, answering
// the media type the body is in and whether this endpoint speaks it. The media
// type comes back parsed because it is also what the archive stores: a replay
// posts a body under the type it arrived in (spec 019 #8), and `; charset=utf-8`
// is a fact about that one request rather than about the bytes.
func acceptableContentType(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", false
	}
	switch mediaType {
	case contentTypeProtobuf, contentTypeProtobufAlt, contentTypeJSON:
		return mediaType, true
	}
	return mediaType, false
}

// decodeFailure is what a body that would not decode is told. The protobuf
// path has one sentence for every failure — there is nothing in a wire-format
// error a client can act on — and the JSON path passes its own message
// through, which names the field.
func decodeFailure(asJSON bool, err error) string {
	if !asJSON {
		return "malformed OTLP body"
	}
	return strings.TrimPrefix(err.Error(), mapping.ErrMalformedBody.Error()+": ")
}

// readBody reads the request body under the configured cap, transparently
// decompressing gzip (spec 002 API contract).
//
// The cap bounds the body twice: the bytes on the wire, and for gzip the bytes
// they decompress to — the body the parser gets — so a few hundred kilobytes
// of compressed zeros cannot become hundreds of megabytes (spec 002 #27). It
// is a per-request bound, not a memory budget: handler concurrency is
// unbounded, so N simultaneous requests can hold N bodies, and the writer
// queue (spec 002 #15) only sees a body after it has been read. An aggregate
// budget belongs with the rate limiting deferred to a later spec.
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	if !strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		return io.ReadAll(limited)
	}

	wire := &countingBody{reader: limited}
	gz, err := gzip.NewReader(wire)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	body, err := io.ReadAll(io.LimitReader(gz, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, &inflatedTooLarge{wire: wire.read, limit: maxBytes}
	}
	return body, nil
}

// inflatedTooLarge is a gzip body that fit the cap on the wire and expanded
// past it. It is a MaxBytesError to everything that answers 413, and its own
// type to the one place that logs it.
type inflatedTooLarge struct {
	// wire is how many compressed bytes had been read when the
	// decompressed stream passed the cap.
	wire  int64
	limit int64
}

func (e *inflatedTooLarge) Error() string {
	return fmt.Sprintf("gzip body expands past %d bytes", e.limit)
}

func (e *inflatedTooLarge) Unwrap() error { return &http.MaxBytesError{Limit: e.limit} }

// countingBody counts what is read through it.
type countingBody struct {
	reader io.Reader
	read   int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += int64(n)
	return n, err
}

// logLimiter lets one line through per interval and counts the rest, so a
// burst of refused bombs costs one log line a minute rather than one a
// request, and the line that does get through says how many it stands for.
//
// allowKey paces each key on its own, for a warning that friendly and hostile
// requests share: one sender repeating itself cannot hold the line another
// needs, and each line counts only its own key's repeats. keys caps how many
// keys an interval admits (one when zero), so a sender inventing a new key per
// request cannot fill the log either; what the cap turned away is counted
// apart and told with the next line let through.
type logLimiter struct {
	mu      sync.Mutex
	every   time.Duration
	keys    int
	seen    map[string]*logKey
	overCap int64
}

type logKey struct {
	at      time.Time
	skipped int64
}

// logHeld is what a line let through stands for: its own key's repeats since
// its last line, and the lines of other keys the cap turned away meanwhile.
type logHeld struct {
	sameKey, overCap int64
}

// allow reports whether to log now and how many were held back since the
// last line that was.
func (l *logLimiter) allow(now time.Time) (skipped int64, ok bool) {
	held, ok := l.allowKey("", now)
	return held.sameKey, ok
}

// allowKey is allow for one key among several.
func (l *logLimiter) allowKey(key string, now time.Time) (logHeld, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]*logKey{}
	}
	limit := max(l.keys, 1)
	k, known := l.seen[key]
	if known && now.Sub(k.at) < l.every {
		k.skipped++
		return logHeld{}, false
	}
	// The cap counts the keys logged within the interval, and a key coming
	// back after its own interval is held to it like a new one: otherwise
	// yesterday's keys, returning, would double what a minute may log.
	active := 0
	for other, o := range l.seen {
		switch {
		case now.Sub(o.at) < l.every:
			active++
		case other != key && (o.skipped == 0 || len(l.seen) > 2*limit):
			// Nothing to tell, or kept long enough: a key that comes
			// back after this starts its count again.
			delete(l.seen, other)
		}
	}
	if active >= limit {
		l.overCap++
		return logHeld{}, false
	}
	held := logHeld{overCap: l.overCap}
	if known {
		held.sameKey = k.skipped
		k.at, k.skipped = now, 0
	} else {
		l.seen[key] = &logKey{at: now}
	}
	l.overCap = 0
	return held, true
}

// writeExportResponse answers with an ExportTraceServiceResponse, carrying
// partial_success when spans were skipped (spec 002 #13), in the encoding the
// request arrived in (spec 019 #7): a client that sent JSON gets JSON, so the
// one response shape it can parse is the one it gets.
func writeExportResponse(w http.ResponseWriter, result *mapping.Result, asJSON bool) {
	var (
		skipped int64
		reason  string
	)
	if result != nil {
		skipped, reason = result.Skipped, result.SkipReason
	}
	if asJSON {
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(http.StatusOK)
		w.Write(mapping.EncodeExportResponseJSON(skipped, reason))
		return
	}
	var body []byte
	if result != nil {
		body = mapping.EncodeExportResponse(skipped, reason)
	}
	w.Header().Set("Content-Type", contentTypeProtobuf)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}
