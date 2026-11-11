package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// recordDirHook replaces hardenDirHook with a recorder that returns result
// and restores it when the test ends. The returned slice holds the path of
// every call. The caller must not use t.Parallel(): the hook is a
// package-level seam.
func recordDirHook(t *testing.T, result error) *[]string {
	t.Helper()
	orig := hardenDirHook
	t.Cleanup(func() { hardenDirHook = orig })
	calls := &[]string{}
	hardenDirHook = func(path string) error {
		*calls = append(*calls, path)
		return result
	}
	return calls
}

// TestEnsureDirCreatesAndSetsTheListOnce asserts a missing directory is
// created and the access list call runs once, on the directory and never on
// a parent the call had to create.
func TestEnsureDirCreatesAndSetsTheListOnce(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		path string
		want string
	}{
		{"missing directory", filepath.Join(root, "one"), filepath.Join(root, "one")},
		{"missing parents", filepath.Join(root, "a", "b", "two"), filepath.Join(root, "a", "b", "two")},
		{"trailing separator", filepath.Join(root, "three") + string(filepath.Separator), filepath.Join(root, "three")},
		{"trailing dot element", filepath.Join(root, "four") + string(filepath.Separator) + ".", filepath.Join(root, "four")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := recordDirHook(t, nil)
			require.NoError(t, EnsureDir(tc.path))
			require.DirExists(t, tc.want)
			assert.Equal(t, []string{tc.want}, *calls)
		})
	}
}

// TestEnsureDirLeavesAnExistingDirectoryAlone asserts a directory that
// exists makes no access list call and returns no error.
func TestEnsureDirLeavesAnExistingDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	calls := recordDirHook(t, nil)
	require.NoError(t, EnsureDir(dir))
	require.NoError(t, EnsureDir(dir+string(filepath.Separator)))
	assert.Empty(t, *calls)
}

// TestEnsureDirRefusesAFileInThePlace asserts a file where the directory
// belongs gives an error and no access list call. The test asserts the
// error only: Linux and macOS return "not a directory", and the text on
// Windows is not compared.
func TestEnsureDirRefusesAFileInThePlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	calls := recordDirHook(t, nil)
	require.Error(t, EnsureDir(path))
	assert.Empty(t, *calls)
}

// TestEnsureDirRefusesAPathBelowAFile asserts a path below a plain file gives
// an error, no access list call, and creates nothing. Linux and macOS fail
// the check of the directory (not a directory); Windows reads the path as
// not existing and fails the create of the parent. The error is checked for
// presence only: its text differs per system.
func TestEnsureDirRefusesAPathBelowAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	calls := recordDirHook(t, nil)
	err := EnsureDir(filepath.Join(file, "cfg"))
	require.Error(t, err)
	t.Logf("below a file: %v", err)
	assert.Empty(t, *calls)
	info, serr := os.Stat(file)
	require.NoError(t, serr)
	assert.True(t, info.Mode().IsRegular(), "the file must stay a file")
}

// TestEnsureDirRefusesAnEmptyName asserts an empty name returns the error
// os.MkdirAll returns for it, with no access list call.
func TestEnsureDirRefusesAnEmptyName(t *testing.T) {
	calls := recordDirHook(t, nil)
	require.Error(t, EnsureDir(""))
	assert.Empty(t, *calls)
}

// TestEnsureDirAcceptsADirectoryAnotherProcessCreated asserts that when the
// create of the directory reports it exists, the call returns no error and
// makes no access list call: only the process whose create succeeds sets
// the list.
func TestEnsureDirAcceptsADirectoryAnotherProcessCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	orig := mkdirDir
	t.Cleanup(func() { mkdirDir = orig })
	mkdirDir = func(name string, perm os.FileMode) error {
		require.NoError(t, orig(name, perm))
		return fs.ErrExist
	}
	calls := recordDirHook(t, nil)
	require.NoError(t, EnsureDir(dir))
	require.DirExists(t, dir)
	assert.Empty(t, *calls)
}

// TestEnsureDirRemovesTheDirectoryWhenTheListFails asserts a failed access
// list call returns an error that wraps the cause and names the directory,
// and leaves no directory behind.
func TestEnsureDirRemovesTheDirectoryWhenTheListFails(t *testing.T) {
	errList := errors.New("list refused")
	dir := filepath.Join(t.TempDir(), "cfg")
	calls := recordDirHook(t, errList)
	err := EnsureDir(dir)
	require.ErrorIs(t, err, errList)
	require.ErrorContains(t, err, "list refused")
	require.ErrorContains(t, err, dir)
	assert.Equal(t, []string{dir}, *calls)
	require.NoDirExists(t, dir)
}

// TestWriteRawConfigCreatesAnOwnerOnlyDirectory asserts a write into a
// missing directory creates it owner-only, through the real access list
// call: mode 0700 on Linux and macOS, an access list for the current user
// only on Windows.
func TestWriteRawConfigCreatesAnOwnerOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	require.NoError(t, WriteRawConfig(FilePath(dir), []byte("{}\n")))
	testfs.RequireOwnerOnly(t, dir)
	testfs.RequireOwnerOnly(t, FilePath(dir))
}
