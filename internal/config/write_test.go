package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			// of the real NTFS DACL (hardenACL is the enforcement there), so
			// the 0600 assertion is meaningless on that platform.
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
		assert.Contains(t, err.Error(), "refusing to write through symlink")

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
			// of the real NTFS DACL (hardenACL is the enforcement there), so
			// the 0600 assertion is meaningless on that platform.
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
			// of the real NTFS DACL (hardenACL is the enforcement there), so
			// the 0600 assertion is meaningless on that platform.
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		}
	})

	// TestAtomicWriteFileHardenACLFailureLeavesNoTempFile exercises the
	// hardenACL failure path via hardenACLHook: the temp file must be
	// closed before it is removed, or the removal silently fails on
	// Windows (no FILE_SHARE_DELETE by default) and leaks the temp file.
	// Close-before-remove is correct on every platform; this test just
	// makes the ordering observable without a real Windows host.
	t.Run("hardenACL failure leaves no temp file behind", func(t *testing.T) {
		orig := hardenACLHook
		hardenACLHook = func(string) error { return fmt.Errorf("boom") }
		t.Cleanup(func() { hardenACLHook = orig })

		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")

		err := atomicWriteFile(path, []byte("new"), 0600)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "boom")

		entries, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		assert.Empty(t, entries, "no stray temp file must remain after a hardenACL failure")
	})
}
