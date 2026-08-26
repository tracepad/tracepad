package server

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/store"
)

// OTLP/HTTP trace ingest (spec 002). Transport is protobuf only (#1): both
// SDK families we target speak it, and adding JSON or gRPC would widen the
// surface for no known consumer.

// contentTypeProtobuf is what the OTLP spec prescribes. `application/protobuf`
// is accepted too — some exporters send it, and refusing a body we can decode
// only teaches users to distrust the endpoint.
const (
	contentTypeProtobuf    = "application/x-protobuf"
	contentTypeProtobufAlt = "application/protobuf"
)

// maxDecompressionRatio bounds a gzipped body's expansion. The configured cap
// applies to what arrives on the wire; without a second bound a small
// compressed body could still exhaust memory.
const maxDecompressionRatio = 20

// handleTraces serves both the canonical OTLP route and the Langfuse-SDK
// alias. They are the same endpoint: one key pair serves both wire formats,
// so a client that guessed the wrong path still works (spec 002 #2).
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "ingest is not available")
		return
	}

	project, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if !acceptableContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType,
			"expected Content-Type "+contentTypeProtobuf)
		return
	}

	// Early drift signal, never enforcement: refusing an unknown SDK
	// version would break users on newer SDKs for nothing, and the raw
	// body means we can always catch up retroactively (spec 002 #17).
	// The counter half of #17 arrives with the system endpoint that would
	// expose it (spec 004); until then the log is the signal.
	if v := r.Header.Get("x-langfuse-ingestion-version"); v != "" {
		slog.Info("langfuse ingestion version", "version", v, "project", project.Name)
	}

	encoding := r.Header.Get("Content-Encoding")
	body, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot read request body")
		return
	}

	resourceSpans, err := mapping.DecodeExportRequest(body)
	if err != nil {
		slog.Warn("undecodable OTLP body", "project", project.Name, "err", err)
		writeError(w, http.StatusBadRequest, "malformed OTLP body")
		return
	}
	if len(resourceSpans) == 0 {
		// Per the OTLP spec an empty batch is a successful no-op; there
		// is nothing to store and nothing to replay later.
		writeExportResponse(w, nil)
		return
	}

	result := mapping.Map(resourceSpans)
	batch := &store.IngestBatch{
		ProjectID:    project.ID,
		Traces:       result.Traces,
		Observations: result.Observations,
	}
	if s.storeRaw {
		batch.Raw = &store.RawBatch{
			ReceivedAt:      time.Now().UnixNano(),
			Dialect:         result.Dialect,
			ContentEncoding: encoding,
			Body:            body,
		}
	}

	if err := s.writer.Submit(r.Context(), batch); err != nil {
		switch {
		case errors.Is(err, store.ErrWriterBusy):
			// Backpressure the exporter can act on: OTLP clients
			// retry 429 with backoff natively (spec 002 #15).
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "ingest queue is full, retry shortly")
		case errors.Is(err, store.ErrWriterClosed):
			writeError(w, http.StatusServiceUnavailable, "server is shutting down")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The client hung up; there is nobody to answer.
		default:
			slog.Error("ingest write failed", "project", project.Name, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to store spans")
		}
		return
	}

	// 200 only now: the transaction is committed and fsynced, so this
	// answer means the spans are on disk (spec 002 #15).
	writeExportResponse(w, result)
}

// authenticate resolves the request's credentials to a project. Both schemes
// are accepted on both routes and both resolve through one sha256 lookup
// (spec 002 #2).
func (s *Server) authenticate(r *http.Request) (*store.Project, bool) {
	secret, ok := credential(r.Header.Get("Authorization"))
	if !ok {
		return nil, false
	}
	project, err := s.store.ProjectBySecret(secret)
	if err != nil {
		slog.Error("key lookup failed", "err", err)
		return nil, false
	}
	return project, project != nil
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

func acceptableContentType(header string) bool {
	if header == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return mediaType == contentTypeProtobuf || mediaType == contentTypeProtobufAlt
}

// readBody reads the request body under the configured cap, transparently
// decompressing gzip (spec 002 API contract).
func readBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	if !strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		return io.ReadAll(limited)
	}

	gz, err := gzip.NewReader(limited)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	max := maxBytes * maxDecompressionRatio
	body, err := io.ReadAll(io.LimitReader(gz, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, &http.MaxBytesError{Limit: max}
	}
	return body, nil
}

// writeExportResponse answers with an ExportTraceServiceResponse, carrying
// partial_success when spans were skipped (spec 002 #13).
func writeExportResponse(w http.ResponseWriter, result *mapping.Result) {
	var body []byte
	if result != nil {
		body = mapping.EncodeExportResponse(result.Skipped, result.SkipReason)
	}
	w.Header().Set("Content-Type", contentTypeProtobuf)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}
