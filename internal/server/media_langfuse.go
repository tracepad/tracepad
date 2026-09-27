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
//
// A grant also names the key that asked for it (Decision 28): revoking that
// key voids it, and so does deleting or erasing its trace within the hour
// (Decision 29). Both are checked before the body is read, and again in the
// write.

// uploadGrant is what an upload token says the PUT may store.
type uploadGrant struct {
	Project  string `json:"p"`
	Trace    string `json:"t"`
	SHA256   string `json:"s"`
	MimeType string `json:"c"`
	Length   int64  `json:"n"`
	Expires  int64  `json:"e"`
	// Key is the public key that asked for the URL (Decision 28).
	Key string `json:"k"`
}

// storeGrant is what the store checks of a grant.
func (g *uploadGrant) storeGrant() store.MediaGrant {
	return store.MediaGrant{ProjectID: g.Project, TraceID: g.Trace, SHA256: g.SHA256, Key: g.Key}
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

// verifyUpload opens a token this server signed; any other is refused.
func (s *Server) verifyUpload(token string) (*uploadGrant, error) {
	encoded, signature, found := strings.Cut(token, ".")
	if !found {
		return nil, store.ErrUploadVoid
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, store.ErrUploadVoid
	}
	given, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return nil, store.ErrUploadVoid
	}
	mac := hmac.New(sha256.New, s.mediaKey)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(given, mac.Sum(nil)) != 1 {
		return nil, store.ErrUploadVoid
	}
	var grant uploadGrant
	if err := json.Unmarshal(payload, &grant); err != nil {
		return nil, store.ErrUploadVoid
	}
	return &grant, nil
}

// refusal is why a grant this server signed does not let a PUT to mediaID
// store anything now, or nil.
func (g *uploadGrant) refusal(mediaID string, now time.Time) error {
	// A token issued before grants named their key cannot be checked
	// against a revocation, so it is refused (Decision 28): the uploads
	// in transit across that one upgrade are what it costs.
	if g.Key == "" || store.MediaIDFor(g.SHA256) != mediaID {
		return store.ErrUploadVoid
	}
	if now.Unix() > g.Expires {
		return errors.New("this upload URL has expired; ask for a new one")
	}
	return nil
}

// handleLangfuseMediaUpload answers the SDK's request for an upload URL.
//
// `uploadUrl` is `null` only when this project already holds the body, and
// nothing is sent. The ref is written then: settled for a trace the project
// has, and pending for one not here yet, dated as the bytes are rather than
// as the ask, which keeps the body until the trace's spans come (Decision 30)
// — and counts toward the cap, so the null answer too can be a `429`. A body another project holds is still asked
// for, because skipping the upload on a hash alone would let any project
// adopt another's picture by naming it — the bytes are the proof of
// possession.
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
	// Case-insensitive (RFC 2045), stored as ingest stores it.
	request.ContentType = strings.ToLower(request.ContentType)
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

	// The URL's hour runs from before the check: a removal the check does
	// not see is stamped after this, and its record outlives the URL.
	issued := time.Now()
	// The ask is refused for a trace a deletion or an erasure removed
	// within the hour and that is not here (Decision 29), and at the cap
	// for a trace the project does not have, where there is no room for
	// the ref either answer would lead to (Decision 31); the guard has
	// settled the project and the key. A read of the pool, so a refusal
	// never reaches the writer.
	if err := s.store.MediaUploadRoom(r.Context(), project.ID, sha, request.TraceID); err != nil {
		var rejection *store.Rejection
		if errors.As(err, &rejection) {
			submitFailure(w, err, apiWrite)
			return
		}
		lookupFailed(w, r, "media", err)
		return
	}
	held, err := s.store.MediaHeld(r.Context(), project.ID, sha)
	if err != nil {
		lookupFailed(w, r, "media", err)
		return
	}
	if held != nil {
		ref := &store.MediaRefAdd{ProjectID: project.ID, SHA256: sha, TraceID: request.TraceID}
		if !s.submit(w, r, ref) {
			return
		}
		if ref.Held {
			writeJSON(w, http.StatusOK, object{}.put("mediaId", mediaID).put("uploadUrl", nil))
			return
		}
		// Collected between the read and the write: ask for the bytes.
	}

	// The route admits a project key and nothing else (the `ingest`
	// policy), and the grant names it.
	token, err := s.signUpload(uploadGrant{
		Project: project.ID, Trace: request.TraceID, SHA256: sha,
		MimeType: request.ContentType, Length: request.ContentLength,
		Expires: issued.Add(store.MediaUploadWindow).Unix(),
		Key:     callerFrom(r.Context()).key.PublicKey,
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
	// A PUT without a token this server signed is refused having read
	// nothing (#14): the route is public.
	values, err := queryParams(r, "token")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	grant, err := s.verifyUpload(values.Get("token"))
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	// An expired token, one for another id or one from before grants named
	// their key is refused having read nothing, like a forged one: no retry
	// can make it good.
	if err := grant.refusal(r.PathValue("mediaId"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	// Before the body is read: the project is there, the key that asked is
	// still one of its keys (Decision 28), its trace was not deleted or
	// erased within the hour (Decision 29), and a ref that would be pending
	// has room (Decision 31). The write asks all of it again.
	media, refusal := s.store.MediaGrantRefusal(r.Context(), grant.storeGrant())
	if refusal != nil {
		refuseUpload(w, r, refusal, grant.Length)
		return
	}

	// A grant longer than the whole budget — issued before a restart with a
	// smaller one — could never be read: a final answer, not a 429 its SDK
	// would retry until the grant expired (spec 043 #32). Decided having read
	// nothing, like the grant's other refusals.
	if grant.Length > s.bodies.capacityBytes() {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the upload is %d bytes and this server now holds at most %d at once; ask for a new upload URL",
				grant.Length, s.bodies.capacityBytes()))
		return
	}
	// The body counts against the budget every body does (spec 043 #13).
	// One the budget cannot hold is refused as the stored state's
	// refusals are, its answer first and the rest of it drained, so the
	// SDK reads the 429 it retries.
	body, err := io.ReadAll(budgeted(http.MaxBytesReader(w, r.Body, grant.Length+1), holdFrom(r.Context()), grant.Length))
	if errors.Is(err, errBodyBudget) {
		s.refuseBodyForBudget(w, r, grant.Project, grant.Length)
		return
	}
	if err != nil {
		// Only the cap is a size problem; a client that hung up or a
		// broken chunked body is not, and the SDK logs what it is told.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the body is larger than the upload declared")
			return
		}
		writeError(w, http.StatusBadRequest, "cannot read the upload body")
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
	if media == store.MediaPlaceholder {
		w.WriteHeader(http.StatusOK)
		return
	}
	job := &store.MediaUpload{
		Grant: grant.storeGrant(),
		Body:  store.MediaBody{SHA256: grant.SHA256, MimeType: grant.MimeType, Body: body},
	}
	if !s.submit(w, r, job) {
		return
	}
	w.WriteHeader(http.StatusOK)
}

// uploadDrainTime bounds how long a refused upload's body is read and
// dropped. A live grant the stored state refuses — a revoked key, a removed
// trace, the cap — whose body the budget could not hold (spec 043 #13), or
// that a failed lookup could not check is a client that was told to upload,
// and the SDK retries the 429 or the 503 it reads, not a
// connection reset under a body the server stopped reading (Decision 31). So
// the refusal is written and flushed first, and the body read and dropped
// after it, up to the length the grant declared — which the ask bounded by
// the API's body limit; past the length or the time the server stops reading
// and closes the connection.
const uploadDrainTime = 10 * time.Second

// refuseUpload answers a refusal from the stored state, or a failed lookup,
// then drains up to `length` of the body.
//
// A client that sent `Expect: 100-continue` hears the refusal before it sends
// anything: net/http sends no `100` once a final status is written, takes the
// body as closed, and closes the connection after the reply — so there is
// nothing to drain.
func refuseUpload(w http.ResponseWriter, r *http.Request, refusal error, length int64) {
	answerThenDrain(w, r, length, func() {
		var rejection *store.Rejection
		if errors.As(refusal, &rejection) {
			submitFailure(w, refusal, apiWrite)
		} else {
			lookupFailed(w, r, "media", refusal)
		}
	})
}

// answerThenDrain writes a refusal of an upload, flushes it, and then reads
// and drops up to `length` of the body, within uploadDrainTime: the SDK reads
// the status it can retry rather than a reset (Decision 31). Dropped, the
// rest of the body costs the budget nothing (spec 043 #13).
func answerThenDrain(w http.ResponseWriter, r *http.Request, length int64, answer func()) {
	control := http.NewResponseController(w)
	// Before the status: otherwise net/http reads what it will of the body
	// before writing it, and closes the connection past that.
	_ = control.EnableFullDuplex()
	answer()
	_ = control.Flush()
	// A recorder in a test takes no deadline; the drain's bound is then its
	// length alone.
	_ = control.SetReadDeadline(time.Now().Add(uploadDrainTime))
	_, _ = io.CopyN(io.Discard, r.Body, length)
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
	info, err := s.store.MediaByLangfuseID(r.Context(), project.ID, mediaID)
	if err != nil {
		readFailed(w, r, "failed to look the media up", err)
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
