//go:build unix

package server

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

// A family the host does not have is a warning and the other family serves; a
// port somebody else holds on either refuses the start, and the listener that
// did bind is let go (spec 001 #23).
func TestOneLoopbackFailing(t *testing.T) {
	refuse := func(network string, err error) func(string, string) (net.Listener, error) {
		return func(n, address string) (net.Listener, error) {
			if n == network {
				return nil, &net.OpError{Op: "listen", Net: n, Err: err}
			}
			return net.Listen("tcp4", "127.0.0.1:0")
		}
	}

	listeners, err := listen("localhost:4318", refuse("tcp6", syscall.EADDRNOTAVAIL))
	if err != nil || len(listeners) != 1 {
		t.Fatalf("without IPv6: %d listeners, %v; want the IPv4 one", len(listeners), err)
	}
	closeAll(listeners)

	var bound net.Listener
	inUse := func(n, address string) (net.Listener, error) {
		if n == "tcp6" {
			return nil, &net.OpError{Op: "listen", Net: n, Err: syscall.EADDRINUSE}
		}
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		bound = l
		return l, err
	}
	if listeners, err := listen("localhost:4318", inUse); err == nil || listeners != nil ||
		!errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("with ::1 taken: %v, %v; want a refusal naming the port in use", listeners, err)
	}
	if _, err := bound.Accept(); err == nil {
		t.Fatal("the IPv4 listener was left open after the refusal")
	}
}
