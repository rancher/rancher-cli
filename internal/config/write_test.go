package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/fsacl"
)

func TestAtomicWriteFile(t *testing.T) {
	t.Run("enforces permissions on existing loose file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "loose.yaml")

		require.NoError(t, os.WriteFile(path, []byte("old"), 0644))

		err := atomicWriteFile(path, []byte("new"), 0600)
		require.NoError(t, err)

		info, err := os.Stat(path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			// On Windows os.FileInfo.Mode() synthesizes mode bits regardless
			// of the real NTFS DACL (the security descriptor the file is created
			// with is the enforcement there), so the 0600 assertion is meaningless
			// on that platform.
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		}

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "new", string(b))
	})

	t.Run("refuses to write through symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.yaml")
		link := filepath.Join(dir, "link.yaml")

		require.NoError(t, os.WriteFile(target, []byte("target-content"), 0600))
		require.NoError(t, os.Symlink(target, link))

		err := atomicWriteFile(link, []byte("attack"), 0600)
		require.Error(t, err)
		require.ErrorContains(t, err, "refusing to write through symlink")

		b, err := os.ReadFile(target)
		require.NoError(t, err)
		assert.Equal(t, "target-content", string(b), "target should not be modified")
	})

	t.Run("creates new file with correct permissions", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "new.yaml")

		err := atomicWriteFile(path, []byte("fresh"), 0600)
		require.NoError(t, err)

		info, err := os.Stat(path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			// On Windows os.FileInfo.Mode() synthesizes mode bits regardless
			// of the real NTFS DACL (the security descriptor the file is created
			// with is the enforcement there), so the 0600 assertion is meaningless
			// on that platform.
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		}

		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "fresh", string(b))
	})

	t.Run("creates parent directory if missing", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nested", "deep", "file.yaml")

		err := atomicWriteFile(path, []byte("deep"), 0600)
		require.NoError(t, err)

		info, err := os.Stat(path)
		require.NoError(t, err)
		if runtime.GOOS != "windows" {
			// On Windows os.FileInfo.Mode() synthesizes mode bits regardless
			// of the real NTFS DACL (the security descriptor the file is created
			// with is the enforcement there), so the 0600 assertion is meaningless
			// on that platform.
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		}
	})

	// A failure after the create must close the temp file before it removes
	// it, or the removal silently fails on Windows (the handle shares no
	// delete access with the removal) and leaks the temp file. The seam
	// returns the created file's name reopened for reading only, with that
	// handle still open: the mode change fails on Windows (no access to write
	// attributes) and the write fails on Linux and macOS. Close-before-remove
	// is correct on every platform; the test makes the ordering observable.
	t.Run("a failure after the create leaves no temp file behind", func(t *testing.T) {
		orig := createTempHook
		createTempHook = func(root *os.Root, pattern string, perm os.FileMode) (*os.File, error) {
			f, err := orig(root, pattern, perm)
			if err != nil {
				return nil, err
			}
			name := f.Name()
			require.NoError(t, f.Close())
			return os.Open(name)
		}
		t.Cleanup(func() { createTempHook = orig })

		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")

		require.Error(t, atomicWriteFile(path, []byte("new"), 0600))

		entries, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		assert.Empty(t, entries, "no stray temp file must remain after a failure past the create")
	})

	t.Run("a failed create leaves the target untouched", func(t *testing.T) {
		orig := createTempHook
		createTempHook = func(*os.Root, string, os.FileMode) (*os.File, error) { return nil, fmt.Errorf("boom") }
		t.Cleanup(func() { createTempHook = orig })

		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("old"), 0600))

		err := atomicWriteFile(path, []byte("new"), 0600)
		require.ErrorContains(t, err, "boom")

		b, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, "old", string(b))
		entries, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		assert.Len(t, entries, 1, "only the target remains")
	})
}

// TestHardenHookIsTheSharedACLCall asserts the hook the identity directory's
// repair goes through is the shared owner-only access list call, so moving the
// code did not leave a second copy behind.
func TestHardenHookIsTheSharedACLCall(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(fsacl.Harden).Pointer(), reflect.ValueOf(hardenACLHook).Pointer())
}

// TestCreateHooksAreTheSharedCreateCalls asserts the hooks every temp file
// and new directory of this package is created through are the shared create
// calls that set the owner-only access list with the create.
func TestCreateHooksAreTheSharedCreateCalls(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(fsacl.CreateTemp).Pointer(), reflect.ValueOf(createTempHook).Pointer())
	assert.Equal(t, reflect.ValueOf(fsacl.CreateFile).Pointer(), reflect.ValueOf(createFileHook).Pointer())
	assert.Equal(t, reflect.ValueOf(fsacl.Mkdir).Pointer(), reflect.ValueOf(mkdirDir).Pointer())
}
