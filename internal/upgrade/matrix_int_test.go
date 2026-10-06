//go:build unix && upgradeint

package upgrade

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	// The cells are the process table's, as the gate's are; a real server
	// cannot be made slow, so its cells fail and interrupt.
	swapPoints, backPoints := pointsOf(t, kindProcess)
	exists := func(item string) bool {
		_, err := os.Stat(item)
		return err == nil
	}
	// invariants: a server answers, the old version or the new, with the
	// two traces sent before the upgrade; every copy set aside opens and
	// passes quick_check; nothing is deleted.
	invariants := func(t *testing.T, w *world, deps Deps, dir string, want ...string) {
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
	snapshot := func(w *world, deps Deps, dir string) func() string {
		return func() string {
			st, _ := loadState(dir)
			pid, _ := lockedBy(w.data)
			v, _ := health(context.Background(), deps.HTTP, w.url)
			beside, _ := filepath.Glob(w.data + ".*")
			return fmt.Sprintf("answers %s, pid %d, %d traces\nbeside the data %q\nset aside %q\nsteps %d", v, pid, w.count(), beside, st.SetAside, len(st.Steps))
		}
	}

	// The kill cells (#34), as the gate's: every step, killed between what
	// it records and the record, then --check and --back as a person runs
	// them.
	kill := func(t *testing.T, w *world, deps Deps, point string, args ...string) {
		t.Helper()
		c := &cell{point: point, kind: "kill"}
		deps.Fault = c.fault
		var out bytes.Buffer
		if _, dead := runKillable(context.Background(), Options{Args: append(args, "--json"), Version: vOld, Stdout: &out, Stderr: io.Discard}, deps); !dead || !c.hit.Load() {
			t.Fatalf("no run records %s: %s", point, out.String())
		}
	}
	recordSwap, recordBack := recordsOf(t, kindProcess)
	for _, point := range recordSwap {
		t.Run("upgrade/kill/"+point, func(t *testing.T) {
			t.Parallel()
			cells <- struct{}{}
			defer func() { <-cells }()
			w := newWorld(t)
			w.start()
			w.sendTrace(1)
			w.sendTrace(2)
			w.waitCount(2)
			deps := w.deps()
			kill(t, w, deps, point, "--to", vNew, "--data-dir", w.data)
			id, dir := runOf(t, deps)
			afterKill(t, deps, id, false)
			invariants(t, w, deps, dir, vOld)
			backAgain(t, deps, id, snapshot(w, deps, dir))
		})
	}
	for _, point := range recordBack {
		t.Run("back/kill/"+point, func(t *testing.T) {
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
			kill(t, w, deps, point, "--back", rep.Run.ID)
			afterKill(t, deps, rep.Run.ID, true)
			invariants(t, w, deps, rep.Run.Dir, vOld)
			backAgain(t, deps, rep.Run.ID, snapshot(w, deps, rep.Run.Dir))
		})
	}

	for _, kind := range []string{"fail", "interrupt"} {
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
				c := &cell{point: point, kind: kind, cancel: cancel}
				deps.Fault = c.fault
				rep, code := w.runCtx(ctx, deps, "--to", vNew, "--data-dir", w.data)
				deps.Fault = nil
				if !c.hit.Load() {
					t.Fatalf("no upgrade passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the upgrade did not finish: %d %s", point, code, rep.Summary)
				}
				if rep.Run == nil {
					invariants(t, w, deps, "", vOld)
					return
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, deps, rep.Run.Dir, vOld)
				backAgain(t, deps, rep.Run.ID, snapshot(w, deps, rep.Run.Dir))
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
				c := &cell{point: point, kind: kind, cancel: cancel}
				deps.Fault = c.fault
				_, code = w.runCtx(ctx, deps, "--back", rep.Run.ID)
				deps.Fault = nil
				if !c.hit.Load() {
					t.Fatalf("no way back passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, deps, rep.Run.Dir, vOld)
				backAgain(t, deps, rep.Run.ID, snapshot(w, deps, rep.Run.Dir))
			})
		}
	}
}
