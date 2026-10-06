//go:build !unix

package upgrade

import "errors"

func isMountPoint(string) bool { return false }

func freeBytes(string) (int64, error) { return 0, errors.New("not supported on this system") }

func lockFile(string) (func(), bool, error) {
	return nil, false, errors.New("not supported on this system")
}
