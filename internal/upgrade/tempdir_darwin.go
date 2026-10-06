//go:build darwin

package upgrade

import (
	"context"
	"strings"
	"time"
)

// userTempDir is macOS's temporary directory of this user's, as getconf
// says it (confstr's DARWIN_USER_TEMP_DIR): where mktemp makes directories
// whatever TMPDIR says. Empty when it cannot be asked.
func userTempDir() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := child(ctx, "getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return "" // the other rules still find the bridge: os.TempDir, mktemp's name
	}
	return strings.TrimSpace(string(out))
}
