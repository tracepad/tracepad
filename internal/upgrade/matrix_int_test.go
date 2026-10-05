//go:build unix && upgradeint

package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The fault matrix again, on real servers built from this tree (spec 054
// #27): what a fake cannot stand for — a real migration, a real server's own
// lock and shutdown. Not in the gate: make upgrade-integration.

// cells is how many server cells run at once: each starts real servers, and
// a machine running all of them together measures its own load, not the
// command.
var cells = make(chan struct{}, 4)

func TestTheFaultMatrixOfARealServer(t *testing.T) {
	builtBinaries(t)
	// The points, recorded from a healthy upgrade and its way back.
	rec := &recorder{}
	w := newWorld(t)
	w.start()
	deps := w.deps()
	deps.Fault = rec.fault
	rep, code := w.run(deps, "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("the healthy upgrade: %d %s", code, rep.Summary)
	}
	swapPoints := slices.Clone(rec.points)
	rec.points = nil
	if back, code := w.run(deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("the healthy way back: %d %s", code, back.Summary)
	}
	backPoints := slices.Clone(rec.points)
	if len(swapPoints) < 5 || len(backPoints) < 7 {
		t.Fatalf("too few points recorded: %q, %q", swapPoints, backPoints)
	}

	exists := func(item string) bool {
		_, err := os.Stat(item)
		return err == nil
	}
	// invariants: a server answers, the old version or the new, with the
	// two traces sent before the upgrade; every copy set aside opens and
	// passes quick_check; nothing is deleted.
	invariants := func(t *testing.T, w *world, dir string, want ...string) {
		t.Helper()
		answered := ""
		for i := 0; i < 200 && !slices.Contains(want, answered); i++ {
			answered, _ = health(context.Background(), deps.HTTP, w.url)
			if !slices.Contains(want, answered) {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if !slices.Contains(want, answered) {
			t.Fatalf("%s answers %q, not one of %q", w.url, answered, want)
		}
		if got := w.count(); got != 2 {
			t.Errorf("traces: %d, not 2", got)
		}
		afters, _ := filepath.Glob(w.data + ".after-*")
		for _, a := range afters {
			if err := quickCheck(context.Background(), filepath.Join(a, dataDBName)); err != nil {
				t.Errorf("set aside, and not whole: %v", err)
			}
		}
		if dir != "" {
			nothingDeleted(t, dir, exists)
		}
	}

	for _, kind := range kinds {
		for _, point := range swapPoints {
			t.Run("upgrade/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				cells <- struct{}{}
				defer func() { <-cells }()
				w := newWorld(t)
				w.start()
				w.sendTrace(1)
				w.sendTrace(2)
				w.waitCount(2)
				deps := w.deps()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				rep, code := w.runCtx(ctx, deps, "--to", vNew, "--data-dir", w.data)
				deps.Fault = nil
				dir := ""
				if rep.Run != nil {
					dir = rep.Run.Dir
					if code != exitOK {
						backUntilDone(t, deps, rep.Run.ID)
					}
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the upgrade did not finish: %d %s", point, code, rep.Summary)
				}
				invariants(t, w, dir, vOld, vNew)
			})
		}
		for _, point := range backPoints {
			t.Run("back/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				cells <- struct{}{}
				defer func() { <-cells }()
				w := newWorld(t)
				w.start()
				w.sendTrace(1)
				w.sendTrace(2)
				w.waitCount(2)
				deps := w.deps()
				rep, code := w.run(deps, "--to", vNew, "--data-dir", w.data)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				w.sendTrace(3)
				w.waitCount(3)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				_, code = w.runCtx(ctx, deps, "--back", rep.Run.ID)
				deps.Fault = nil
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, rep.Run.Dir, vOld)
			})
		}
	}
}
