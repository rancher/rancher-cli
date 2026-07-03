package config

import (
	"os"
	"path/filepath"
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
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

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
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

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
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	})
}
