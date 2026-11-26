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

// mkdirCall is one call of mkdirDir: the path of the directory made and
// whether its owner-only entry passes to new files and directories.
type mkdirCall struct {
	path        string
	inheritable bool
}

// recordMkdir replaces mkdirDir with a wrapper that records every call. With
// a nil result it then makes the directory as fsacl.Mkdir does; otherwise it
// makes nothing and returns result. It restores mkdirDir when the test ends.
// The caller must not use t.Parallel(): mkdirDir is a package-level seam.
func recordMkdir(t *testing.T, result error) *[]mkdirCall {
	t.Helper()
	orig := mkdirDir
	t.Cleanup(func() { mkdirDir = orig })
	calls := &[]mkdirCall{}
	mkdirDir = func(root *os.Root, name string, perm os.FileMode, inheritable bool) error {
		*calls = append(*calls, mkdirCall{filepath.Join(root.Name(), name), inheritable})
		if result != nil {
			return result
		}
		return orig(root, name, perm, inheritable)
	}
	return calls
}

// TestEnsureDirCreatesTheDirectoryWithOneOwnerOnlyCall asserts a missing
// directory is created by one call of the owner-only create, made inheritable,
// on the directory and never on a parent the call had to create.
func TestEnsureDirCreatesTheDirectoryWithOneOwnerOnlyCall(t *testing.T) {
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
			calls := recordMkdir(t, nil)
			require.NoError(t, EnsureDir(tc.path))
			require.DirExists(t, tc.want)
			assert.Equal(t, []mkdirCall{{tc.want, true}}, *calls)
		})
	}
}

// TestEnsureDirLeavesAnExistingDirectoryAlone asserts a directory that
// exists makes no create call and returns no error.
func TestEnsureDirLeavesAnExistingDirectoryAlone(t *testing.T) {
	dir := t.TempDir()
	calls := recordMkdir(t, nil)
	require.NoError(t, EnsureDir(dir))
	require.NoError(t, EnsureDir(dir+string(filepath.Separator)))
	assert.Empty(t, *calls)
}

// TestEnsureDirRefusesAFileInThePlace asserts a file where the directory
// belongs gives an error and no create call. The test asserts the
// error only: Linux and macOS return "not a directory", and the text on
// Windows is not compared.
func TestEnsureDirRefusesAFileInThePlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	calls := recordMkdir(t, nil)
	require.Error(t, EnsureDir(path))
	assert.Empty(t, *calls)
}

// TestEnsureDirRefusesAPathBelowAFile asserts a path below a plain file gives
// an error, no create call, and creates nothing. Linux and macOS fail
// the check of the directory (not a directory); Windows reads the path as
// not existing and fails the create of the parent. The error is checked for
// presence only: its text differs per system.
func TestEnsureDirRefusesAPathBelowAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	calls := recordMkdir(t, nil)
	err := EnsureDir(filepath.Join(file, "cfg"))
	require.Error(t, err)
	t.Logf("below a file: %v", err)
	assert.Empty(t, *calls)
	info, serr := os.Stat(file)
	require.NoError(t, serr)
	assert.True(t, info.Mode().IsRegular(), "the file must stay a file")
}

// TestEnsureDirRefusesAnEmptyName asserts an empty name returns the error
// os.MkdirAll returns for it, with no create call.
func TestEnsureDirRefusesAnEmptyName(t *testing.T) {
	calls := recordMkdir(t, nil)
	require.Error(t, EnsureDir(""))
	assert.Empty(t, *calls)
}

// TestEnsureDirAcceptsADirectoryAnotherProcessCreated asserts that when the
// create of the directory reports it exists, the call returns no error.
func TestEnsureDirAcceptsADirectoryAnotherProcessCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	orig := mkdirDir
	t.Cleanup(func() { mkdirDir = orig })
	mkdirDir = func(root *os.Root, name string, perm os.FileMode, inheritable bool) error {
		require.NoError(t, orig(root, name, perm, inheritable))
		return fs.ErrExist
	}
	require.NoError(t, EnsureDir(dir))
	require.DirExists(t, dir)
}

// TestEnsureDirReturnsTheErrorOfAFailedCreate asserts a failed create returns
// its error unchanged and leaves no directory behind.
func TestEnsureDirReturnsTheErrorOfAFailedCreate(t *testing.T) {
	errCreate := errors.New("create refused")
	dir := filepath.Join(t.TempDir(), "cfg")
	calls := recordMkdir(t, errCreate)
	require.ErrorIs(t, EnsureDir(dir), errCreate)
	assert.Equal(t, []mkdirCall{{dir, true}}, *calls)
	require.NoDirExists(t, dir)
}

// TestWriteRawConfigCreatesAnOwnerOnlyDirectory asserts a write into a
// missing directory creates it owner-only, through the real create call:
// mode 0700 on Linux and macOS, an access list for the current user only on
// Windows.
func TestWriteRawConfigCreatesAnOwnerOnlyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	require.NoError(t, WriteRawConfig(FilePath(dir), []byte("{}\n")))
	testfs.RequireOwnerOnly(t, dir)
	testfs.RequireOwnerOnly(t, FilePath(dir))
}
