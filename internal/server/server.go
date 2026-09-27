// Package server owns the HTTP surface: routing, lifecycle, shutdown.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/logpace"
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
	// ExpectedBy is when a compaction requested now will have run
	// (spec 044 #11).
	ExpectedBy() int64
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
	// maxSpans is the most spans one export may carry (spec 043 #10), and
	// bodies the budget every request body is read from (#13).
	maxSpans int
	bodies   *bodyBudget
	// responseBudget is the default byte budget a read spends on payloads
	// (spec 004 #2); `?budget=` overrides it per request.
	responseBudget int64
	// readTimeout is the deadline of one read and reads its slots
	// (spec 043 #15, #16): the read gate's two bounds.
	readTimeout time.Duration
	reads       readSlots
	// systemReads is the one slot `GET /api/v1/system` reads in: a lane
	// of its own, so the gauge answers while every read slot is taken
	// and its counts cannot crowd out the rest (spec 043 #27).
	systemReads readSlots
	// mcp reports whether /mcp is being served, which `GET /api/v1/system`
	// publishes because the endpoint map deliberately does not (Decision
	// 27).
	mcp bool
	// adminToken authenticates everything cross-project (spec 005 #11).
	// Empty means no such credential exists on this deployment, and the
	// endpoints that need one say so rather than 401ing.
	adminToken string

	// Accounts (spec 028). sessionLife is how long a browser session lasts
	// and how far each slide moves it (Decision 4); configured is the
	// operator's `TRACEPAD_URL`, read once (setPublicURL), which wins over a
	// guessed host in a printed setup or invite link (Decision 11) and is
	// one of the server's own origins (Decision 23). Nil when unset or
	// unreadable.
	sessionLife time.Duration
	configured  *publicAddress
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
	// setupExpires is when setupToken stops opening anything (Decision
	// 32), and setupOff is TRACEPAD_SETUP=off: no token is minted at all.
	setupExpires time.Time
	setupOff     bool
	limiter      *loginLimiter
	// passwordChanges counts wrong current passwords per account, apart
	// from the login's count by email (spec 028 #31).
	passwordChanges *loginLimiter
	// passwords bounds the bcrypt work in flight, and passwordLog paces
	// the warning for what it turns away (spec 028 #31).
	passwords   *store.PasswordGate
	passwordLog *logpace.Keyed
	// running counts the handlers in flight, and handlerGrace is how long
	// a stop waits for them once their connections are closed (spec 001
	// #16).
	running      inflight
	handlerGrace time.Duration
	// stopping ends when a stop begins, and with it every MCP stream
	// (mcpStream): a client holding its event stream open would otherwise
	// keep the drain waiting for its whole window. Nothing else is tied to
	// it, so an ingest waiting on its commit is left to finish (spec 001 #16).
	stopping    context.Context
	stopStreams context.CancelFunc
	// inflatedLog paces the warning for a gzip body refused after
	// decompression (spec 002 #27).
	inflatedLog *logpace.Keyed
	// originLog paces the warning for a browser request refused for its
	// origin (spec 028 #30), per origin, so that a page elsewhere posting
	// once a minute cannot hide the line about the operator's own proxy.
	originLog *logpace.Keyed
	// cutLog paces the warning for a response the write deadline cut
	// short, per route (spec 001 #19).
	cutLog *logpace.Keyed

	// The web interface (spec 006): the built bundle, nil in a build
	// without the `ui` tag; the path segments the API owns, so a mistyped
	// endpoint never resolves to a web page; and the matcher that
	// reconstructs the 405 the catch-all would otherwise swallow.
	assets   fs.FS
	reserved map[string]bool
	paths    pathMatcher

	startedAt time.Time
	counters  *counters
	// mediaKey signs the Langfuse channel's upload URLs (spec 041,
	// Decisions 14 and 22), kept in the database across restarts.
	mediaKey []byte

	// keyUses is when each key last authenticated a request, held until
	// the next flush writes it (spec 045 #9); the rest is the flusher's
	// lifetime, which is serving's (keyuse.go).
	keyUses     *keyUses
	keyUseEvery time.Duration
	flusherOnce sync.Once
	flusherStop context.CancelFunc
	flusherDone chan struct{}
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
	maxSpans := cfg.MaxSpansPerRequest
	if maxSpans <= 0 {
		maxSpans = config.DefaultMaxSpansPerRequest
	}
	// Never below the cap, which config.Load refuses: a budget smaller
	// than one body would refuse a body the cap admits on an idle server.
	bodyBudgetBytes := cfg.BodyBudgetBytes
	if bodyBudgetBytes <= 0 {
		bodyBudgetBytes = config.DefaultBodyBudgetBytes(maxBody)
	}
	bodyBudgetBytes = max(bodyBudgetBytes, maxBody)
	budget := cfg.ResponseBudgetBytes
	if budget <= 0 {
		budget = config.DefaultResponseBudgetBytes
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = config.DefaultReadTimeout
	}
	readConcurrency := cfg.ReadConcurrency
	if readConcurrency <= 0 {
		readConcurrency = config.DefaultReadConcurrency(runtime.GOMAXPROCS(0))
	}
	sessionLife := cfg.SessionLife
	if sessionLife <= 0 {
		sessionLife = config.DefaultSessionLife
	}
	s := &Server{
		store:           st,
		writer:          writer,
		sweeper:         sweeper,
		version:         version,
		storeRaw:        cfg.StoreRaw,
		maxBodyBytes:    maxBody,
		maxSpans:        maxSpans,
		bodies:          &bodyBudget{capacity: bodyBudgetBytes},
		responseBudget:  budget,
		readTimeout:     readTimeout,
		reads:           newReadSlots(readConcurrency),
		systemReads:     newReadSlots(1),
		mcp:             cfg.MCP,
		adminToken:      cfg.AdminToken,
		sessionLife:     sessionLife,
		setupOff:        cfg.SetupDisabled,
		limiter:         newLoginLimiter(),
		passwordChanges: newLoginLimiter(),
		passwords:       newPasswordGate(),
		passwordLog:     &logpace.Keyed{Every: time.Minute},
		handlerGrace:    defaultHandlerGrace,
		inflatedLog:     &logpace.Keyed{Every: time.Minute},
		originLog:       &logpace.Keyed{Every: time.Minute, Keys: 64},
		cutLog:          &logpace.Keyed{Every: time.Minute},
		assets:          ui.Assets(),
		startedAt:       time.Now(),
		counters:        newCounters(),
		keyUses:         newKeyUses(),
		keyUseEvery:     keyUseFlushEvery,
	}
	s.setPublicURL(cfg.URL)
	if st != nil {
		s.mediaKey = st.MediaUploadKey()
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
		// Every read but the public ones runs under the read deadline and
		// in a read slot (spec 043 #15, #16), which the guard enters once
		// it knows who is asking; routes.go says which are which.
		switch readBoundOf(route) {
		case sharedSlots:
			route.handler = s.readGate(route.handler, s.reads)
		case ownLane:
			route.handler = s.readGate(route.handler, s.systemReads)
		}
		// Every body read into memory but a public route's counts against
		// one budget (spec 043 #13); routes.go says which.
		if bodyBudgetedOf(route) {
			route.handler = s.holdBodies(route.handler)
		}
		// The policy column is applied here, once, rather than by each
		// handler asking for its own credentials (spec 028 Decision 7).
		mux.HandleFunc(route.Method+" "+route.Path, s.guard(route))
	}
	if s.mcp {
		// Guarded like every route, but not one of routes(): /mcp is a
		// JSON-RPC transport rather than an endpoint of this API, so it
		// belongs in neither the endpoint map nor the OpenAPI document
		// (Decision 27). Its tools reach the read API through this
		// same mux — one implementation of budgets, truncation, auth
		// and JSON shape (spec 004 #16).
		mux.Handle(mcpserver.Path, s.mcpStream(mcpserver.HTTPHandler(version, &mcpserver.Loopback{Handler: mux})))
	}
	// The web interface, also outside the table (spec 006): a catch-all
	// that serves the SPA and its assets, and hands anything under an API
	// prefix the JSON 404 or 405 the mux can no longer produce for itself.
	mux.HandleFunc("/", s.handleUI)

	s.http = &http.Server{
		Addr: cfg.Listen,
		// The headers outermost, so that even the 503 a stop answers
		// once it has begun waiting carries them (spec 001 #14).
		Handler:           s.withResponseHeaders(s.running.track(s.reportCutResponses(mux))),
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds slow-dripping request bodies; IdleTimeout
		// reaps abandoned keep-alives; WriteTimeout bounds a slow reader
		// (spec 001 #15). The one route that streams lifts its own
		// deadline, and only for a project key (`mcpStream`), so the
		// bound is for documents and for anybody without one.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: writeTimeout,
		IdleTimeout:  120 * time.Second,
	}
	s.stopping, s.stopStreams = context.WithCancel(context.Background())
	s.http.RegisterOnShutdown(s.stopStreams)
	return s
}

// Handler exposes the routing for tests.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// listenAddr is the address to bind for a configured one. An empty address
// is `:http`, as http.Server.ListenAndServe has it; net.Listen alone would
// read it as "any free port".
func listenAddr(configured string) string {
	if configured == "" {
		return ":http"
	}
	return configured
}

// ListenAndServe blocks until the server stops. It binds the listener itself
// so that the log names the address actually bound — the port `:0` chose, the
// address a host name resolved to — rather than the one configured.
func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp", listenAddr(s.http.Addr))
	if err != nil {
		return err
	}
	slog.Info("listening", "addr", listener.Addr().String())
	s.startKeyUseFlusher()
	err = s.http.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown drains in-flight requests, then writes the key uses they made
// (spec 045 #9). The ingest writer outlives it by design: a handler still
// waiting on a commit must get its answer before the writer is closed by its
// owner, and so must the last flush.
//
// A connection still open when ctx ends — a response a slow client has not
// finished reading — is closed rather than waited for. Closing a connection
// does not stop its handler, though, and the caller closes the writer next, so
// Shutdown then waits up to handlerGrace for every handler to return (spec 001
// #16), on every path out, whatever else failed. A stop whose handlers all
// returned is a clean one, however it got there; one that still has a handler
// running reports errHandlersStuck, so the process exits non-zero and a
// supervisor sees that it did not stop cleanly. MCP streams end when the stop
// begins (stopping), rather than holding the drain for its whole window.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		slog.Warn("connections still open after the drain window were closed")
		err = s.http.Close()
	}
	// Whatever went wrong above, the handlers are waited for: the caller
	// closes the writer next on every path out of here.
	grace, cancel := context.WithTimeout(context.Background(), s.handlerGrace)
	defer cancel()
	if waitErr := s.running.wait(grace); waitErr != nil {
		slog.Warn("handlers still running after the stop's grace period; stopping without them",
			"running", s.running.count())
		err = errors.Join(err, waitErr)
	}
	// After the handlers, which are what touch keys; before the owner
	// closes the writer the flush is a job on.
	s.stopKeyUseFlusher()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), keyUseFlushTime(grace))
	defer flushCancel()
	s.flushKeyUses(flushCtx)
	return err
}

// keyUseFlushTime is how long a stop gives its last write of key uses: at
// most a second, which one small job never needs, and only what is left of
// the handlers' grace, so the stop stays inside its budget (spec 001 #16) —
// but never under a quarter of a second, since the uses the drained requests
// made still deserve their one write.
func keyUseFlushTime(grace context.Context) time.Duration {
	left := time.Second
	if deadline, ok := grace.Deadline(); ok {
		left = min(left, time.Until(deadline))
	}
	return max(left, 250*time.Millisecond)
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
