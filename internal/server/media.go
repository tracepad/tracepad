package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Media (spec 041): what ingest takes out of the payloads, the one endpoint
// that serves it back, and the Langfuse media channel (media_langfuse.go).

// mediaOptions is one project's walk: its setting (#6), the Langfuse ids it
// can resolve (#9), and where a malformed body is reported (edge cases).
func (s *Server) mediaOptions(project *store.Project) mapping.MediaOptions {
	return mapping.MediaOptions{
		Placeholder: project.Media == store.MediaPlaceholder,
		Resolve: func(mediaID string) (string, int64, bool) {
			info, err := s.store.MediaByLangfuseID(project.ID, mediaID)
			if err != nil {
				slog.Warn("could not resolve a Langfuse media id; the reference is left as sent",
					"project", project.Name, "media_id", mediaID, "err", err)
				return "", 0, false
			}
			if info == nil {
				return "", 0, false
			}
			return info.SHA256, info.Size, true
		},
		Warn: func(traceID, reason string) {
			slog.Warn("malformed media left inline", "project", project.Name,
				"trace_id", traceID, "reason", reason)
		},
	}
}

// mediaRows turns what the walk found into what the batch writes. A ref is
// kept only for a trace the batch actually stores — a span the mapper skipped
// has no row to follow — and a body only while something keeps it: a kept
// ref, or the raw body when it was stored with the media factored out.
func mediaRows(found *mapping.MediaResult, traces []*model.Trace, rawMedia bool) (
	[]store.MediaBody, []store.MediaRef, []string) {
	stored := make(map[string]bool, len(traces))
	for _, t := range traces {
		stored[t.ID] = true
	}
	var refs []store.MediaRef
	kept := map[string]bool{}
	for _, ref := range found.Refs {
		if stored[ref.TraceID] {
			refs = append(refs, ref)
			kept[ref.SHA256] = true
		}
	}
	var raw []string
	if rawMedia {
		raw = found.SHAs()
		for _, sha := range raw {
			kept[sha] = true
		}
	}
	var bodies []store.MediaBody
	for _, body := range found.Bodies {
		if kept[body.SHA256] {
			bodies = append(bodies, *body)
		}
	}
	return bodies, refs, raw
}

// mediaCacheControl is the cache lifetime of a body: a year, and `immutable`,
// because the address is the content's hash and cannot come to mean other
// bytes (#7).
const mediaCacheControl = "private, max-age=31536000, immutable"

// credentialVary keys a cached response by the credential and the project that
// read it: the three things that decide which project a request is about
// (spec 028 Decision 6). The guard sends it on every route that needs a
// caller (spec 001 #17).
const credentialVary = "Authorization, Cookie, " + projectHeader

// mediaSandbox is the policy every body is served under. The bytes are the
// client's, and a body declared `text/html` or `image/svg+xml` opened as a page
// on this origin would otherwise run whatever script it carries next to the
// session cookie (Decision 15). It replaces the server-wide policy on these
// responses rather than sitting beside it, so it carries that policy's
// `frame-ancestors 'none'` too (spec 001 #14).
const mediaSandbox = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox; frame-ancestors 'none'"

// handleGetMedia serves one body to a project that points at it (#7). A hash
// the project holds no ref to is `404`, exactly as one nobody holds is: a
// hash that appeared in a log is not a capability across projects.
func (s *Server) handleGetMedia(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sha := r.PathValue("sha256")
	if !store.ValidMediaSHA(sha) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("a media id is the lower-case hex SHA-256 of the body, got %q", sha))
		return
	}
	file, err := s.store.MediaFor(project.ID, sha)
	if err != nil {
		slog.Error("read media failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the media")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("media %s not found", sha))
		return
	}
	writeMediaBody(w, sha, file)
}

// writeMediaBody answers with the bytes and the headers every body carries.
// Only images, audio and video are offered inline; anything else is a
// download, because a browser has no business rendering it on this origin.
func writeMediaBody(w http.ResponseWriter, sha string, file *store.MediaFile) {
	header := w.Header()
	header.Set("Content-Type", file.MimeType)
	header.Set("Content-Length", strconv.Itoa(len(file.Body)))
	// The answer depends on who asks: a browser that cached a body under one
	// project must not serve it from the cache under another, which holds no
	// ref to it (#7). The Vary that says so is the guard's, sent on every
	// route that needs a caller (spec 001 #17); only the lifetime is this
	// handler's.
	header.Set("Cache-Control", mediaCacheControl)
	header.Set("Content-Security-Policy", mediaSandbox)
	if !inlineMedia(file.MimeType) {
		header.Set("Content-Disposition", `attachment; filename="`+sha[:16]+`"`)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(file.Body)
}

func inlineMedia(mime string) bool {
	for _, prefix := range []string{"image/", "audio/", "video/"} {
		if strings.HasPrefix(mime, prefix) {
			return true
		}
	}
	return false
}

// inlineRawMedia puts the bytes back where the references are in an archived
// body (#8), so that the one reader whose consumer does not know Tracepad's
// references — the export — sends a batch that is whole. A body with no
// reference in it is answered as stored, without being decoded at all.
func (s *Server) inlineRawMedia(projectID string, batch *store.RawBody) []byte {
	if !bytes.Contains(batch.Body, []byte(mapping.MediaRefKey)) {
		return batch.Body
	}
	decoded, err := mapping.DecodeExportBody(batch.Body, batch.ContentType == contentTypeJSON)
	if err != nil {
		// Unreachable for a body ingest decoded and factored; were it to
		// happen, the reader gets the references, and the log says so.
		slog.Warn("could not decode a raw body to inline its media; serving it as stored",
			"raw_batch", batch.ID, "err", err)
		return batch.Body
	}
	changed := mapping.InlineMedia(decoded.ResourceSpans, func(sha string) (string, []byte, bool) {
		file, err := s.store.MediaFor(projectID, sha)
		if err != nil {
			slog.Warn("could not read media to inline into a raw body", "sha256", sha, "err", err)
			return "", nil, false
		}
		if file == nil {
			return "", nil, false
		}
		return file.MimeType, file.Body, true
	})
	whole, err := decoded.Encode(changed)
	if err != nil {
		slog.Error("could not inline media into a raw body; serving it as stored", "err", err)
		return batch.Body
	}
	return whole
}

// mediaBlock is what `GET /api/v1/system` says about media (#11): the
// project's setting, and the bodies its refs hold with their bytes.
func (s *Server) mediaBlock(project *store.Project) (object, error) {
	summary, err := s.store.MediaSummary(project.ID)
	if err != nil {
		return nil, err
	}
	return object{}.
		put("setting", project.Media).
		put("count", summary.Count).
		put("bytes", summary.Bytes), nil
}
