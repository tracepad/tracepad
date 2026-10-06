package upgrade

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// nativeArch is the architecture to install for: under Rosetta the native
// build, as the install script decides.
func nativeArch() string {
	if runtime.GOARCH == "amd64" {
		if t, err := unix.SysctlUint32("sysctl.proc_translated"); err == nil && t == 1 {
			return "arm64"
		}
	}
	return runtime.GOARCH
}
