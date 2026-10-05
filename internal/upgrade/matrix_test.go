//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The fault matrix (spec 054 #26). The steps of a swap and of a way back are
// not listed here: a healthy upgrade and its way back are run once with a
// Fault that records every point they pass, and each point recorded becomes
// two cells — the step failing there, and an interrupt arriving there. A new
// step marked with j.at gets its cells without anyone writing them. After
// the fault, --back is run as a person would, until it is done; then every
// cell holds the same invariants: a server runs, the old version or the new;
// the data is whole; nothing is deleted.

var errInjected = errors.New("injected")

// recorder is a Fault that records the points a run passes.
type recorder struct{ points []string }

func (r *recorder) fault(point string) error {
	if !slices.Contains(r.points, point) {
		r.points = append(r.points, point)
	}
	return nil
}

// faultAt fails, or interrupts, at one point.
func faultAt(point, kind string, cancel context.CancelFunc) func(string) error {
	return func(p string) error {
		if p != point {
			return nil
		}
		if kind == "interrupt" {
			cancel()
			return nil
		}
		return errInjected
	}
}

var kinds = []string{"fail", "interrupt"}

// runIn runs the command with a context, and decodes its report.
func runIn(t *testing.T, ctx context.Context, deps Deps, args ...string) (Report, int) {
	t.Helper()
	var out bytes.Buffer
	code := run(ctx, Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, deps)
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return rep, code
}

// backUntilDone runs --back as a person does after a run that did not end
// healthy, until it is done or says it already is: at most three times.
func backUntilDone(t *testing.T, deps Deps, run string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		rep, code := runIn(t, context.Background(), deps, "--back", run)
		if code == exitOK || strings.Contains(rep.Summary, "already ran") {
			return
		}
		t.Logf("--back %d: %d %s", i+1, code, rep.Summary)
	}
	t.Errorf("--back did not finish in three tries")
}

// nothingDeleted: the run's directory, its archive when it took one, and
// everything it set aside are where the run left them.
func nothingDeleted(t *testing.T, dir string, exists func(item string) bool) {
	t.Helper()
	st, err := loadState(dir)
	if err != nil {
		t.Fatalf("the run's state: %v", err)
	}
	if st.Archive != nil {
		if _, err := os.Stat(filepath.Join(dir, "data.tar.gz")); err != nil {
			t.Errorf("the archive is gone: %v", err)
		}
	}
	for _, item := range st.SetAside {
		if !exists(item) {
			t.Errorf("set aside, and gone: %s", item)
		}
	}
}

func TestTheFaultMatrixOfAContainer(t *testing.T) {
	// The points, recorded from a healthy upgrade and its way back.
	rec := &recorder{}
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	deps.Fault = rec.fault
	rep, code := runIn(t, context.Background(), deps)
	if code != exitOK {
		t.Fatalf("the healthy upgrade: %d %s", code, rep.Summary)
	}
	swapPoints := slices.Clone(rec.points)
	rec.points = nil
	if back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("the healthy way back: %d %s", code, back.Summary)
	}
	backPoints := slices.Clone(rec.points)
	if len(swapPoints) < 5 || len(backPoints) < 4 {
		t.Fatalf("too few points recorded: %q, %q", swapPoints, backPoints)
	}

	exists := func(d *fakeDocker) func(string) bool {
		return func(item string) bool {
			kind, name, _ := strings.Cut(item, " ")
			if kind == "volume" {
				_, ok := d.volumes[name]
				return ok
			}
			return d.byName[name] != nil
		}
	}
	// invariants: the container runs, the old image or the new, on a
	// volume that holds the traces it held; the fake fails any removal.
	invariants := func(t *testing.T, d *fakeDocker, dir string) {
		t.Helper()
		c := d.byName["tracepad-app"]
		if c == nil || !c.State.Running || c.State.Restarting {
			t.Fatalf("no container runs as tracepad-app: %+v", c)
		}
		if img := c.Config.Image; img != "ghcr.io/tracepad/tracepad:0.1.0" && img != "ghcr.io/tracepad/tracepad:0.2.0" {
			t.Errorf("it runs %s", img)
		}
		for _, m := range c.Mounts {
			if m.Destination == "/data" && d.volumes[m.Name] != 7 {
				t.Errorf("its volume %s holds %d traces, not 7", m.Name, d.volumes[m.Name])
			}
		}
		if dir != "" {
			nothingDeleted(t, dir, exists(d))
		}
	}

	for _, kind := range kinds {
		for _, point := range swapPoints {
			t.Run("upgrade/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				d := newFakeDocker(t)
				d.setupContainer(t, t.TempDir())
				deps := containerDeps(t, d)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				rep, code := runIn(t, ctx, deps)
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
				invariants(t, d, dir)
			})
		}
		for _, point := range backPoints {
			t.Run("back/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				d := newFakeDocker(t)
				d.setupContainer(t, t.TempDir())
				deps := containerDeps(t, d)
				rep, code := runIn(t, context.Background(), deps)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				_, code = runIn(t, ctx, deps, "--back", rep.Run.ID)
				deps.Fault = nil
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, d, rep.Run.Dir)
				if img := d.byName["tracepad-app"].Config.Image; img != "ghcr.io/tracepad/tracepad:0.1.0" {
					t.Errorf("after the way back, %s runs", img)
				}
			})
		}
	}
}

func TestTheFaultMatrixOfAServer(t *testing.T) {
	// The points, recorded from a healthy upgrade and its way back.
	rec := &recorder{}
	w := newFakeWorld(t, 2)
	deps := w.deps()
	deps.Fault = rec.fault
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("the healthy upgrade: %d %s", code, rep.Summary)
	}
	swapPoints := slices.Clone(rec.points)
	rec.points = nil
	if back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID); code != exitOK {
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
	// traces the data held before the upgrade; every copy set aside opens
	// and passes quick_check; nothing is deleted.
	invariants := func(t *testing.T, w *fakeWorld, dir string, traces int64, want ...string) {
		t.Helper()
		if v := w.answers(); !slices.Contains(want, v) {
			t.Fatalf("%s answers %q, not one of %q", w.url(), v, want)
		}
		if n, err := countTraces(filepath.Join(w.data, dataDBName)); err != nil || n != traces {
			t.Errorf("traces: %d (%v), not %d", n, err, traces)
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
				w := newFakeWorld(t, 2)
				deps := w.deps()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				rep, code := runIn(t, ctx, deps, "--to", fNew, "--data-dir", w.data)
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
				invariants(t, w, dir, 2, fOld, fNew)
			})
		}
		for _, point := range backPoints {
			t.Run("back/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				w := newFakeWorld(t, 2)
				deps := w.deps()
				rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				w.addTrace()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				deps.Fault = faultAt(point, kind, cancel)
				_, code = runIn(t, ctx, deps, "--back", rep.Run.ID)
				deps.Fault = nil
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, rep.Run.Dir, 2, fOld)
				// What the new version wrote is kept, set aside.
				if n, err := countTraces(filepath.Join(w.data+".after-"+rep.Run.ID, dataDBName)); err != nil || n != 3 {
					t.Errorf("set aside: %d traces (%v), not 3", n, err)
				}
			})
		}
	}
}
