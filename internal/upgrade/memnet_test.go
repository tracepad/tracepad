package upgrade

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"testing"
)

// memNet is the fake host's loopback (spec 054 #63): a server's address is a
// key in this process, never a port of the machine. With real ports, a port
// freeAddr chose was free only until it closed its listener, and a server the
// command stopped freed its port until the next start took it: in either
// window a parallel test, or another process on the machine, could take it (two
// flakes of the 0.1.1 gate). Here nothing outside the process listens, a key
// is never handed out twice, and a dial where nothing listens is refused at
// once, as loopback refuses it.
var memNet = struct {
	sync.Mutex
	next      int
	listeners map[string]*memListener
}{next: 20000, listeners: map[string]*memListener{}}

// freeAddr is a loopback address nothing listens on, and that no other test
// of the process is given.
func freeAddr(*testing.T) string {
	memNet.Lock()
	defer memNet.Unlock()
	memNet.next++
	return "127.0.0.1:" + strconv.Itoa(memNet.next)
}

// memKey is an address as loopback reaches it: by its family and port, so a
// server on 0.0.0.0 is reached at 127.0.0.1 and one on [::] at [::1], and
// never one family's at the other's.
func memKey(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "127.0.0.1", "localhost":
		host = "4"
	case "::", "::1":
		host = "6"
	}
	return host + "/" + port
}

type memAddr string

func (a memAddr) Network() string { return "tcp" }
func (a memAddr) String() string  { return string(a) }

type memListener struct {
	key   string
	addr  memAddr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

// memListen listens at addr, or is refused as an address in use is.
func memListen(addr string) (net.Listener, error) {
	key := memKey(addr)
	memNet.Lock()
	defer memNet.Unlock()
	if _, ok := memNet.listeners[key]; ok {
		return nil, &net.OpError{Op: "listen", Net: "tcp", Err: syscall.EADDRINUSE}
	}
	l := &memListener{key: key, addr: memAddr(addr), conns: make(chan net.Conn), done: make(chan struct{})}
	memNet.listeners[key] = l
	return l, nil
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() {
		memNet.Lock()
		if memNet.listeners[l.key] == l {
			delete(memNet.listeners, l.key)
		}
		memNet.Unlock()
		close(l.done)
	})
	return nil
}

func (l *memListener) Addr() net.Addr { return l.addr }

// memDial connects to the listener at addr.
func memDial(ctx context.Context, _, addr string) (net.Conn, error) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	memNet.Lock()
	l := memNet.listeners[memKey(addr)]
	memNet.Unlock()
	if l == nil {
		return nil, refused
	}
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
		refused.Err = ctx.Err()
	}
	client.Close() // ignored: never handed out
	server.Close() // ignored: never accepted
	return nil, refused
}

// memTransport is an HTTP transport over memNet, with no proxy, as the
// command's own is.
func memTransport() *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: memDial}
}
