package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// limitedServers serves handler behind a connection limit of n, as
// ListenAndServe does — on as many listeners as asked, sharing the one limit —
// and returns their addresses. Every test connection comes from 127.0.0.1, so
// the bound of one source is out of the way unless perSource sets it.
func limitedServers(t *testing.T, listeners, n, perSource int, handler http.Handler) []string {
	t.Helper()
	if perSource == 0 {
		perSource = math.MaxInt
	}
	limit := newConnLimit(n, perSource, nil)
	server := &http.Server{Handler: handler, ConnState: limit.track, ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(func() { server.Close() })
	addrs := make([]string, listeners)
	for i := range addrs {
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go server.Serve(limit.listener(raw))
		addrs[i] = raw.Addr().String()
	}
	return addrs
}

func limitedServer(t *testing.T, n int, handler http.Handler) string {
	return limitedServers(t, 1, n, 0, handler)[0]
}

// keptConn is a client connection that sends keep-alive requests by hand.
type keptConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialKept(t *testing.T, addr string) keptConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return keptConn{conn, bufio.NewReader(conn)}
}

// closedByServer reports whether the server has closed the connection.
func (k keptConn) closedByServer(t *testing.T) bool {
	t.Helper()
	k.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := k.reader.ReadByte()
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// request sends one keep-alive GET on conn and reads its answer, leaving the
// connection open and idle.
func request(t *testing.T, conn net.Conn, reader *bufio.Reader) error {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		return err
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return err
}

// A connection past the limit is not refused: the oldest idle keep-alive is
// closed to make room for it, and the connection that was idle longest is the
// one that goes (spec 043 #45).
func TestConnectionLimitClosesTheOldestIdleForANewOne(t *testing.T) {
	addr := limitedServer(t, 2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))

	type client struct {
		conn   net.Conn
		reader *bufio.Reader
	}
	dial := func() client {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return client{conn, bufio.NewReader(conn)}
	}
	first, second := dial(), dial()
	if err := request(t, first.conn, first.reader); err != nil {
		t.Fatalf("first: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := request(t, second.conn, second.reader); err != nil {
		t.Fatalf("second: %v", err)
	}

	third := dial()
	if err := request(t, third.conn, third.reader); err != nil {
		t.Fatalf("a connection past the limit was not served: %v", err)
	}
	// The first went idle first and was closed for the third; the second
	// is still served.
	first.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := first.reader.ReadByte(); err != io.EOF {
		t.Errorf("the oldest idle connection read %v, want it closed (EOF)", err)
	}
	if err := request(t, second.conn, second.reader); err != nil {
		t.Errorf("the newer idle connection was closed too: %v", err)
	}
}

// A connection serving a request is never closed for room: past the limit a
// new one waits until a slot is given back, and is then served.
func TestConnectionLimitWaitsForABusyOne(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	addr := limitedServer(t, 1, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		if r.URL.Path == "/slow" {
			<-release
		}
		io.WriteString(w, "ok")
	}))

	busy, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	fmt.Fprint(busy, "GET /slow HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	<-entered

	served := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Get("http://" + addr + "/")
		if err == nil {
			response.Body.Close()
		}
		served <- err
	}()
	select {
	case err := <-served:
		t.Fatalf("a connection past the limit was served while the only slot was busy (err = %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-served; err != nil {
		t.Errorf("the waiting connection was not served once the slot came back: %v", err)
	}
}

// Two listeners — `localhost` is 127.0.0.1 and ::1 — share one limit, and an
// arrival held on each is served once connections go idle: the wake is a
// broadcast, not a token one of them takes from the other.
func TestConnectionLimitWakesEveryHeldArrival(t *testing.T) {
	release := make(chan struct{})
	addrs := limitedServers(t, 2, 2, 0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
		io.WriteString(w, "ok")
	}))
	// Both slots busy, one on each listener.
	var busy sync.WaitGroup
	for _, addr := range addrs {
		k := dialKept(t, addr)
		busy.Add(1)
		go func() {
			defer busy.Done()
			fmt.Fprint(k.conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
			response, err := http.ReadResponse(k.reader, nil)
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	// One arrival held on each listener.
	served := make(chan error, 2)
	for _, addr := range addrs {
		go func() {
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
			response, err := client.Get("http://" + addr + "/")
			if err == nil {
				response.Body.Close()
			}
			served <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	// The busy two finish and stay open, idle: each is closed for one of
	// the held arrivals.
	close(release)
	busy.Wait()
	for range 2 {
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("a held arrival failed: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a held arrival was not served after the connections went idle")
		}
	}
}

// A keep-alive that has done its work goes before a connection that has said
// nothing yet: the silent one may be an exporter's POST about to arrive. A
// silent one is closed only once it has been silent for the grace, and an
// arrival that finds nothing else waits for that.
func TestConnectionLimitClosesIdleBeforeSilent(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	serve := func(n int, grace time.Duration) string {
		limit := newConnLimit(n, math.MaxInt, nil)
		limit.grace = grace
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: handler, ConnState: limit.track}
		go server.Serve(limit.listener(raw))
		t.Cleanup(func() { server.Close() })
		return raw.Addr().String()
	}

	// An idle keep-alive and a silent connection: the idle one goes.
	addr := serve(2, time.Hour)
	worker := dialKept(t, addr)
	if err := request(t, worker.conn, worker.reader); err != nil {
		t.Fatal(err)
	}
	silent := dialKept(t, addr)
	time.Sleep(50 * time.Millisecond)
	arrival := dialKept(t, addr)
	if err := request(t, arrival.conn, arrival.reader); err != nil {
		t.Fatalf("the arrival was not served: %v", err)
	}
	if !worker.closedByServer(t) {
		t.Error("the idle keep-alive was kept")
	}
	// The silent one was not closed: it can still send its request.
	if err := request(t, silent.conn, silent.reader); err != nil {
		t.Errorf("the silent connection was closed instead: %v", err)
	}

	// Only silent ones: the arrival waits out the grace, then the one
	// silent longest goes.
	const grace = 300 * time.Millisecond
	addr = serve(2, grace)
	first := dialKept(t, addr)
	time.Sleep(20 * time.Millisecond)
	second := dialKept(t, addr)
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	late := dialKept(t, addr)
	if err := request(t, late.conn, late.reader); err != nil {
		t.Fatalf("the arrival was not served once a silent one outlasted the grace: %v", err)
	}
	if waited := time.Since(start); waited < grace/2 {
		t.Errorf("served after %s: a silent connection inside its grace was closed", waited)
	}
	if !first.closedByServer(t) {
		t.Error("the connection silent longest was kept")
	}
	if err := request(t, second.conn, second.reader); err != nil {
		t.Errorf("the younger silent connection was closed too: %v", err)
	}
}

// One source at its bound makes room among its own idle connections, and is
// refused when every one of them is busy — while the server has room left.
// A source the exemption covers — a trusted proxy — has no bound of its own.
func TestConnectionLimitPerSource(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-release
		}
		io.WriteString(w, "ok")
	})
	addr := limitedServers(t, 1, 16, 2, handler)[0]

	idle := dialKept(t, addr)
	if err := request(t, idle.conn, idle.reader); err != nil {
		t.Fatal(err)
	}
	busy := dialKept(t, addr)
	fmt.Fprint(busy.conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
	time.Sleep(50 * time.Millisecond)

	// At its bound with one idle: the idle one makes room.
	third := dialKept(t, addr)
	if err := request(t, third.conn, third.reader); err != nil {
		t.Fatalf("the source's third connection was not served: %v", err)
	}
	if !idle.closedByServer(t) {
		t.Error("the source's idle connection was kept")
	}
	// Both busy now: the next is refused.
	fmt.Fprint(third.conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
	time.Sleep(50 * time.Millisecond)
	refused := dialKept(t, addr)
	if !refused.closedByServer(t) {
		t.Error("a connection past the source's bound, all of its others busy, was kept")
	}

	// Exempt, the same source is not bounded.
	limit := newConnLimit(16, 2, func(netip.Addr) bool { return true })
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ConnState: limit.track}
	go server.Serve(limit.listener(raw))
	defer server.Close()
	for range 4 {
		k := dialKept(t, raw.Addr().String())
		fmt.Fprint(k.conn, "GET /slow HTTP/1.1\r\nHost: x\r\n\r\n")
	}
	time.Sleep(50 * time.Millisecond)
	last := dialKept(t, raw.Addr().String())
	if err := request(t, last.conn, last.reader); err != nil {
		t.Errorf("an exempt source was bounded: %v", err)
	}
}

// The wrapper keeps the half-close net/http sends before closing on a body it
// did not read, so the client reads the answer and then an orderly end.
func TestLimitedConnPassesTheHalfCloseThrough(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	limit := newConnLimit(4, 4, nil)
	wrapped := limit.listener(listener)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := wrapped.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	half, ok := server.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the wrapped connection hides CloseWrite")
	}
	if _, ok := server.(io.ReaderFrom); !ok {
		t.Error("the wrapped connection hides ReadFrom")
	}
	io.WriteString(server, "answer")
	if err := half.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "answer" {
		t.Errorf("read %q, %v; want the answer and an orderly EOF", got, err)
	}
	// Half closed, not closed: the server still reads.
	io.WriteString(client, "more")
	buf := make([]byte, 4)
	server.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(server, buf); err != nil || string(buf) != "more" {
		t.Errorf("after the half-close the server read %q, %v", buf, err)
	}
}
