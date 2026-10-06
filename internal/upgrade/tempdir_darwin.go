//go:build darwin

package upgrade

import (
	"context"
	"strings"
	"time"
)

// systemUserTempDir is macOS's temporary directory of this user's, as
// getconf says it (confstr's DARWIN_USER_TEMP_DIR): where mktemp makes
// directories whatever TMPDIR says. getconf by its path: a shell with no
// TMPDIR often has no PATH either (the review of #225). Empty when it
// cannot be asked.
func systemUserTempDir() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := child(ctx, "/usr/bin/getconf", "DARWIN_USER_TEMP_DIR").Output()
	if err != nil {
		return "" // the other rules still find the bridge: os.TempDir, mktemp's name
	}
	return strings.TrimSpace(string(out))
}
