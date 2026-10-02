package server

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tracepad/tracepad/internal/logpace"
)

/*
How many connections the listener holds at once (spec 043 #45).

Every other bound in the server is per request — the body budget, the read
slots, the writer queue — and all of them are reached only once a request has
been read. A connection costs a goroutine and two buffers before that, so a
flood of connections that never send a request, or that send one and then sit
idle, was bounded by nothing but the process's file descriptors.

Two bounds, both on the TCP peer, since nothing else is known before a request
is read:

  - TRACEPAD_MAX_CONNECTIONS for the server. A connection that arrives past it
    is held, unserved, until a slot is free, and the ones behind it wait in the
    kernel's backlog rather than being refused, which a client sees as a slow
    connect and not as an error. While one is held, a connection is closed to
    make room: the keep-alive idle longest, or else one that has been silent —
    accepted, not a byte of a request read — for two seconds, at once or as
    soon as one is. Nothing is closed for room unless somebody has arrived to
    need it, and a connection serving a request never is.
  - TRACEPAD_MAX_CONNECTIONS_PER_SOURCE for one source, as the password limit
    reads a source (an IPv4 address, an IPv6 /64; spec 046 #4) but from the
    peer alone. A source at its bound makes room the same way among its own
    connections, and is refused — the connection closed — when it has none, so
    one source cannot take every slot and hold the rest of the world in the
    backlog. A trusted proxy (TRACEPAD_TRUSTED_PROXIES) is exempt: every client
    behind it arrives from its address.

A silent connection is also closed by ReadHeaderTimeout (spec 001 #15) ten
seconds after it arrives, so the bound is on how many arrive in that window.
*/

// silentGrace is how long a connection may have said nothing before the limit
// may close it for room. An exporter sends its request the moment it connects,
// so a connection silent this long is not one about to; well inside the ten
// seconds ReadHeaderTimeout gives it anyway.
const silentGrace = 2 * time.Second

// connLimit is the listener's bounds, shared by every listener the server
// binds: `localhost` is two, and one limit covers both.
type connLimit struct {
	slots     chan struct{}
	perSource int
	// exempt is a peer the per-source bound does not apply to: a trusted
	// proxy.
	exempt func(netip.Addr) bool
	// log paces the two warnings, keyed by which bound and which source:
	// a flood is exactly when the log would fill.
	log *logpace.Keyed
	// grace is silentGrace, shorter in tests.
	grace time.Duration

	// waiting counts the arrivals held for a slot. While it is zero a
	// connection going idle touches nothing but its own state.
	waiting atomic.Int32

	mu sync.Mutex
	// conns is every connection holding a slot.
	conns    map[*limitedConn]struct{}
	bySource map[netip.Prefix]int
	// roomMade is closed, and dropped, when a connection goes idle while an
	// arrival waits: every held arrival wakes and looks again. A channel per
	// generation rather than a token, because two listeners may each be
	// holding one. Made by the first arrival to wait on it.
	roomMade chan struct{}
}

func newConnLimit(n, perSource int, exempt func(netip.Addr) bool) *connLimit {
	if exempt == nil {
		exempt = func(netip.Addr) bool { return false }
	}
	return &connLimit{
		slots: make(chan struct{}, n), perSource: perSource, exempt: exempt,
		log: &logpace.Keyed{Every: time.Minute, Keys: 16}, grace: silentGrace,
		conns: map[*limitedConn]struct{}{}, bySource: map[netip.Prefix]int{},
	}
}

// listener wraps a listener so that each connection it accepts holds a slot
// until it is closed.
func (c *connLimit) listener(l net.Listener) net.Listener {
	return &limitedListener{Listener: l, limit: c, done: make(chan struct{})}
}

// track is the server's ConnState hook: it keeps each connection's state and
// since when, which is what the limit chooses by when it needs room. The state
// is stored before waiting is read, and an arrival counts itself waiting before
// it reads the states, so one of the two always sees the other.
func (c *connLimit) track(conn net.Conn, state http.ConnState) {
	limited, ok := conn.(*limitedConn)
	if !ok {
		return
	}
	limited.since.Store(time.Now().UnixNano())
	limited.state.Store(int32(state))
	if state != http.StateIdle || c.waiting.Load() == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roomMade != nil {
		close(c.roomMade)
		c.roomMade = nil
	}
}

// admitSource counts a connection against its source, making room among the
// source's own connections when it is at its bound. False is a source with
// nothing to give up: the caller refuses the connection.
func (c *connLimit) admitSource(conn *limitedConn) bool {
	if c.exempt(conn.peer) {
		return true
	}
	// Twice at most: a connection closed for room gives its count back as
	// it closes, and a second pass finds the room or a source that has
	// filled it again meanwhile.
	for range 2 {
		c.mu.Lock()
		if c.bySource[conn.source] < c.perSource {
			c.bySource[conn.source]++
			conn.counted = true
			c.mu.Unlock()
			return true
		}
		victim, _ := c.closable(&conn.source, time.Now())
		c.mu.Unlock()
		if victim == nil {
			break
		}
		victim.Close()
	}
	text := sourceText(conn.source)
	if held, ok := c.log.Allow("source "+text, time.Now()); ok {
		slog.Warn("a source holds as many connections as TRACEPAD_MAX_CONNECTIONS_PER_SOURCE allows, "+
			"none of them idle, so its next ones are refused",
			"source", text, "max_connections_per_source", c.perSource,
			"not_logged_since_last", held.SameKey, "not_logged_over_cap", held.OverCap)
	}
	return false
}

// acquire takes a slot for a connection that has arrived. With none free it
// closes a connection to make room, and then waits for a slot, looking again
// each time a connection goes idle or a silent one outlasts silentGrace. It
// gives up when done is closed, which is the listener closing.
func (c *connLimit) acquire(conn *limitedConn, done <-chan struct{}) bool {
	select {
	case c.slots <- struct{}{}:
		c.hold(conn)
		return true
	default:
	}
	c.waiting.Add(1)
	defer c.waiting.Add(-1)
	for {
		c.mu.Lock()
		if c.roomMade == nil {
			c.roomMade = make(chan struct{})
		}
		roomMade := c.roomMade
		victim, next := c.closable(nil, time.Now())
		c.mu.Unlock()
		if held, ok := c.log.Allow("full", time.Now()); ok {
			slog.Warn("the server holds as many connections as TRACEPAD_MAX_CONNECTIONS allows; "+
				"new ones wait, and idle or long-silent ones are closed to make room",
				"max_connections", cap(c.slots), "closed_one", victim != nil,
				"not_logged_since_last", held.SameKey)
		}
		if victim != nil {
			victim.Close()
		}
		var (
			timer       *time.Timer
			silentTurns <-chan time.Time
		)
		if victim == nil && !next.IsZero() {
			timer = time.NewTimer(time.Until(next))
			silentTurns = timer.C
		}
		got, closed := false, false
		select {
		case c.slots <- struct{}{}:
			got = true
		case <-roomMade:
		case <-silentTurns:
		case <-done:
			closed = true
		}
		if timer != nil {
			timer.Stop()
		}
		if got {
			c.hold(conn)
			return true
		}
		if closed {
			return false
		}
	}
}

func (c *connLimit) hold(conn *limitedConn) {
	conn.since.Store(time.Now().UnixNano())
	c.mu.Lock()
	defer c.mu.Unlock()
	conn.holding = true
	c.conns[conn] = struct{}{}
}

// closable is the connection to close for room, of one source or of any, and
// takes it out of the running so that two looks do not pick the same one: the
// keep-alive idle longest, or else the connection silent longest, once it has
// been silent silentGrace. A connection serving a request never is. With
// nothing to close, next is when the first silent one becomes closable, or
// zero. The caller holds mu, and closes the connection once it has let go of
// it.
func (c *connLimit) closable(source *netip.Prefix, now time.Time) (victim *limitedConn, next time.Time) {
	var idle, silent *limitedConn
	var idleSince, silentSince int64
	for conn := range c.conns {
		if conn.condemned || source != nil && conn.source != *source {
			continue
		}
		since := conn.since.Load()
		switch http.ConnState(conn.state.Load()) {
		case http.StateIdle:
			if idle == nil || since < idleSince {
				idle, idleSince = conn, since
			}
		case http.StateNew:
			if silent == nil || since < silentSince {
				silent, silentSince = conn, since
			}
		}
	}
	switch {
	case idle != nil:
		victim = idle
	case silent != nil && now.Sub(time.Unix(0, silentSince)) >= c.grace:
		victim = silent
	case silent != nil:
		return nil, time.Unix(0, silentSince).Add(c.grace)
	default:
		return nil, time.Time{}
	}
	victim.condemned = true
	return victim, time.Time{}
}

// release gives back what a connection held.
func (c *connLimit) release(conn *limitedConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn.counted {
		if c.bySource[conn.source]--; c.bySource[conn.source] <= 0 {
			delete(c.bySource, conn.source)
		}
	}
	if conn.holding {
		delete(c.conns, conn)
		<-c.slots
	}
}

type limitedListener struct {
	net.Listener
	limit     *connLimit
	done      chan struct{}
	closeOnce sync.Once
}

// Accept takes the connection first and the slot second, so that a connection
// is closed for room only for one that has actually arrived: net/http calls
// Accept again the moment it has one, and a slot taken before that would make
// room for nobody. A connection its source has no room for is closed here and
// the next one taken.
func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		peer := peerAddress(raw.RemoteAddr().String())
		conn := &limitedConn{Conn: raw, limit: l.limit, peer: peer, source: sourceOf(peer)}
		conn.state.Store(int32(http.StateNew))
		conn.since.Store(time.Now().UnixNano())
		if !l.limit.admitSource(conn) {
			raw.Close()
			continue
		}
		if !l.limit.acquire(conn, l.done) {
			conn.Close()
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

func (l *limitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitedConn gives back what it holds on the first Close; net/http may close
// a connection more than once. state and since are written by track without
// the limit's mutex; the flags are the limit's, under it.
type limitedConn struct {
	net.Conn
	limit     *connLimit
	peer      netip.Addr
	source    netip.Prefix
	closeOnce sync.Once

	state atomic.Int32 // an http.ConnState
	since atomic.Int64 // when it entered state, in Unix nanoseconds

	counted, holding, condemned bool
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() { c.limit.release(c) })
	return err
}

// CloseWrite passes the half-close through: net/http sends a FIN this way
// before closing a connection whose request body it did not read — an early
// 413 or 401 — so that the client reads the answer rather than a reset. A
// wrapper that hid it turned every such answer into a RST.
func (c *limitedConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return nil
}

// ReadFrom passes the connection's own through, so that a file served to it
// can still go out by sendfile.
func (c *limitedConn) ReadFrom(r io.Reader) (int64, error) {
	if from, ok := c.Conn.(io.ReaderFrom); ok {
		return from.ReadFrom(r)
	}
	return io.Copy(struct{ io.Writer }{c.Conn}, r)
}
