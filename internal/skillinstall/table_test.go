package skillinstall

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTargetsUserScopeDetectsOnlyPresentHarnesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("CODEX_HOME", "")
	t.Setenv("COPILOT_HOME", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".agents"), 0o755))

	got, err := Targets("all", ScopeUser, "")
	require.NoError(t, err)
	var names []string
	for _, tg := range got {
		names = append(names, tg.Harness)
	}
	assert.ElementsMatch(t, []string{"claude", "agents"}, names)
	for _, tg := range got {
		assert.True(t, filepath.IsAbs(tg.Dir))
		assert.Equal(t, "rancher", filepath.Base(tg.Dir))
		assert.Equal(t, "skills", filepath.Base(filepath.Dir(tg.Dir)))
	}
}

func TestTargetsCodexHomeOverride(t *testing.T) {
	home, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	got, err := Targets("codex", ScopeUser, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, filepath.Join(codexHome, "skills", "rancher"), got[0].Dir)
}

func TestTargetsProjectScopeDedupsCopilotIntoAgents(t *testing.T) {
	cwd := t.TempDir()
	got, err := Targets("all", ScopeProject, cwd)
	require.NoError(t, err)
	var names []string
	for _, tg := range got {
		names = append(names, tg.Harness)
	}
	assert.Equal(t, []string{"claude", "codex", "agents"}, names)
}

func TestTargetsCopilotProjectAliasesAgentsDir(t *testing.T) {
	cwd := t.TempDir()
	got, err := Targets("copilot", ScopeProject, cwd)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "agents", got[0].Harness)
	assert.Equal(t, filepath.Join(cwd, ".agents", "skills", "rancher"), got[0].Dir)
}

func TestTargetsUnknownAgent(t *testing.T) {
	_, err := Targets("emacs", ScopeUser, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown agent")
}

func TestAgentNames(t *testing.T) {
	assert.Equal(t, []string{"claude", "codex", "copilot", "agents", "all"}, AgentNames())
}
