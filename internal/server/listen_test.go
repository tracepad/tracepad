package server

import (
	"net"
	"testing"
)

// `localhost` is both loopback addresses on one port, so a client reaches the
// server whichever family it tries first (spec 001 #23).
func TestLocalhostBindsBothLoopbacks(t *testing.T) {
	if probe, err := net.Listen("tcp6", "[::1]:0"); err != nil {
		t.Skipf("this host has no IPv6 loopback: %v", err)
	} else {
		probe.Close()
	}
	listeners, err := listen("localhost:0", net.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(listeners)
	if len(listeners) != 2 {
		t.Fatalf("got %d listeners, want 127.0.0.1 and ::1", len(listeners))
	}
	v4, v6 := listeners[0].Addr().(*net.TCPAddr), listeners[1].Addr().(*net.TCPAddr)
	if !v4.IP.Equal(net.IPv4(127, 0, 0, 1)) || !v6.IP.Equal(net.IPv6loopback) || v4.Port != v6.Port {
		t.Fatalf("bound %v and %v, want both loopbacks on one port", v4, v6)
	}
	for _, addr := range []string{v4.String(), v6.String()} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		conn.Close()
	}
}

// Any other host is one address, as net.Listen binds it.
func TestAnAddressBindsItself(t *testing.T) {
	listeners, err := listen("127.0.0.1:0", net.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(listeners)
	if len(listeners) != 1 {
		t.Fatalf("got %d listeners for one address", len(listeners))
	}
}

func closeAll(listeners []net.Listener) {
	for _, l := range listeners {
		l.Close()
	}
}
