package skills

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

// TestEmbeddedSkillMatchesDisk asserts the embedded tree equals the
// rancher/ directory on disk: complete relative-path and byte equality,
// both directions.
func TestEmbeddedSkillMatchesDisk(t *testing.T) {
	embedded := treeMap(t, Rancher())
	disk := treeMap(t, os.DirFS("rancher"))
	assert.Equal(t, disk, embedded)
}
