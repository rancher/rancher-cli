//go:build unix

package skillinstall

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadTreeRejectsFIFO(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("x"), 0o644))
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))
	_, err := ReadTree(dir)
	require.ErrorIs(t, err, ErrNonRegular)
}
