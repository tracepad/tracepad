package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/client"
	"github.com/tracepad/tracepad/internal/mcpserver"
)

// The transport's own defences (spec 001 Decisions 14–16): what every response
// says about how it may be used, how long one may take to write, and how a stop
// waits for the requests it interrupts. None of it knows about routes; the one
// credential it asks about is the MCP stream's, because that is the one
// response it lets run without a deadline.

// securityHeaders are sent on every response (spec 001 #14).
//
//   - The interface must never be drawn inside another page's frame: a
//     session cookie rides along into the frame, and a page on another port
//     of the same host is "same-site" to that cookie, so a click on what
//     looks like the other page can land on a Delete button here. Both
//     spellings, because `X-Frame-Options` is what an older browser reads
//     and `frame-ancestors` is what a current one does.
//   - `nosniff`, so a response is only ever what its Content-Type says.
//   - `same-origin` referrers: this server's own requests keep the full
//     `Referer` that the cross-site check falls back to (spec 028 Decision
//     5), and a link out of the interface tells the other site nothing —
//     not even the name of the host a self-hosted deployment runs on.
//
// A full `script-src` policy is the next step and not this one: it has to be
// written against what the interface's bundle actually loads.
var securityHeaders = [...][2]string{
	{"Content-Security-Policy", "frame-ancestors 'none'"},
	{"X-Frame-Options", "DENY"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "same-origin"},
}

// withResponseHeaders sets the headers every response carries before the
// handler runs: this build's version (Decision 28 — a client that has to spend
// a round trip on `/api/v1/system` to notice version skew will not spend it)
// and securityHeaders. Set first, so a handler with a policy of its own — a
// media body's sandbox — replaces a header rather than adding a second one.
// The version header's name lives in the client package because that is who
// reads it; the server is the only writer.
func (s *Server) withResponseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set(client.VersionHeader, s.version)
		for _, pair := range securityHeaders {
			header.Set(pair[0], pair[1])
		}
		next.ServeHTTP(w, r)
	})
}

// writeTimeout bounds how long one response may take to write, from the end
// of the request headers (spec 001 #15). A client that reads a response one
// byte a second holds a connection for as long as the server lets it; five
// minutes is what the largest ordinary response — one 20 MiB body, a media
// file or a raw batch — needs over a link of about half a megabit, and no
// longer.
const writeTimeout = 5 * time.Minute

// mcpStream serves MCP over streamable HTTP, the one response that is a stream
// rather than a document: its answer takes as long as the tool it runs, so it
// has no write deadline. It is a route of the guard's table with the policy
// `stream` and the scope `read` (spec 045 #13), so the guard decides who may
// open one exactly as it decides every other route: a project key whose
// project is live and which may read, because every MCP tool is a read of the
// API (spec 004 #20). Anyone else is refused under the ordinary deadline and so
// cannot hold a connection open by not reading — no key, the admin token,
// which reaches no data-plane route (spec 028 #3), and a soft-deleted
// project's key with `401` and a Bearer challenge; a key without `read` with
// the guard's `403` and `insufficient_scope`. A store that cannot answer is
// guardLookup's 503 with Retry-After and no challenge: the key may be fine.
//
// The tools reach the read API through the router in this process
// (mcpserver.Loopback), as requests of their own carrying this one's context,
// and the guard takes the caller it admitted here from that context instead of
// looking the key up again on each tool call (loopbackCaller).
//
// The read deadline needs no lifting: net/http clears it once the request body
// has been read, so a tool call that outlasts ReadTimeout is not cancelled
// (asserted in TestOnlyAKeyedStreamOutlivesTheWriteDeadline), and lifting it
// here, before the body is read, would give up the bound on a slow body.
func (s *Server) mcpStream(next http.Handler) http.Handler {
	return s.guard(s.mcpRoute(next))
}

// mcpRoute is the stream's row of the table. No method: the transport answers
// the ones it serves and refuses the rest itself, after the guard.
func (s *Server) mcpRoute(next http.Handler) route {
	return route{"", mcpserver.Path, stream, scopeRead,
		"MCP over streamable HTTP", func(w http.ResponseWriter, r *http.Request) {
			// A writer that cannot take a deadline has none to lift.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
			// A stop ends the stream at once rather than after the drain
			// window: the client reconnects to whatever serves next.
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			defer context.AfterFunc(s.stopping, cancel)()
			next.ServeHTTP(w, r.WithContext(ctx))
		}}
}

// defaultHandlerGrace is how long a stop waits for handlers to return once
// their connections have been closed (spec 001 #16): a handler waiting on a
// commit finishes in well under a second, and one that has not in three is
// stuck. With the five-second drain it leaves two of the ten seconds `docker
// stop` gives before it kills for the closes that follow.
const defaultHandlerGrace = 3 * time.Second

// errHandlersStuck is a stop that could not wait for every handler. The
// process still exits, but not cleanly, and a supervisor should see that.
var errHandlersStuck = errors.New("handlers were still running after the stop's grace period")

// inflight counts the handlers running now, so a stop can wait for them after
// it has closed their connections. idle exists only while a wait is waiting:
// wait makes it, and the handler that brings the count to zero closes it, so
// every other request only counts.
//
// closed is set when a stop starts waiting, and from then on no handler
// starts: closing a connection does not stop a goroutine that has already
// read a request from it, and one that reached the handler after wait saw
// zero would meet the writer its owner is about to close. It gets 503
// instead. Checked and set under the same lock as the count, so there is no
// moment between the two.
type inflight struct {
	mu      sync.Mutex
	running int
	idle    chan struct{}
	closed  bool
}

func (f *inflight) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			writeError(w, http.StatusServiceUnavailable, "the server is stopping")
			return
		}
		f.running++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.running--
			if f.running == 0 && f.idle != nil {
				close(f.idle)
				f.idle = nil
			}
			f.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// count is how many handlers are running now.
func (f *inflight) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

// wait closes the door to new handlers, then returns once no handler is
// running, or reports that some still are when ctx ends.
func (f *inflight) wait(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	if f.running == 0 {
		f.mu.Unlock()
		return nil
	}
	if f.idle == nil {
		f.idle = make(chan struct{})
	}
	idle := f.idle
	f.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return errHandlersStuck
	}
}
