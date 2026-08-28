// Package ui carries the single-page application the server hands to a
// browser. It exists in two variants selected by the `ui` build tag (spec 006
// Decision 9): with the tag, the built bundle is embedded; without it, only
// the stub page below is, and the server says so rather than 404ing at the
// root.
//
// The split is what keeps `go build ./...`, `go vet ./...` and the whole Go
// test suite runnable with no Node toolchain present: the tagless variant
// embeds one HTML file that lives in this directory, and `internal/ui/dist`
// — the copy `make ui` drops here — is never referenced.
package ui

import _ "embed"

// Stub is the page a build without the `ui` tag serves at every SPA route. It
// is not an error page: the API, the CLI and the MCP server of such a build
// are complete, and the only honest thing to say is which builds carry the
// interface.
//
//go:embed stub.html
var Stub []byte
