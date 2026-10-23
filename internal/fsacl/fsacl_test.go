package fsacl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHardenAcceptsAFileAndADirectory asserts both calls succeed on an
// existing path on every platform and leave the path usable by its owner.
func TestHardenAcceptsAFileAndADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.NoError(t, Harden(file))

	sub := filepath.Join(dir, "d")
	require.NoError(t, os.Mkdir(sub, 0o700))
	require.NoError(t, HardenInheritable(sub))

	inner := filepath.Join(sub, "g")
	require.NoError(t, os.WriteFile(inner, []byte("y"), 0o600))
	got, err := os.ReadFile(inner)
	require.NoError(t, err)
	require.Equal(t, "y", string(got))
}
