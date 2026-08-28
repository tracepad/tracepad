package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The write jobs behind the admin API (spec 005). The HTTP tests cover what an
// operator sees; these cover the property an operator cannot see, which is that
// every destructive job decides for itself, from the row inside its own
// transaction, whether it is allowed to proceed (spec 003 Decision 20).

func TestShrinkingAWindowDemandsTheEchoFromInsideTheTransaction(t *testing.T) {
	f := newSweepFixture(t)

	// A handler that believed the window was 90 days would not ask for a
	// confirmation to set 30. The job asks anyway, because it reads the
	// stored row itself.
	update := &ProjectUpdate{
		ProjectID: f.project.ID,
		Retention: OptionalDays{Set: true, Value: days(30)},
	}
	err := f.writer.Submit(t.Context(), update)
	if err == nil {
		t.Fatal("a window shortened from forever to 30 days went through without an echo")
	}
	if !strings.Contains(err.Error(), `"test"`) {
		t.Errorf("err = %v, want it to name the project to echo", err)
	}
	if project, _ := f.store.ProjectByID(f.project.ID); project.RetentionDays != nil {
		t.Errorf("retention = %v after a refused update, want it untouched", project.RetentionDays)
	}

	update.Confirm = "test"
	if err := f.writer.Submit(t.Context(), update); err != nil {
		t.Fatalf("the confirmed update failed: %v", err)
	}
	if project, _ := f.store.ProjectByID(f.project.ID); *project.RetentionDays != 30 {
		t.Errorf("retention = %v, want 30", project.RetentionDays)
	}

	// Growing it back needs nothing: it destroys nothing.
	grow := &ProjectUpdate{
		ProjectID: f.project.ID,
		Retention: OptionalDays{Set: true, Value: days(60)},
	}
	if err := f.writer.Submit(t.Context(), grow); err != nil {
		t.Fatalf("growing the window asked for a confirmation: %v", err)
	}
}

// TestRawWindowShrinkIsMeasuredAgainstTheEffectiveOne: raw follows the trace
// window when it has none of its own (#6), so setting a raw window of 30 days
// under a 90-day trace window is a shrink even though the raw column was NULL.
func TestRawWindowShrinkIsMeasuredAgainstTheEffectiveOne(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(90), nil)

	project, err := f.store.ProjectByID(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	shorter := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true, Value: days(30)},
	}
	if !shorter.Shrinks(project) {
		t.Errorf("a 30-day raw window under a 90-day trace window is not seen as a shrink")
	}
	longer := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true, Value: days(180)},
	}
	if longer.Shrinks(project) {
		t.Errorf("a raw window longer than the trace window is seen as a shrink")
	}
	// Clearing raw back to "follow the traces" is not a shrink either.
	follow := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true},
	}
	if follow.Shrinks(project) {
		t.Errorf("clearing the raw window is seen as a shrink")
	}
}

// TestCreatingADeletedProjectsNameIsRefused: the name stays reserved through
// the grace window, so that restore always has its name to come back to (#9).
func TestCreatingADeletedProjectsNameIsRefused(t *testing.T) {
	f := newSweepFixture(t)

	deletion := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}

	create := &ProjectCreate{Name: "test", Keys: KeyPair{PublicKey: "tp-pk-2", Secret: "tp-sk-2"}}
	err := f.writer.Submit(t.Context(), create)
	if err == nil {
		t.Fatal("a deleted project's name was reused while it was still restorable")
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if !strings.Contains(rejection.Message, "restored") {
		t.Errorf("message = %q, want it to point at the restore", rejection.Message)
	}

	// Once it is restored, so is everything about it.
	if err := f.writer.Submit(t.Context(), &ProjectRestore{ProjectID: f.project.ID}); err != nil {
		t.Fatal(err)
	}
	project, _ := f.store.ProjectByID(f.project.ID)
	if project.Deleted() {
		t.Errorf("the project is still deleted after a restore")
	}
}

// TestDeletingATwiceDeletedProject: the second delete is a conflict naming the
// purge date, not a silent no-op that resets the grace window.
func TestDeletingATwiceDeletedProject(t *testing.T) {
	f := newSweepFixture(t)
	first := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test",
		Now: sweepNow.Add(time.Hour).UnixNano()}
	if err := f.writer.Submit(t.Context(), second); err == nil {
		t.Fatal("deleting a deleted project moved its purge date")
	}
	project, _ := f.store.ProjectByID(f.project.ID)
	if *project.DeletedAt != sweepNow.UnixNano() {
		t.Errorf("deleted_at = %d, want the first deletion's %d", *project.DeletedAt, sweepNow.UnixNano())
	}
}
