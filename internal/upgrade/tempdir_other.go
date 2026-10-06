//go:build !darwin

package upgrade

// systemUserTempDir: only macOS keeps one apart from TMPDIR.
func systemUserTempDir() string { return "" }
