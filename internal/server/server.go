// Package server owns the HTTP surface: routing, lifecycle, shutdown.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/store"
)

// JobWriter is the write side every handler that changes data needs: hand
// over a job, get back the outcome of its commit. *store.Writer is the
// implementation; the interface keeps the handlers testable against a writer
// that is saturated or failing on demand.
type JobWriter interface {
	Submit(ctx context.Context, job store.WriteJob) error
}

// Server is the Tracepad HTTP server.
type Server struct {
	http    *http.Server
	store   *store.Store
	writer  JobWriter
	version string

	storeRaw     bool
	maxBodyBytes int64
	// responseBudget is the default byte budget a read spends on payloads
	// (spec 004 #2); `?budget=` overrides it per request.
	responseBudget int64
	// mcp reports whether /mcp is being served, which `GET /api/v1/system`
	// publishes because the endpoint map deliberately does not (Decision
	// 27).
	mcp bool

	startedAt time.Time
	counters  *counters
}

// New builds the server around an opened store and the writer that
// serializes writes into it. The writer is passed in rather than created
// here: its lifetime is owned by whoever opened the store, and tests tune its
// queue to exercise backpressure.
func New(cfg *config.Config, version string, st *store.Store, writer JobWriter) *Server {
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		// config.Load never produces this, but a hand-built Config
		// would, and a zero cap silently 413s every export.
		maxBody = config.DefaultMaxBodyBytes
	}
	budget := cfg.ResponseBudgetBytes
	if budget <= 0 {
		budget = config.DefaultResponseBudgetBytes
	}
	s := &Server{
		store:          st,
		writer:         writer,
		version:        version,
		storeRaw:       cfg.StoreRaw,
		maxBodyBytes:   maxBody,
		responseBudget: budget,
		mcp:            cfg.MCP,
		startedAt:      time.Now(),
		counters:       newCounters(),
	}

	// One table declares the whole surface (Decision 27); the mux is built
	// from it rather than beside it.
	mux := http.NewServeMux()
	for _, route := range s.routes() {
		mux.HandleFunc(route.Method+" "+route.Path, route.handler)
	}
	if s.mcp {
		// Registered outside the table: /mcp is a JSON-RPC transport
		// rather than an endpoint of this API, so it belongs in
		// neither the endpoint map nor the OpenAPI document
		// (Decision 27). Its tools reach the read API through this
		// same mux — one implementation of budgets, truncation, auth
		// and JSON shape (spec 004 #16).
		mux.Handle(mcpserver.Path, mcpserver.HTTPHandler(version, &mcpserver.Loopback{Handler: mux}))
	}

	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.withVersion(mux),
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

// withVersion stamps this build on every response. A client that has to spend
// a round trip on `/api/v1/system` to notice version skew will not spend it,
// so the answer rides along with whatever it was already asking for
// (Decision 28). The header name lives in the client package because that is
// who reads it; the server is the only writer.
func (s *Server) withVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(client.VersionHeader, s.version)
		next.ServeHTTP(w, r)
	})
}
