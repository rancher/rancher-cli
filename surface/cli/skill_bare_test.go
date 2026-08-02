package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestTokenEntryIsMarkedBare pins that the top-level "token" entry has
// bare: true, so adaptEntry never calls resolveDeps before legacyTokenCmd.Execute
// and a broken config.yaml cannot block a warm exec-credential cache hit.
func TestTokenEntryIsMarkedBare(t *testing.T) {
	t.Parallel()
	for _, e := range buildEntries() {
		d := e.make(runDeps{}).Describe()
		if d.Name == "token" {
			assert.True(t, e.bare, "the 'token' entry must have bare: true")
			return
		}
	}
	t.Fatal("'token' entry not found in buildEntries")
}

// TestSkillEntriesAreMarkedBare pins that every entry in the "skill" group has
// bare: true, so adaptEntry never calls resolveDeps on their action path.
func TestSkillEntriesAreMarkedBare(t *testing.T) {
	t.Parallel()
	for _, e := range buildEntries() {
		if e.group != "skill" {
			continue
		}
		name := e.make(runDeps{}).Describe().Name
		assert.True(t, e.bare, "skill entry %q must have bare: true", name)
	}
}

// TestSkillStatusRunsWithInvalidConfig proves that skill status completes and
// produces a result envelope even when the Rancher config path resolves to a
// regular file — a shape that would cause resolveDeps to fail with "not a
// directory". The bare adapter path must never reach resolveDeps.
func TestSkillStatusRunsWithInvalidConfig(t *testing.T) {
	// Sandbox HOME and cache dirs so no ambient harness or lock state leaks.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LocalAppData", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("COPILOT_HOME", "")

	// Invalid config dir: a regular file — resolveDeps would fail with an
	// OS-level "not a directory" error when building the config file path.
	badCfg := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badCfg, []byte("dummy"), 0o600))
	t.Setenv(config.EnvConfigDir, badCfg)

	orig := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = orig }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	// skill status with no harness dirs in the sandboxed home: exit 0,
	// empty data, one warning. If resolveDeps ran on the invalid config the
	// action would fail silently before any output was produced.
	runErr := root.Run(context.Background(), []string{"rancher", "skill", "status", "-o", "json"})
	require.NoError(t, runErr, "skill status must complete without error even with an invalid Rancher config path")

	var env struct {
		Data     []any    `json:"data"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env),
		"output must be a valid JSON envelope; got %q", out.String())
	assert.Empty(t, env.Data, "no harnesses detected in sandboxed home")
	assert.NotEmpty(t, env.Warnings, "empty-home skill status must warn about no harnesses detected")
}
