package server

import (
	_ "embed"
	"net/http"
)

// The OpenAPI document (spec 004 #9). Hand-authored and embedded rather than
// generated: the document *is* the contract, so it belongs in the diff where a
// reviewer sees it change. What keeps it honest is the parity test, which
// walks the route table and fails on any route the document does not describe
// and any path the document describes that the server does not serve — the
// `docs-gen && git diff --exit-code` idea turned inward.

//go:embed openapi.json
var openAPIDocument []byte

// handleOpenAPI serves the document. No authentication: an agent deciding
// whether to talk to this server at all should not need a key to read what it
// would be talking to, and the document describes the shape of the API, never
// the data in it.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(openAPIDocument)
}
