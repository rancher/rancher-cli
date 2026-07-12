package commands

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContextNamesSortedOrder and the corrupt/missing case below pin the
// completion output order and degrade-to-empty contracts ONCE at the
// contextNames level (external review findings 8+9): contextUseCmd's own
// CompleteArgs test only needs to prove it delegates, not re-prove these
// same facts under a different receiver type.
func TestContextNamesSortedOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Contexts: map[string]config.Context{
			"zebra-ctx": {}, "alpha-ctx": {}, "mid-ctx": {},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	assert.Equal(t, []string{"alpha-ctx", "mid-ctx", "zebra-ctx"}, contextNames(dir))
}

func TestContextNamesEmptyOnMissingOrCorruptConfig(t *testing.T) {
	missing := t.TempDir()
	assert.Empty(t, contextNames(missing))

	corrupt := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(corrupt), []byte("not: [valid yaml\n"), 0o600))
	assert.Empty(t, contextNames(corrupt))
}

// TestServerNamesSortedOrder and the corrupt/missing case below pin the same
// contracts ONCE at the serverNames level — shared by config use-server AND
// config delete-server, so per-command tests only need to prove delegation.
func TestServerNamesSortedOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"zebra-srv": {}, "alpha-srv": {}, "mid-srv": {},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	assert.Equal(t, []string{"alpha-srv", "mid-srv", "zebra-srv"}, serverNames(dir))
}

func TestServerNamesEmptyOnMissingOrCorruptConfig(t *testing.T) {
	missing := t.TempDir()
	assert.Empty(t, serverNames(missing))

	corrupt := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(corrupt), []byte("not: [valid yaml\n"), 0o600))
	assert.Empty(t, serverNames(corrupt))
}
