package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultConfigDirIsHermetic asserts that a test in this package which
// sets no config directory of its own resolves to the temporary one
// TestMain created, never to the operator's real directory.
func TestDefaultConfigDirIsHermetic(t *testing.T) {
	dir, err := config.ResolveDir("")
	require.NoError(t, err)
	assert.Equal(t, os.Getenv(config.EnvConfigDir), dir)
	assert.True(t, strings.HasPrefix(dir, os.TempDir()), "%s is not under %s", dir, os.TempDir())
}
