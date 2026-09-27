package server

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

/*
The read gate (spec 043 #15, #16). Tracepad is one process with one connection
pool and one heap, and a read had no deadline and no limit: a client that hung
up left its query running, a filter that scans every row ran as long as it
took, and any number of them ran at once.

Every `GET` route of the table except the public ones goes through here, after
the guard has resolved who is asking — so a credential lookup never waits for a
slot — and before its handler runs:

 1. The request gets a context with the read deadline, `TRACEPAD_READ_TIMEOUT`.
    Every store read runs under it, and the driver interrupts a statement
    whose context ends.
 2. It waits for one of `TRACEPAD_READ_CONCURRENCY` slots, within the same
    deadline: one number bounds the whole read. A request that gets none is
    `503` "busy" with `Retry-After`, because the next attempt may find one.
 3. The handler runs, and the slot is given back the moment the response's
    status is written — after the store work and the rendering, which
    writeJSON does before the status (spec 043 #3), and before the client's
    download, so a slow reader of a large body holds a socket and not a slot
    — or when the handler returns without writing one.
 4. A read the deadline stopped is `503` "narrow it" without `Retry-After`,
    because the same request would be stopped again. Whatever the handler made
    of its failed query — nothing, or a `5xx` — the gate answers instead; a
    request whose client hung up is answered by nobody.

Ingest, the Langfuse media upload and the writes take no slot: their cost is
bounded by the body cap and the writer's queue, and a flood of reads must not
turn into lost spans.
*/

// readSlots is the semaphore of spec 043 #16: a buffered channel whose length
// is the reads in flight.
type readSlots chan struct{}

func newReadSlots(capacity int) readSlots {
	return make(readSlots, capacity)
}

// ReadConcurrency is how many reads this server serves at once, the number
// the store's pool is sized by (spec 043 #16).
func (s *Server) ReadConcurrency() int { return s.reads.capacity() }

// busy and capacity are the gauge `GET /api/v1/system` reports (#21).
func (r readSlots) busy() int     { return len(r) }
func (r readSlots) capacity() int { return cap(r) }

// The two answers of the gate, beside the one in the handlers.
const readBusy = "the server is busy; retry shortly"

// readStopped is the deadline's answer; the number is the setting.
func readStopped(timeout time.Duration) string {
	return fmt.Sprintf("the read took longer than %s and was stopped; narrow the time range or the filters", timeout)
}

// readGate wraps one read route's handler, reading in the given slots; the
// guard runs it once the caller is resolved.
func (s *Server) readGate(next http.HandlerFunc, slots readSlots) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), s.readTimeout)
		defer cancel()
		asked := time.Now()
		if !s.takeSlot(w, r, ctx, slots) {
			return
		}
		admitted := time.Now()
		gated := &gatedWriter{ResponseWriter: w, ctx: ctx, slots: slots}
		defer gated.release()
		readAdmitted(ctx)
		next(gated, r.WithContext(ctx))
		gated.release()

		if hungUp(r) || ctx.Err() == nil || (gated.wrote && !gated.swallowed) {
			return
		}
		// The deadline stopped the read, and the handler either said
		// nothing or said `5xx` about a query the deadline interrupted.
		s.answerStopped(w, r, asked, admitted)
	}
}

// takeSlot waits for one of slots within the read's deadline, and reports
// whether it got one. A read that gets none is answered busy, with
// `Retry-After`, and counted; one whose client hung up while it waited is
// answered by nobody (spec 043 #16).
func (s *Server) takeSlot(w http.ResponseWriter, r *http.Request, ctx context.Context, slots readSlots) bool {
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		if !hungUp(r) {
			s.counters.observeRead(callerProject(r), readRefusedBusy)
			retryLater(w, readBusy)
		}
		return false
	}
}

// answerStopped answers a read the deadline stopped. One that spent longer
// waiting for its slot than running was slow because the server was busy, not
// because of what it asked, and is told so (spec 043, Edge cases); the other
// is told to narrow the request.
func (s *Server) answerStopped(w http.ResponseWriter, r *http.Request, asked, admitted time.Time) {
	header := w.Header()
	// Whatever the handler set for an answer it did not give: caching
	// headers a failure must not keep, and a retry hint only the busy
	// answer carries.
	header.Del("Retry-After")
	header.Del("ETag")
	header.Del("Last-Modified")
	header.Set("Cache-Control", callerCacheControl)
	if admitted.Sub(asked) > time.Since(admitted) {
		s.counters.observeRead(callerProject(r), readRefusedBusy)
		retryLater(w, readBusy)
		return
	}
	s.counters.observeRead(callerProject(r), readTimedOut)
	writeError(w, http.StatusServiceUnavailable, readStopped(s.readTimeout))
}

/*
readInSlot runs one read a write route makes — a dry run's counts, the selection
a bulk act works through — as a read route runs (spec 043 #29): under the read
deadline and in one of the read slots, taken for the read alone, so the write
that may follow waits for nothing and holds nothing. A read of this kind scans
like a listing, and uncounted it could hold every connection of the bounded
pool for as long as its scan took (#16). It answers the client itself when the
read does not complete — busy, stopped, or the read's own failure, as message —
and reports whether it did.
*/
func (s *Server) readInSlot(w http.ResponseWriter, r *http.Request, message string,
	read func(ctx context.Context) error) bool {
	ctx, cancel := context.WithTimeout(r.Context(), s.readTimeout)
	defer cancel()
	asked := time.Now()
	if !s.takeSlot(w, r, ctx, s.reads) {
		return false
	}
	admitted := time.Now()
	err := func() error {
		// Given back however the read ends, a panic included: a slot
		// that is never returned is one fewer for every read after.
		defer func() { <-s.reads }()
		readAdmitted(ctx)
		return read(ctx)
	}()
	switch {
	case err == nil:
		return true
	case hungUp(r):
	case ctx.Err() != nil:
		s.answerStopped(w, r, asked, admitted)
	default:
		readFailed(w, r, message, err)
	}
	return false
}

// readAdmitted is told when a read has its slot, before its handler runs — a
// seam for the tests that hold slots.
var readAdmitted = func(context.Context) {}

// callerProject is the project a gated request is about, for its counters;
// empty for a caller the guard resolved to no project — an account's own
// routes, the admin token's listings.
func callerProject(r *http.Request) string {
	if c := callerFrom(r.Context()); c != nil && c.project != nil {
		return c.project.ID
	}
	return ""
}

// gatedWriter gives the read slot back when the status is written, and holds
// back a `5xx` the handler wrote after the deadline passed, for the gate to
// answer in its place.
type gatedWriter struct {
	http.ResponseWriter
	ctx context.Context
	// slots is where the request's slot goes back to, nil once it has.
	slots readSlots
	// wrote is whether a final status was written; swallowed whether it was
	// a 5xx held back.
	wrote, swallowed bool
}

// release gives the slot back, once.
func (g *gatedWriter) release() {
	if g.slots == nil {
		return
	}
	<-g.slots
	g.slots = nil
}

func (g *gatedWriter) WriteHeader(status int) {
	if status < http.StatusOK {
		// An informational status is not the answer.
		g.ResponseWriter.WriteHeader(status)
		return
	}
	if g.wrote {
		if !g.swallowed {
			g.ResponseWriter.WriteHeader(status)
		}
		return
	}
	g.wrote = true
	g.release()
	if status >= http.StatusInternalServerError && g.ctx.Err() != nil {
		g.swallowed = true
		return
	}
	g.ResponseWriter.WriteHeader(status)
}

func (g *gatedWriter) Write(body []byte) (int, error) {
	if !g.wrote {
		g.WriteHeader(http.StatusOK)
	}
	if g.swallowed {
		return len(body), nil
	}
	return g.ResponseWriter.Write(body)
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (g *gatedWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// readRefusal is which of the gate's two answers a read got, for the
// project's counters (spec 043 #21).
type readRefusal int

const (
	readRefusedBusy readRefusal = iota
	readTimedOut
)

// observeRead counts one read the gate refused or stopped, in the project it
// was about only; a read about no project is counted nowhere, since the
// counters are a project's and name nobody else's traffic (spec 004 #33).
func (c *counters) observeRead(projectID string, refusal readRefusal) {
	if projectID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	project := c.forProject(projectID)
	switch refusal {
	case readRefusedBusy:
		project.readsRefusedBusy++
	case readTimedOut:
		project.readsTimedOut++
	}
}
