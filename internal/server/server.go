// Package server owns the HTTP surface: routing, lifecycle, shutdown.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Server is the Tracepad HTTP server.
type Server struct {
	http    *http.Server
	store   *store.Store
	version string
}

// New builds the server around an opened store.
func New(listen, version string, st *store.Store) *Server {
	s := &Server{store: st, version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	s.http = &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
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

// Shutdown drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": s.version})
}
