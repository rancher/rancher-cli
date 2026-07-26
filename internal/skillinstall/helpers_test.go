package skillinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// skipIfNoSymlinks skips the test if symlinks cannot be created in the
// current environment. On Windows, creating a file symlink requires
// SeCreateSymbolicLinkPrivilege, which is unavailable to a non-elevated
// process without Developer Mode. On other platforms symlinks are always
// available and this function is a no-op.
func skipIfNoSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "probe-src")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Skip("cannot create test file for symlink probe")
	}
	if err := os.Symlink(src, filepath.Join(dir, "probe-link")); err != nil {
		t.Skip("symlink creation requires elevated privileges or Developer Mode on this Windows build")
	}
}
