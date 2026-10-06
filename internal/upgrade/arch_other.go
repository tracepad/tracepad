//go:build !darwin

package upgrade

import "runtime"

// nativeArch is the architecture to install for.
func nativeArch() string { return runtime.GOARCH }
