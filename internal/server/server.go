// Package server owns the HTTP surface: routing, lifecycle, shutdown.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/ui"
)

// JobWriter is the write side every handler that changes data needs: hand
// over a job, get back the outcome of its commit. *store.Writer is the
// implementation; the interface keeps the handlers testable against a writer
// that is saturated or failing on demand.
type JobWriter interface {
	Submit(ctx context.Context, job store.WriteJob) error
}

// SweepReporter is the retention sweeper as `GET /api/v1/system` needs it: a
// per-project view of what the passes have removed and when the next one runs
// (spec 005 #14). There is deliberately no method to trigger a pass — an
// immediate-sweep endpoint would be a destructive trigger with none of the
// dry-run semantics the rest of this spec insists on (Decision 14).
type SweepReporter interface {
	Status(projectID string) store.SweepStatus
}

// Server is the Tracepad HTTP server.
type Server struct {
	http    *http.Server
	store   *store.Store
	writer  JobWriter
	sweeper SweepReporter
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
	// adminToken authenticates everything cross-project (spec 005 #11).
	// Empty means no such credential exists on this deployment, and the
	// endpoints that need one say so rather than 401ing.
	adminToken string

	// Accounts (spec 028). sessionLife is how long a browser session lasts
	// and how far each slide moves it (Decision 4); publicURL is the
	// operator's `TRACEPAD_URL`, which wins over a guessed host in a
	// printed setup or invite link (Decision 11).
	sessionLife time.Duration
	publicURL   string
	// setupToken is minted at each start while this server has no owner who
	// can sign in, and lives in memory only: a token from yesterday's log
	// opens nothing today, and a restart is the recovery if the link was
	// lost (Decision 9). Empty once an owner exists.
	//
	// Behind a mutex because two browsers can hold the same link — a double
	// click is enough — and the handler both reads it and clears it. What
	// actually stops a second owner being created is the transaction
	// (`ErrSetupDone`); this is so that the read and the clearing are not a
	// data race.
	setupMu    sync.RWMutex
	setupToken string
	limiter    *loginLimiter

	// The web interface (spec 006): the built bundle, nil in a build
	// without the `ui` tag; the path segments the API owns, so a mistyped
	// endpoint never resolves to a web page; and the matcher that
	// reconstructs the 405 the catch-all would otherwise swallow.
	assets   fs.FS
	reserved map[string]bool
	paths    pathMatcher

	startedAt time.Time
	counters  *counters
}

// New builds the server around an opened store, the writer that serializes
// writes into it, and the sweeper it reports on. All three are passed in
// rather than created here: their lifetimes are owned by whoever opened the
// store, and tests tune the writer's queue to exercise backpressure and drive
// the sweeper by hand.
func New(cfg *config.Config, version string, st *store.Store, writer JobWriter, sweeper SweepReporter) *Server {
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
	sessionLife := cfg.SessionLife
	if sessionLife <= 0 {
		sessionLife = config.DefaultSessionLife
	}
	s := &Server{
		store:          st,
		writer:         writer,
		sweeper:        sweeper,
		version:        version,
		storeRaw:       cfg.StoreRaw,
		maxBodyBytes:   maxBody,
		responseBudget: budget,
		mcp:            cfg.MCP,
		adminToken:     cfg.AdminToken,
		sessionLife:    sessionLife,
		publicURL:      cfg.URL,
		limiter:        newLoginLimiter(),
		assets:         ui.Assets(),
		startedAt:      time.Now(),
		counters:       newCounters(),
	}
	// The setup token is minted here rather than on demand, once per start:
	// "while no owner exists" is a property of the server's lifetime, and a
	// token minted per request would be a token the printed link never
	// matched (Decision 9).
	s.mintSetupToken()
	s.reserved = reservedSegments(s.routes())
	s.paths = newPathMatcher(s.routes())

	// One table declares the whole surface (Decision 27); the mux is built
	// from it rather than beside it.
	mux := http.NewServeMux()
	for _, route := range s.routes() {
		// The policy column is applied here, once, rather than by each
		// handler asking for its own credentials (spec 028 Decision 7).
		mux.HandleFunc(route.Method+" "+route.Path, s.guard(route))
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
	// The web interface, also outside the table (spec 006): a catch-all
	// that serves the SPA and its assets, and hands anything under an API
	// prefix the JSON 404 or 405 the mux can no longer produce for itself.
	mux.HandleFunc("/", s.handleUI)

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
