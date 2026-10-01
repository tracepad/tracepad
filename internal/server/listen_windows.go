//go:build windows

package server

import (
	"errors"
	"syscall"
)

// The Winsock codes for the three errors listen_unix.go names: Go's syscall
// package spells them only as numbers.
const (
	wsaeProtoNoSupport syscall.Errno = 10043
	wsaeAFNoSupport    syscall.Errno = 10047
	wsaeAddrNotAvail   syscall.Errno = 10049
)

// familyUnavailable reports whether a bind failed because this host has no
// such address family or loopback address — IPv6 switched off, most often —
// rather than for a reason that concerns the port (spec 001 #23).
func familyUnavailable(err error) bool {
	return errors.Is(err, wsaeAddrNotAvail) || errors.Is(err, wsaeAFNoSupport) ||
		errors.Is(err, wsaeProtoNoSupport)
}
