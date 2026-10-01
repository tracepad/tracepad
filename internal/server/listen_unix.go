//go:build unix

package server

import (
	"errors"
	"syscall"
)

// familyUnavailable reports whether a bind failed because this host has no
// such address family or loopback address — IPv6 switched off, most often —
// rather than for a reason that concerns the port (spec 001 #23).
func familyUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT)
}
