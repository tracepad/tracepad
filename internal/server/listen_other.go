//go:build !unix && !windows

package server

// familyUnavailable has no errors to recognize on a platform that is neither:
// a family that fails to bind there fails the start (spec 001 #23).
func familyUnavailable(error) bool { return false }
