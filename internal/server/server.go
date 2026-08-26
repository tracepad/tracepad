// Package server owns the HTTP surface: routing, lifecycle, shutdown.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// IngestWriter is the write side the ingest handler needs: hand over a
// mapped batch, get back the outcome of its commit. *store.Writer is the
// implementation; the interface keeps the handler testable against a writer
// that is saturated or failing on demand.
type IngestWriter interface {
	Submit(ctx context.Context, batch *store.IngestBatch) error
}

// Server is the Tracepad HTTP server.
type Server struct {
	http    *http.Server
	store   *store.Store
	writer  IngestWriter
	version string

	storeRaw     bool
	maxBodyBytes int64
}

// New builds the server around an opened store and the ingest writer that
// serializes writes into it. The writer is passed in rather than created
// here: its lifetime is owned by whoever opened the store, and tests tune its
// queue to exercise backpressure.
func New(cfg *config.Config, version string, st *store.Store, writer IngestWriter) *Server {
	s := &Server{
		store:        st,
		writer:       writer,
		version:      version,
		storeRaw:     cfg.StoreRaw,
		maxBodyBytes: cfg.MaxBodyBytes,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	// The canonical OTLP path and the Langfuse-SDK alias are one endpoint
	// (spec 002 #2).
	mux.HandleFunc("POST /v1/traces", s.handleTraces)
	mux.HandleFunc("POST /api/public/otel/v1/traces", s.handleTraces)

	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds slow-dripping request bodies; IdleTimeout
		// reaps abandoned keep-alives. WriteTimeout stays unset on
		// purpose: future endpoints stream (MCP over HTTP, trace
		// tailing) and a global write deadline would cut them off
		// mid-response.
		ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second,
	}
	return s
}

// Handler exposes the routing for tests.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// ListenAndServe blocks until the server stops.
func (s *Server) ListenAndServe() error {
	slog.Info("listening", "addr", s.http.Addr)
	err := s.http.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown drains in-flight requests. The ingest writer outlives it by
// design: a handler still waiting on a commit must get its answer before the
// writer is closed by its owner.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": s.version})
}

// writeError renders the one error shape the HTTP surface uses.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
