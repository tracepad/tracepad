//go:build ui

package ui

import (
	"embed"
	"io/fs"
)

// Enabled reports whether this build carries the web interface.
const Enabled = true

// `all:` keeps the files SvelteKit emits with a leading underscore —
// `_app/immutable/…` is the whole bundle — which the default embed rules
// would silently drop.
//
//go:embed all:dist
var bundle embed.FS

// Assets is the built SPA, rooted at the directory that holds `index.html`.
// `make ui` builds it into `ui/dist` and copies it here; the directory is
// gitignored, so a tree without it simply cannot be built with this tag —
// which is the intended failure, loud and at compile time.
func Assets() fs.FS {
	sub, err := fs.Sub(bundle, "dist")
	if err != nil {
		// Unreachable: the embed above fails to compile before this can
		// happen.
		panic(err)
	}
	return sub
}
