package main

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func treeMap(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	m := map[string]string{}
	require.NoError(t, fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, path)
		require.NoError(t, err)
		m[path] = string(b)
		return nil
	}))
	return m
}

// TestEmbeddedSkillMatchesDisk pins embed == skills/rancher/: complete
// relative-path and byte equality, both directions.
func TestEmbeddedSkillMatchesDisk(t *testing.T) {
	sub, err := fs.Sub(skillFS, "skills/rancher")
	require.NoError(t, err)
	embedded := treeMap(t, sub)
	disk := treeMap(t, os.DirFS("skills/rancher"))
	assert.Equal(t, disk, embedded)
}
