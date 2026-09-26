package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Last use of a key (spec 045 Decision 9).
//
// A write per request would put a row update in front of every ingest batch
// and turn every read into a write, on the one path everything serialises
// through (spec 003 #9). So the guard records a use here, in memory, and a
// ticker hands the writer one job a minute for every key seen since the last
// one; `Shutdown` hands it a final one after the handlers have drained. A
// crash loses at most a minute of last-use, which the docs say.

// keyUseFlushEvery is how often the uses are written. A constant rather than a
// setting, like the session slide: the question it answers during a rotation —
// "has the old key gone quiet since I moved the SDKs?" — is about minutes.
const keyUseFlushEvery = time.Minute

// keyUses is public key → the latest time it authenticated a request, Unix
// nanoseconds.
type keyUses struct {
	mu sync.Mutex
	// seen is what the next flush writes.
	seen map[string]int64
	// flushing is what the flush in progress is writing, kept until the
	// writer has answered so that the listing still sees it meanwhile.
	flushing map[string]int64
}

func newKeyUses() *keyUses { return &keyUses{seen: map[string]int64{}} }

// touch records one use.
func (u *keyUses) touch(publicKey string, at int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if at > u.seen[publicKey] {
		u.seen[publicKey] = at
	}
}

// pending is the latest use not yet written, if any.
func (u *keyUses) pending(publicKey string) (int64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	at, ok := u.seen[publicKey]
	if flushed, inFlight := u.flushing[publicKey]; inFlight && flushed > at {
		return flushed, true
	}
	return at, ok
}

// take hands the uses seen so far to one flush. Nil when there is nothing to
// write, or while another flush is still out.
func (u *keyUses) take() map[string]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.seen) == 0 || u.flushing != nil {
		return nil
	}
	u.flushing, u.seen = u.seen, map[string]int64{}
	return u.flushing
}

// settle ends the flush in progress. A flush that failed puts its values back
// for the next one, where a newer use of the same key wins.
func (u *keyUses) settle(failed bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if failed {
		for publicKey, at := range u.flushing {
			if at > u.seen[publicKey] {
				u.seen[publicKey] = at
			}
		}
	}
	u.flushing = nil
}

// flushKeyUses writes the uses seen since the last flush as one job.
func (s *Server) flushKeyUses(ctx context.Context) {
	if s.writer == nil {
		return
	}
	seen := s.keyUses.take()
	if seen == nil {
		return
	}
	err := s.writer.Submit(ctx, &store.KeyUse{Seen: seen})
	if err != nil {
		// Late by a minute, never wrong: the next tick tries again.
		slog.Warn("could not record when keys were last used", "keys", len(seen), "err", err)
	}
	s.keyUses.settle(err != nil)
}

// startKeyUseFlusher runs the ticker until `Shutdown` stops it. It starts with
// serving rather than with `New`, so that a server built only for its handler
// — every test in this package — has no goroutine writing behind its back.
func (s *Server) startKeyUseFlusher() {
	s.flusherOnce.Do(func() {
		s.flusherStop = make(chan struct{})
		s.flusherDone = make(chan struct{})
		go func() {
			defer close(s.flusherDone)
			ticker := time.NewTicker(keyUseFlushEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.flushKeyUses(context.Background())
				case <-s.flusherStop:
					return
				}
			}
		}()
	})
}

// stopKeyUseFlusher stops the ticker, if it runs, and waits for a tick in
// progress to finish, so that the final flush is the last one.
func (s *Server) stopKeyUseFlusher() {
	s.flusherOnce.Do(func() {}) // a flusher that never started never will
	if s.flusherStop == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.flusherStop) })
	<-s.flusherDone
}
