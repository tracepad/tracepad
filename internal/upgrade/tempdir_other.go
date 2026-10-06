//go:build !darwin

package upgrade

// userTempDir: only macOS keeps one apart from TMPDIR.
func userTempDir() string { return "" }
