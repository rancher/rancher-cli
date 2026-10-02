package cmdtest

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailOnNewFilesNamesWhatATestLeft asserts the TestMain guard keeps a
// package's exit code when its tests left nothing in the working
// directory, and fails it, naming every path, when they left a file or a
// directory.
func TestFailOnNewFilesNamesWhatATestLeft(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("kept.txt", nil, 0o600))
	since := NewFiles()
	var w bytes.Buffer
	assert.Equal(t, 0, FailOnNewFiles(0, since, &w))
	assert.Equal(t, 3, FailOnNewFiles(3, since, &w), "a failing package keeps its code")
	assert.Empty(t, w.String())

	require.NoError(t, os.WriteFile("rancher-support-1.tar.gz", nil, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join("out", "x"), 0o700))
	added, err := since()
	require.NoError(t, err)
	assert.Equal(t, []string{"out", filepath.Join("out", "x"), "rancher-support-1.tar.gz"}, added)
	assert.Equal(t, 1, FailOnNewFiles(0, since, &w))
	assert.Contains(t, w.String(), "tests left files in the package directory")
	assert.Contains(t, w.String(), "rancher-support-1.tar.gz")
}

// TestFailOnNewFilesFailsWhenTheDirectoryCannotBeListed asserts a walk
// error fails the package instead of reading as "nothing was left".
func TestFailOnNewFilesFailsWhenTheDirectoryCannotBeListed(t *testing.T) {
	switch {
	case runtime.GOOS == "windows":
		t.Skip("on Windows os.Chmod only sets the read-only attribute, which does not stop listing a directory")
	case os.Geteuid() == 0:
		t.Skip("root reads a directory whatever its mode")
	}
	t.Chdir(t.TempDir())
	require.NoError(t, os.Mkdir("locked", 0o700))
	since := NewFiles()
	require.NoError(t, os.Chmod("locked", 0))
	t.Cleanup(func() { _ = os.Chmod("locked", 0o700) })
	_, err := since()
	require.Error(t, err)
	var w bytes.Buffer
	assert.Equal(t, 1, FailOnNewFiles(0, since, &w))
	assert.Contains(t, w.String(), "could not list the package directory")
}
