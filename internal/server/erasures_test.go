package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// A confirmed erasure is a task (spec 047 #6–#14): accepted with the resource
// and where to watch it, answered at once when it ends within `wait`.

// erasureBody is the erasure resource as a client reads it.
type erasureBody struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	Phase      *string `json:"phase"`
	UserID     *string `json:"user_id"`
	DryRun     bool    `json:"dry_run"`
	FinishedAt *string `json:"finished_at"`
	Progress   struct {
		TracesAtStart *int64 `json:"traces_at_start"`
		TracesDeleted int64  `json:"traces_deleted"`
	} `json:"progress"`
	Deleted map[string]int64 `json:"deleted"`
	Error   *string          `json:"error"`
}

// `202` with `Location` and the resource; `GET` of the location reads it, and
// the listing has it. With `wait` the same shape comes back `200` once it has
// ended (#6, #7, #8, #14).
func TestAConfirmedErasureIsAcceptedAndWatched(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"})
	h.seed(t, &model.Trace{ID: traceHex(2), UserID: "keep-me"})
	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"

	rec := h.call(t, "DELETE", path+"?confirm=erase-me", nil)
	expectStatus(t, rec, http.StatusAccepted)
	expectUniqueKeys(t, rec)
	accepted := decodeJSON[erasureBody](t, rec)
	location := rec.Header().Get("Location")
	if location != "/api/v1/projects/"+h.project.ID+"/erasures/"+accepted.ID || len(accepted.ID) != 32 {
		t.Fatalf("Location %q for erasure %q", location, accepted.ID)
	}
	if accepted.UserID == nil || *accepted.UserID != "erase-me" || accepted.DryRun ||
		(accepted.State != store.ErasureQueued && accepted.State != store.ErasureRunning &&
			accepted.State != store.ErasureDone) {
		t.Errorf("accepted = %+v, want the user's erasure under way", accepted)
	}

	// Watched until it ends, which takes the worker moments.
	var watched erasureBody
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		rec = h.get(t, location)
		expectStatus(t, rec, http.StatusOK)
		if watched = decodeJSON[erasureBody](t, rec); watched.State == store.ErasureDone {
			break
		}
	}
	if watched.State != store.ErasureDone || watched.Deleted["traces"] != 1 || watched.Progress.TracesDeleted != 1 ||
		watched.Progress.TracesAtStart == nil || *watched.Progress.TracesAtStart != 1 || watched.FinishedAt == nil {
		t.Fatalf("watched = %+v, want done with the one trace", watched)
	}
	// It has ended, so it names no one (#9).
	if watched.UserID != nil || watched.Phase != nil {
		t.Errorf("an ended erasure answers user %v in phase %v, want null and null", watched.UserID, watched.Phase)
	}
	listing := decodeJSON[struct {
		Erasures []erasureBody `json:"erasures"`
	}](t, h.get(t, "/api/v1/projects/"+h.project.ID+"/erasures"))
	if len(listing.Erasures) != 1 || listing.Erasures[0].ID != accepted.ID || listing.Erasures[0].UserID != nil {
		t.Errorf("listing = %+v, want the one erasure, naming no one", listing.Erasures)
	}

	// Waited for, a small erasure is answered in one round trip, and the
	// answer to the request that started it names the user.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/keep-me/data?confirm=keep-me&wait=30", nil)
	expectStatus(t, rec, http.StatusOK)
	expectUniqueKeys(t, rec)
	waited := decodeJSON[erasureBody](t, rec)
	if waited.State != store.ErasureDone || waited.UserID == nil || *waited.UserID != "keep-me" ||
		waited.Deleted["traces"] != 1 || rec.Header().Get("Location") == "" {
		t.Errorf("waited = %+v, want done, the user named, and where to read it", waited)
	}

	// Unknown, and another project's, are the same 404.
	expectError(t, h.get(t, "/api/v1/projects/"+h.project.ID+"/erasures/"+strings.Repeat("0", 32)),
		http.StatusNotFound, "no such erasure")
	other, err := h.store.CreateProject("other", store.KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	expectError(t, h.call(t, "GET", "/api/v1/projects/"+other.ID+"/erasures/"+accepted.ID, nil, asAdmin),
		http.StatusNotFound, "no such erasure")
}

// Everything that can refuse an erasure refuses before it is recorded (#6): a
// wrong echo, a `wait` out of range, a viewer, a key without `write`, a
// soft-deleted project.
func TestARefusedErasureRecordsNothing(t *testing.T) {
	h := newAccountHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"})
	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"
	readOnly := h.mint(t, "read", "ingest")
	viewer := h.viewer(t)

	for _, tc := range []struct {
		name   string
		query  string
		status int
		as     []func(*http.Request)
	}{
		{"a wrong echo", "?confirm=keep-me", http.StatusBadRequest, nil},
		{"a wait too long", "?confirm=erase-me&wait=31", http.StatusBadRequest, nil},
		{"a wait that is not a number", "?confirm=erase-me&wait=x", http.StatusBadRequest, nil},
		{"a negative wait", "?confirm=erase-me&wait=-1", http.StatusBadRequest, nil},
		{"a viewer", "?confirm=erase-me", http.StatusForbidden,
			[]func(*http.Request){asSession(viewer), inProject(h.project.ID)}},
		{"a key without write", "?confirm=erase-me", http.StatusForbidden,
			[]func(*http.Request){asKey(readOnly.SecretKey)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectStatus(t, h.call(t, "DELETE", path+tc.query, nil, tc.as...), tc.status)
		})
	}
	// Watching is the same role and scope as starting.
	expectStatus(t, h.call(t, "GET", "/api/v1/projects/"+h.project.ID+"/erasures", nil,
		asSession(viewer), inProject(h.project.ID)), http.StatusForbidden)
	expectStatus(t, h.call(t, "GET", "/api/v1/projects/"+h.project.ID+"/erasures", nil,
		asKey(readOnly.SecretKey)), http.StatusForbidden)

	if err := h.writer.Submit(t.Context(), &store.ProjectDelete{
		ProjectID: h.project.ID, Confirm: "test", Now: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, h.call(t, "DELETE", path+"?confirm=erase-me", nil, asAdmin), http.StatusConflict)

	erasures, err := h.store.Erasures(t.Context(), h.project.ID)
	if err != nil || len(erasures) != 0 {
		t.Errorf("the refusals recorded %d erasures (%v)", len(erasures), err)
	}
	if got := h.countTraces(t); got != 1 {
		t.Errorf("%d traces left after the refusals, want the one", got)
	}
}

// While an erasure of a user is queued or running, a second request answers it
// (#11), the dry run names it (#13), and the listing names the user (#14).
func TestAnErasureUnderWayIsAnsweredAndNamed(t *testing.T) {
	h := newAdminHarness(t)
	// No worker: what a request records stays queued.
	h.eraser.Close()
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"})
	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"

	first := decodeJSON[erasureBody](t, h.call(t, "DELETE", path+"?confirm=erase-me", nil))
	rec := h.call(t, "DELETE", path+"?confirm=erase-me&wait=1", nil)
	expectStatus(t, rec, http.StatusAccepted)
	if second := decodeJSON[erasureBody](t, rec); second.ID != first.ID || second.State != store.ErasureQueued {
		t.Errorf("a second request answered %s (%s), want the first, %s, queued", second.ID, second.State, first.ID)
	}

	rec = h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, http.StatusOK)
	expectUniqueKeys(t, rec)
	preview := decodeJSON[struct {
		DryRun  bool `json:"dry_run"`
		Running *struct {
			ID    string  `json:"id"`
			State string  `json:"state"`
			Phase *string `json:"phase"`
		} `json:"running"`
	}](t, rec)
	if !preview.DryRun || preview.Running == nil || preview.Running.ID != first.ID ||
		preview.Running.State != store.ErasureQueued || preview.Running.Phase != nil {
		t.Errorf("the dry run's running = %+v, want the queued erasure", preview.Running)
	}
	// Another user's dry run has no such block.
	if strings.Contains(h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/keep-me/data", nil).Body.String(),
		`"running"`) {
		t.Error("another user's dry run names a running erasure")
	}

	listing := decodeJSON[struct {
		Erasures []erasureBody `json:"erasures"`
	}](t, h.get(t, "/api/v1/projects/"+h.project.ID+"/erasures"))
	if len(listing.Erasures) != 1 || listing.Erasures[0].UserID == nil || *listing.Erasures[0].UserID != "erase-me" {
		t.Errorf("listing = %+v, want the queued erasure, naming its user", listing.Erasures)
	}
}

// A stop does not wait for an erasure (#17), and neither does a request
// waiting for one: the worker stopping answers it at once, so the drain is not
// held for the rest of its thirty seconds.
func TestAStopAnswersARequestWaitingForAnErasure(t *testing.T) {
	h := newAdminHarness(t)
	h.eraser.Close()
	held := h.store.NewEraser(eraserWaitsForever{}, store.EraserOptions{})
	held.Start()
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"})

	// When the answer came, taken as it comes: the worker's own stop may
	// spend its second trying to give its start back through this writer,
	// which never answers, and that is no wait of the request's.
	type answer struct {
		rec *httptest.ResponseRecorder
		at  time.Time
	}
	answered := make(chan answer, 1)
	go func() {
		rec := h.call(t, "DELETE",
			"/api/v1/projects/"+h.project.ID+"/users/erase-me/data?confirm=erase-me&wait=30", nil)
		answered <- answer{rec, time.Now()}
	}()
	// The request is waiting once its erasure is recorded.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if erasures, _ := h.store.Erasures(t.Context(), h.project.ID); len(erasures) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the erasure was never recorded")
		}
	}
	time.Sleep(50 * time.Millisecond)
	stopped := time.Now()
	held.Close()
	select {
	case got := <-answered:
		if took := got.at.Sub(stopped); took > 500*time.Millisecond {
			t.Errorf("the waiting request was answered %v after the stop", took)
		}
		expectStatus(t, got.rec, http.StatusAccepted)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request was not answered when the worker stopped")
	}
}

// eraserWaitsForever is a writer the erasure worker's jobs wait in until the worker
// stops, as behind a commit that does not end.
type eraserWaitsForever struct{}

func (eraserWaitsForever) Submit(ctx context.Context, _ store.WriteJob) error {
	<-ctx.Done()
	return ctx.Err()
}
