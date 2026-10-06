//go:build linux

package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A binary written a moment ago is run while other goroutines start
// children: a child between its fork and its exec holds a copy of every
// descriptor, the new file's write descriptor included, and Linux refuses
// to execute a file open for writing (ETXTBSY). The fault matrix's parallel
// cells met it on CI. putInPlace must not fail of it.
func TestABinaryJustWrittenRunsWhileOthersFork(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	scriptBinary(t, src, "1.2.3")
	r := &runner{deps: Deps{Version: binaryVersion}}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				dst := filepath.Join(dir, fmt.Sprintf("d%d-%d", g, i), "tracepad")
				_ = os.MkdirAll(filepath.Dir(dst), 0o755)
				if err := r.putInPlace(context.Background(), src, dst, "1.2.3"); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
