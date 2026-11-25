package skillinstall

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nestedSource is the fixture of testSource plus one file in a subdirectory.
func nestedSource(t *testing.T) fstest.MapFS {
	t.Helper()
	src := testSource(t)
	src["references/x.md"] = &fstest.MapFile{Data: []byte("nested\n")}
	return src
}

// TestInstallStatusAndUninstallHandleAFileInASubdirectory asserts that an
// install writes a file below a subdirectory, that status reports a changed
// and a missing nested file as modified, and that an uninstall removes the
// subdirectory with the rest.
func TestInstallStatusAndUninstallHandleAFileInASubdirectory(t *testing.T) {
	SetSource(nestedSource(t))
	tg := targetIn(t)
	nested := filepath.Join(tg.Dir, "references", "x.md")

	install(t, tg, "v1.0.0")
	got, err := os.ReadFile(nested)
	require.NoError(t, err)
	assert.Equal(t, "nested\n", string(got))
	verdict, _, err := StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictInSync, verdict)

	require.NoError(t, os.WriteFile(nested, []byte("edited\n"), 0o644))
	verdict, _, err = StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictModified, verdict, "a changed nested file")

	install(t, tg, "v1.0.0")
	require.NoError(t, os.Remove(nested))
	verdict, _, err = StatusTarget(tg, "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, VerdictModified, verdict, "a missing nested file")

	install(t, tg, "v1.0.0")
	got, err = os.ReadFile(nested)
	require.NoError(t, err)
	assert.Equal(t, "nested\n", string(got), "an install restores the nested file")

	leases, err := Acquire(testLockDir(tg), []Target{tg}, "uninstall", false)
	require.NoError(t, err)
	removed, _, err := leases[0].Uninstall()
	ReleaseAll(leases)
	require.NoError(t, err)
	assert.True(t, removed)
	assert.NoDirExists(t, filepath.Join(tg.Dir, "references"))
	assert.NoDirExists(t, tg.Dir)
}
