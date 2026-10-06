//go:build !unix

package upgrade

import (
	"errors"
	"os"
)

func isMountPoint(string) bool { return false }

func freeBytes(string) (int64, error) { return 0, errors.New("not supported on this system") }

func ownedByMe(os.FileInfo) bool { return true }
