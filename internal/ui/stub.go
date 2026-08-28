//go:build !ui

package ui

import "io/fs"

// Enabled reports whether this build carries the web interface.
const Enabled = false

// Assets is nil in a build without the SPA. The server reads that as "serve
// the stub at every route the interface would have owned".
func Assets() fs.FS { return nil }
