package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

/*
The body budget (spec 043 #13). The body cap bounds one request's body, and
handler concurrency is unbounded, so N requests at once held N bodies and
their decoded, mapped and re-encoded copies. Every body read into memory now
reserves its bytes from one budget, `TRACEPAD_BODY_BUDGET_BYTES`:

 1. The reservation is made in steps of bodyStep as the body is read, counting
    what reaches the reader — the decompressed bytes of a gzip body, whose size
    nothing declares until it is inflated.
 2. A request whose next step does not fit fails at once: `429` with
    `Retry-After`, the status every OTLP exporter retries. Nothing waits, so
    nothing can deadlock on half a body.
 3. The reservation is held until the handler returns, because the decoded
    export lives that long — through its wait for the writer, which is the
    backpressure.

The public routes' few-KiB bodies (spec 028 #26) are not counted: a caller
nobody has identified must not be able to spend what ingest needs.
*/

// bodyStep is the unit a body's reservation grows by.
const bodyStep = 64 << 10

// bodyBudget is the bytes of request bodies the server holds at once.
type bodyBudget struct {
	mu       sync.Mutex
	held     int64
	capacity int64
}

// reserve takes n bytes if they fit.
func (b *bodyBudget) reserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.held+n > b.capacity {
		return false
	}
	b.held += n
	return true
}

func (b *bodyBudget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held -= n
}

// heldBytes and capacityBytes are the gauge `GET /api/v1/system` reports (#21).
func (b *bodyBudget) heldBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held
}

func (b *bodyBudget) capacityBytes() int64 { return b.capacity }

// bodyHold is one request's reservation.
type bodyHold struct {
	budget   *bodyBudget
	reserved int64
}

// cover grows the reservation to cover `read` bytes, a step at a time, and
// reports whether it could. No step goes past `limit`, the most the body may
// be: a body at its cap holds exactly the cap, so a budget of one cap admits
// it on an idle server whatever the cap is a multiple of.
func (h *bodyHold) cover(read, limit int64) bool {
	read = min(read, limit)
	for h.reserved < read {
		step := min(bodyStep, limit-h.reserved)
		if !h.budget.reserve(step) {
			return false
		}
		h.reserved += step
	}
	return true
}

func (h *bodyHold) releaseAll() {
	h.budget.release(h.reserved)
	h.reserved = 0
}

type bodyHoldKey struct{}

// holdBodies gives a route's requests a reservation that is released when the
// handler returns (spec 043 #13). Which routes, bodyBudgetedOf says beside the
// route table.
func (s *Server) holdBodies(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hold := &bodyHold{budget: s.bodies}
		defer hold.releaseAll()
		next(w, r.WithContext(context.WithValue(r.Context(), bodyHoldKey{}, hold)))
	}
}

// holdFrom is the request's reservation, nil for a route the budget does not
// count.
func holdFrom(ctx context.Context) *bodyHold {
	hold, _ := ctx.Value(bodyHoldKey{}).(*bodyHold)
	return hold
}

// errBodyBudget is a body whose next step did not fit the budget.
var errBodyBudget = errors.New("the body budget is spent")

// bodyBusy is the budget's answer (spec 043 #13).
const bodyBusy = "the server is holding as many request bodies as it can; retry shortly"

// budgeted reads through a reservation: at most one step at a time, each
// covered once it has been read, so a refused body has read at most one step
// past what fit. `limit` is the most the body may be; what the reader hands on
// past it is about to be refused as too large, and is not reserved. A reader
// for a request with no reservation is returned as it is.
func budgeted(reader io.Reader, hold *bodyHold, limit int64) io.Reader {
	if hold == nil {
		return reader
	}
	return &budgetReader{reader: reader, hold: hold, limit: limit}
}

type budgetReader struct {
	reader io.Reader
	hold   *bodyHold
	limit  int64
	read   int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if len(p) > bodyStep {
		p = p[:bodyStep]
	}
	n, err := b.reader.Read(p)
	b.read += int64(n)
	if err != nil && err != io.EOF {
		// A body over its cap, a broken stream: its own answer.
		return n, err
	}
	if !b.hold.cover(b.read, b.limit) {
		return n, errBodyBudget
	}
	return n, err
}
