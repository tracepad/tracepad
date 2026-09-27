package server

import (
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/config"
)

/*
Where a request comes from (spec 046 #1–#5).

The TCP peer is the one address a client cannot choose, so it is the source —
unless it is a proxy the operator named in TRACEPAD_TRUSTED_PROXIES, and then
the source is the hop that proxy vouches for in `X-Forwarded-For`. Each proxy
appends the peer it saw, so an entry is only as good as the hop to its right:
the header is read from the right, through the trusted hops, and the first
address that is not trusted is the client. Everything to its left was written
by that client and may be invented.

One reading serves every use of a client's address — the per-source limit on
password checks, the address a session is listed with, and the lines the log
names a client in — because two readings of one fact drift.
*/

// maxForwardedHops is how many `X-Forwarded-For` entries are read from the
// right (spec 046 #2). No real chain of proxies is that deep, and a header of
// thousands of entries is not a parser workload.
const maxForwardedHops = 32

// trustedProxies is the parsed TRACEPAD_TRUSTED_PROXIES.
type trustedProxies []netip.Prefix

// newTrustedProxies turns the configured list into the one the server reads
// with: nil is the default, loopback, and an empty list trusts nobody.
func newTrustedProxies(configured []netip.Prefix) trustedProxies {
	if configured == nil {
		return trustedProxies(config.LoopbackProxies)
	}
	return trustedProxies(configured)
}

func (t trustedProxies) trusts(addr netip.Addr) bool {
	for _, prefix := range t {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// strings is the list as /api/v1/system and the log show it.
func (t trustedProxies) strings() []string {
	out := make([]string, 0, len(t))
	for _, prefix := range t {
		out = append(out, prefix.String())
	}
	return out
}

// TrustedProxies is the list in effect, as the start logs it: "none" when it
// trusts nobody.
func (s *Server) TrustedProxies() string {
	if len(s.trusted) == 0 {
		return "none"
	}
	return strings.Join(s.trusted.strings(), ",")
}

// clientAddress is where the request comes from (spec 046 #1, #2): the peer,
// or, while the peer is a trusted proxy, the next hop to the left in
// `X-Forwarded-For`. An entry that is not an address stops the walk at the
// last trusted one, so an invented entry cannot move it further left. An
// address that could not be read at all is the zero Addr, which is a source
// of its own.
func (s *Server) clientAddress(r *http.Request) netip.Addr {
	peer := peerAddress(r.RemoteAddr)
	if !s.trusted.trusts(peer) {
		s.noteUntrustedProxy(r, peer)
		return peer
	}
	current := peer
	hops := forwardedHops(r.Header.Values("X-Forwarded-For"))
	for range maxForwardedHops {
		entry, ok := hops()
		if !ok {
			return current
		}
		addr, ok := parseForwardedAddress(entry)
		if !ok {
			return current
		}
		current = addr
		if !s.trusted.trusts(current) {
			return current
		}
	}
	return current
}

// peerAddress reads the connection's address, as net/http writes it.
func peerAddress(remote string) netip.Addr {
	if addrPort, err := netip.ParseAddrPort(remote); err == nil {
		return normalAddress(addrPort.Addr())
	}
	if addr, err := netip.ParseAddr(remote); err == nil {
		return normalAddress(addr)
	}
	return netip.Addr{}
}

// normalAddress drops what names the same client twice: an IPv6 zone, and
// the IPv4-mapped spelling a dual-stack socket gives an IPv4 peer.
func normalAddress(addr netip.Addr) netip.Addr {
	return addr.WithZone("").Unmap()
}

// forwardedHops returns the entries of every `X-Forwarded-For` line, joined
// in order, one at a time from the right. It cuts entries out of the strings
// in place rather than splitting them, so only the ones the walk reads are
// looked at. An empty entry — "a,", or an empty line — is returned as one,
// and the walk stops there like at any other entry that is not an address.
func forwardedHops(lines []string) func() (string, bool) {
	line, rest, open := len(lines)-1, "", false
	return func() (string, bool) {
		if !open {
			if line < 0 {
				return "", false
			}
			rest, open = lines[line], true
			line--
		}
		var entry string
		if i := strings.LastIndexByte(rest, ','); i >= 0 {
			entry, rest = rest[i+1:], rest[:i]
		} else {
			entry, open = rest, false
		}
		return strings.TrimSpace(entry), true
	}
}

// parseForwardedAddress reads one `X-Forwarded-For` entry: an address, with
// a port or not, an IPv6 one in brackets or not. Anything else — empty,
// `unknown`, an obfuscated identifier — is not an address.
func parseForwardedAddress(entry string) (netip.Addr, bool) {
	if addr, err := netip.ParseAddr(entry); err == nil {
		return normalAddress(addr), true
	}
	if addrPort, err := netip.ParseAddrPort(entry); err == nil {
		return normalAddress(addrPort.Addr()), true
	}
	if inner, ok := strings.CutPrefix(entry, "["); ok {
		if inner, ok = strings.CutSuffix(inner, "]"); ok {
			if addr, err := netip.ParseAddr(inner); err == nil {
				return normalAddress(addr), true
			}
		}
	}
	return netip.Addr{}, false
}

// sourceOf is the source a client's address counts as (spec 046 #4): an IPv4
// address is itself, an IPv6 one is its /64 — one host is routinely given a
// whole /64, and its privacy addresses rotate inside it.
func sourceOf(addr netip.Addr) netip.Prefix {
	if !addr.IsValid() {
		return netip.Prefix{}
	}
	if addr.Is4() {
		return netip.PrefixFrom(addr, 32)
	}
	prefix, _ := addr.Prefix(64)
	return prefix
}

// sourceText is a source as the log and /api/v1/system name it: an IPv4
// address bare, an IPv6 source as its /64.
func sourceText(source netip.Prefix) string {
	switch {
	case !source.IsValid():
		return "unknown"
	case source.Addr().Is4():
		return source.Addr().String()
	}
	return source.String()
}

// addressText is a client's address as a session is listed with it.
func addressText(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// noteUntrustedProxy warns about a proxy nobody named (spec 046 #12): a
// request carrying `X-Forwarded-For` from a loopback or private peer that is
// not trusted is almost always a proxy in front of this server — a container
// bridge's gateway, say — and every client behind it is then one source.
// A public peer sending the header is a client, and a client inventing
// headers is not the operator's to be told about. Once an hour per peer, for
// at most eight peers an hour.
func (s *Server) noteUntrustedProxy(r *http.Request, peer netip.Addr) {
	if !peer.IsValid() || !proxyLike(peer) || len(r.Header.Values("X-Forwarded-For")) == 0 {
		return
	}
	text := peer.String()
	if held, ok := s.proxyLog.Allow(text, time.Now()); ok {
		slog.Warn("a request arrived through a proxy this server does not trust, so every client behind it counts as one source; "+
			"add the proxy's address to TRACEPAD_TRUSTED_PROXIES",
			"peer", text, "trusted_proxies", s.TrustedProxies(),
			"not_logged_since_last", held.SameKey, "not_logged_over_cap", held.OverCap)
	}
}

// sharedAddressSpace is 100.64.0.0/10 (RFC 6598), which carrier and cloud
// networks use between their proxies and the hosts behind them.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// proxyLike is an address a proxy of this deployment would have: loopback,
// private (RFC 1918, fc00::/7), shared (RFC 6598) or link-local.
func proxyLike(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || sharedAddressSpace.Contains(addr)
}
