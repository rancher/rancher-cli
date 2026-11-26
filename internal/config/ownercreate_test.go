package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// checkOnCreate wraps the three create hooks so that each file or directory
// made through them is checked the moment it exists, before the caller does
// anything with it: owner-only by mode on Linux and macOS, and on Windows an
// access list that grants the current user alone. It returns the paths it
// checked and restores the hooks when the test ends. The caller must not use
// t.Parallel(): the hooks are package-level seams.
//
// A create that sets the list after the file exists fails the check on
// Windows when the directory grants another user access.
func checkOnCreate(t *testing.T) *[]string {
	t.Helper()
	origTemp, origFile, origMkdir := createTempHook, createFileHook, mkdirDir
	t.Cleanup(func() { createTempHook, createFileHook, mkdirDir = origTemp, origFile, origMkdir })
	checked := &[]string{}
	check := func(path string) {
		t.Helper()
		testfs.RequireOwnerOnly(t, path)
		*checked = append(*checked, path)
	}
	createTempHook = func(root *os.Root, pattern string, perm os.FileMode) (*os.File, error) {
		f, err := origTemp(root, pattern, perm)
		if err == nil {
			check(f.Name())
		}
		return f, err
	}
	createFileHook = func(root *os.Root, name string, perm os.FileMode) (*os.File, error) {
		f, err := origFile(root, name, perm)
		if err == nil {
			check(f.Name())
		}
		return f, err
	}
	mkdirDir = func(root *os.Root, name string, perm os.FileMode, inheritable bool) error {
		err := origMkdir(root, name, perm, inheritable)
		if err == nil {
			check(filepath.Join(root.Name(), name))
		}
		return err
	}
	return checked
}

// sharedParent returns a new directory that, on Windows, grants Everyone read
// access to what is created in it, as a directory does that another local
// user can reach. On other systems it is an ordinary new directory.
func sharedParent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testfs.GrantEveryoneRead(t, dir)
	return dir
}

// TestAtomicWriteFileTempFileIsOwnerOnlyWhenItIsCreated asserts the temp file
// of an atomic write is owner-only as soon as it exists, in a directory that
// grants another user access, and that the written file holds the data.
func TestAtomicWriteFileTempFileIsOwnerOnlyWhenItIsCreated(t *testing.T) {
	dir := sharedParent(t)
	checked := checkOnCreate(t)
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, atomicWriteFile(path, []byte("new"), 0o600))
	require.Len(t, *checked, 1)
	assert.Equal(t, dir, filepath.Dir((*checked)[0]))
	assert.Contains(t, filepath.Base((*checked)[0]), ".config.yaml-")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	testfs.RequireOwnerOnly(t, path)
}

// TestWriteTransitionTempFileIsOwnerOnlyWhenItIsCreated asserts the same for
// the temp file of a store-change record.
func TestWriteTransitionTempFileIsOwnerOnlyWhenItIsCreated(t *testing.T) {
	dir := sharedParent(t)
	checked := checkOnCreate(t)
	require.NoError(t, writeTransition(dir, validManifest(t)))
	require.Len(t, *checked, 1)
	assert.Equal(t, dir, filepath.Dir((*checked)[0]))
	testfs.RequireOwnerOnly(t, TransitionPath(dir))
}

// TestWriteIdentityRecordCreatesOwnerOnlyDirectoryAndTempFile asserts the
// credential identity directory and the temp file of its first record are
// owner-only as soon as they exist, in a config directory that grants
// another user access.
func TestWriteIdentityRecordCreatesOwnerOnlyDirectoryAndTempFile(t *testing.T) {
	dir := sharedParent(t)
	checked := checkOnCreate(t)
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	require.Len(t, *checked, 2)
	assert.Equal(t, identityDirPath(dir), (*checked)[0])
	assert.Equal(t, identityDirPath(dir), filepath.Dir((*checked)[1]))
	testfs.RequireOwnerOnly(t, filepath.Join(identityDirPath(dir), identityFileName("prod")))
}

// TestEnsureDirCreatesAnOwnerOnlyDirectoryUnderASharedParent asserts the
// config directory is owner-only as soon as it exists, below a directory that
// grants another user access, and a parent EnsureDir had to create is not
// made through the owner-only create.
func TestEnsureDirCreatesAnOwnerOnlyDirectoryUnderASharedParent(t *testing.T) {
	parent := sharedParent(t)
	checked := checkOnCreate(t)
	dir := filepath.Join(parent, "cfg")
	require.NoError(t, EnsureDir(dir))
	assert.Equal(t, []string{dir}, *checked)

	deep := filepath.Join(parent, "a", "b", "cfg")
	require.NoError(t, EnsureDir(deep))
	assert.Equal(t, []string{dir, deep}, *checked)
}

// TestWriteIdentityRecordRepairsAnExistingDirectory asserts a credential
// identity directory that exists is not created again and still gets the
// owner-only call on every write.
func TestWriteIdentityRecordRepairsAnExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(identityDirPath(dir), 0o700))
	origHarden := hardenACLHook
	t.Cleanup(func() { hardenACLHook = origHarden })
	var hardened []string
	hardenACLHook = func(path string) error {
		hardened = append(hardened, path)
		return origHarden(path)
	}
	checked := checkOnCreate(t)
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-1")))
	require.NoError(t, writeIdentityRecord(dir, activeRecord(t, "prod", "token-2")))
	assert.Equal(t, []string{identityDirPath(dir), identityDirPath(dir)}, hardened)
	require.Len(t, *checked, 2, "the two temp files, no directory")
}
