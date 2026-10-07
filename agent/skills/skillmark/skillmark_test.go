package skillmark

import (
	"os"
	"path/filepath"
	"testing"
)

// A file where a directory of the path would be — a project's `.claude`
// file — holds no copy, as a missing directory holds none: never a copy that
// could not be read (the second review of #232).
func TestAFileOnThePathIsNoCopy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".claude"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{filepath.Join(root, ".claude", "skills", Name), filepath.Join(root, "none", Name)} {
		if ok, v, err := Read(dir); ok || v != "" || err != nil {
			t.Errorf("%s: %v %q %v", dir, ok, v, err)
		}
	}
}
