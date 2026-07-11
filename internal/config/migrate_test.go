package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleLegacy = `{
  "CurrentServer": "rancherDefault",
  "Servers": {
    "rancherDefault": {
      "accessKey": "token-abc",
      "secretKey": "sekret",
      "tokenKey": "token-abc:sekret",
      "url": "https://legacy.ex",
      "project": "c-1:p-1",
      "cacert": "PEMDATA"
    }
  }
}`

func TestLegacyPathsAlwaysIncludesDotRancher(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Run("env unset", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "")
		paths := LegacyPaths()
		assert.Contains(t, paths, filepath.Join(home, ".rancher", "cli2.json"))
	})
	t.Run("env set adds env path but keeps ~/.rancher", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "/custom")
		paths := LegacyPaths()
		assert.Equal(t, "/custom/cli2.json", paths[0])
		assert.Contains(t, paths, filepath.Join(home, ".rancher", "cli2.json"))
	})
}

func TestMigrateImportsServerAndToken(t *testing.T) {
	legacyDir := t.TempDir()
	legacyPath := filepath.Join(legacyDir, "cli2.json")
	require.NoError(t, os.WriteFile(legacyPath, []byte(sampleLegacy), 0600))

	newDir := t.TempDir()
	store := NewFileStore(newDir)

	cfg, err := Migrate(legacyPath, newDir, store)
	require.NoError(t, err)
	assert.Equal(t, "rancherDefault", cfg.CurrentContext)
	assert.Equal(t, "https://legacy.ex", cfg.Servers["rancherDefault"].URL)
	assert.Equal(t, "c-1", cfg.Contexts["rancherDefault"].Cluster)
	assert.Equal(t, "p-1", cfg.Contexts["rancherDefault"].Project)

	tok, err := store.GetToken("rancherDefault")
	require.NoError(t, err)
	assert.Equal(t, "token-abc:sekret", tok)
}

func TestPurgeLegacyRemovesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cli2.json")
	require.NoError(t, os.WriteFile(p, []byte("{}"), 0600))
	require.NoError(t, PurgeLegacy(p))
	_, err := os.Stat(p)
	assert.True(t, os.IsNotExist(err))
}
