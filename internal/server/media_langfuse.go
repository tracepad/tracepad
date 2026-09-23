package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/store"
)

// The Langfuse media channel (spec 041 #9), as the Langfuse Python SDK speaks
// it (`langfuse/_task_manager/media_manager.py`): it asks for an upload URL,
// PUTs the bytes there, and reports how the upload went. Served on the same
// `media` table as everything else, so a bridged picture is stored once and
// read back like any other.
//
// The upload URL is a *presigned* one (Decision 14). The SDK PUTs it with its
// plain HTTP client, which carries no credential — against Langfuse the URL
// points at an object store and the signature is in the query — so the route
// is public and the URL carries a signed token instead: the project, the trace,
// the hash, the type and the length the POST declared, and an hour to use it
// in. The token is HMAC-signed with a key the database keeps (Decision 22),
// so a URL issued before a restart still uploads within its hour.

// mediaUploadWindow is how long an upload URL is good for.
const mediaUploadWindow = time.Hour

// uploadGrant is what an upload token says the PUT may store.
type uploadGrant struct {
	Project  string `json:"p"`
	Trace    string `json:"t"`
	SHA256   string `json:"s"`
	MimeType string `json:"c"`
	Length   int64  `json:"n"`
	Expires  int64  `json:"e"`
}

func (s *Server) signUpload(grant uploadGrant) (string, error) {
	payload, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.mediaKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

var errBadUploadToken = errors.New("this upload URL is not valid; ask for a new one")

func (s *Server) verifyUpload(token string, now time.Time) (*uploadGrant, error) {
	encoded, signature, found := strings.Cut(token, ".")
	if !found {
		return nil, errBadUploadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errBadUploadToken
	}
	given, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return nil, errBadUploadToken
	}
	mac := hmac.New(sha256.New, s.mediaKey)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(given, mac.Sum(nil)) != 1 {
		return nil, errBadUploadToken
	}
	var grant uploadGrant
	if err := json.Unmarshal(payload, &grant); err != nil {
		return nil, errBadUploadToken
	}
	if now.Unix() > grant.Expires {
		return nil, errors.New("this upload URL has expired; ask for a new one")
	}
	return &grant, nil
}

// handleLangfuseMediaUpload answers the SDK's request for an upload URL.
//
// `uploadUrl` is `null` only when this project already holds the body: the
// ref for the named trace is recorded and nothing is sent. A body another
// project holds is still asked for, because skipping the upload on a hash
// alone would let any project adopt another's picture by naming it — the
// bytes are the proof of possession.
func (s *Server) handleLangfuseMediaUpload(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}
	// Lenient, unlike the rest of the API: this is the SDK's body, and a
	// field a newer SDK adds must not break an upload (spec 002 #17).
	var request struct {
		TraceID       string `json:"traceId"`
		ObservationID string `json:"observationId"`
		ContentType   string `json:"contentType"`
		ContentLength int64  `json:"contentLength"`
		SHA256Hash    string `json:"sha256Hash"`
		Field         string `json:"field"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if request.TraceID == "" {
		// A dataset item's media (the SDK's other caller) is out of
		// scope (spec 041, Overview).
		writeError(w, http.StatusBadRequest, "traceId is required: media is stored for traces only")
		return
	}
	if !mapping.IsHexID(request.TraceID) {
		// The id a trace is stored under; any other spelling is a ref
		// that no span could ever settle or resolve.
		writeError(w, http.StatusBadRequest, "traceId must be 32 lower-case hex digits")
		return
	}
	if !strings.Contains(request.ContentType, "/") {
		writeError(w, http.StatusBadRequest, "contentType must be a MIME type")
		return
	}
	if request.ContentLength <= 0 || request.ContentLength > s.maxBodyBytes {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("contentLength must be between 1 and %d bytes", s.maxBodyBytes))
		return
	}
	sum, err := base64.StdEncoding.DecodeString(request.SHA256Hash)
	if err != nil || len(sum) != sha256.Size {
		writeError(w, http.StatusBadRequest, "sha256Hash must be the base64 SHA-256 of the body")
		return
	}
	sha := hex.EncodeToString(sum)
	mediaID := store.MediaIDFor(sha)

	// The placeholder setting keeps no body (#6): nothing is asked for,
	// and the reference string the SDK leaves stays as it is.
	if project.Media == store.MediaPlaceholder {
		writeJSON(w, http.StatusOK, object{}.put("mediaId", mediaID).put("uploadUrl", nil))
		return
	}

	held, err := s.store.MediaHeld(project.ID, sha)
	if err != nil {
		slog.Error("media lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to look the media up")
		return
	}
	if held != nil {
		ref := &store.MediaRefAdd{ProjectID: project.ID, SHA256: sha, TraceID: request.TraceID}
		if !s.submit(w, r, ref) {
			return
		}
		if ref.Added {
			writeJSON(w, http.StatusOK, object{}.put("mediaId", mediaID).put("uploadUrl", nil))
			return
		}
		// Collected between the read and the write: ask for the bytes.
	}

	token, err := s.signUpload(uploadGrant{
		Project: project.ID, Trace: request.TraceID, SHA256: sha,
		MimeType: request.ContentType, Length: request.ContentLength,
		Expires: time.Now().Add(mediaUploadWindow).Unix(),
	})
	if err != nil {
		slog.Error("could not sign an upload URL", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to issue an upload URL")
		return
	}
	upload := s.originFor(r) + "/api/public/media/" + url.PathEscape(mediaID) +
		"/upload?token=" + url.QueryEscape(token)
	writeJSON(w, http.StatusOK, object{}.put("mediaId", mediaID).put("uploadUrl", upload))
}

// handleLangfuseMediaPut receives the bytes at the URL the POST handed out.
// The body must hash to what the POST declared — and to the SDK's own
// `x-amz-checksum-sha256` when it sends one — or nothing is stored.
func (s *Server) handleLangfuseMediaPut(w http.ResponseWriter, r *http.Request) {
	if s.store == nil || s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return
	}
	values, err := queryParams(r, "token")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grant, err := s.verifyUpload(values.Get("token"), time.Now())
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if store.MediaIDFor(grant.SHA256) != r.PathValue("mediaId") {
		writeError(w, http.StatusForbidden, errBadUploadToken.Error())
		return
	}
	project, err := s.store.ProjectByID(grant.Project)
	if err != nil {
		slog.Error("project lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the project")
		return
	}
	if project == nil || project.Deleted() {
		writeError(w, http.StatusForbidden, errBadUploadToken.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grant.Length+1))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the body is larger than the upload declared")
		return
	}
	sum := sha256.Sum256(body)
	if int64(len(body)) != grant.Length || hex.EncodeToString(sum[:]) != grant.SHA256 {
		writeError(w, http.StatusBadRequest, "the body does not match the declared length and SHA-256")
		return
	}
	if checksum := r.Header.Get("x-amz-checksum-sha256"); checksum != "" &&
		checksum != base64.StdEncoding.EncodeToString(sum[:]) {
		writeError(w, http.StatusBadRequest, "the body does not match x-amz-checksum-sha256")
		return
	}
	// Switched to the placeholder setting since the URL was issued: the
	// upload succeeds, as far as the SDK is concerned, and nothing is kept.
	if project.Media == store.MediaPlaceholder {
		w.WriteHeader(http.StatusOK)
		return
	}
	job := &store.MediaUpload{
		ProjectID: project.ID, TraceID: grant.Trace,
		Body: store.MediaBody{SHA256: grant.SHA256, MimeType: grant.MimeType, Body: body},
	}
	if !s.submit(w, r, job) {
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleLangfuseMediaPatch takes the SDK's report on an upload. There is no
// upload state to keep — the PUT either stored the body or did not — so the
// report is logged, a failure at warning level, and answered `204`.
func (s *Server) handleLangfuseMediaPatch(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}
	var report struct {
		UploadHTTPStatus int    `json:"uploadHttpStatus"`
		UploadHTTPError  string `json:"uploadHttpError"`
		UploadTimeMs     int64  `json:"uploadTimeMs"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	if report.UploadHTTPStatus >= 300 {
		slog.Warn("a Langfuse SDK reported a failed media upload", "project", project.Name,
			"media_id", r.PathValue("mediaId"), "status", report.UploadHTTPStatus,
			"error", report.UploadHTTPError)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLangfuseMediaGet answers the Langfuse shape for one of this project's
// bodies. `url` is the read endpoint, which needs a credential of the project
// like every read (#7); `urlExpiry` is a year away, because the address never
// changes.
func (s *Server) handleLangfuseMediaGet(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	mediaID := r.PathValue("mediaId")
	info, err := s.store.MediaByLangfuseID(project.ID, mediaID)
	if err != nil {
		slog.Error("media lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to look the media up")
		return
	}
	if info == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("media %s not found", mediaID))
		return
	}
	writeJSON(w, http.StatusOK, object{}.
		put("mediaId", mediaID).
		put("contentType", info.MimeType).
		put("contentLength", info.Size).
		put("uploadedAt", formatTime(info.CreatedAt)).
		put("url", s.originFor(r)+"/api/v1/media/"+info.SHA256).
		put("urlExpiry", time.Now().AddDate(1, 0, 0).UTC().Format(time.RFC3339)))
}
