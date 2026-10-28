package skillinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	assert.EqualError(t, err, `unknown harness "emacs" (valid: claude, codex, copilot, agents, all)`)
}

func TestAgentNames(t *testing.T) {
	assert.Equal(t, []string{"claude", "codex", "copilot", "agents", "all"}, AgentNames())
}

// TestTargetsUserScopeSymlinkedCodexHomeDedupsToOne proves that when
// CODEX_HOME is a symlink pointing at ~/.claude, Targets returns exactly one
// target labeled "claude" (the first row in table order) and Acquire succeeds
// on the deduplicated result without a self-conflict error.
func TestTargetsUserScopeSymlinkedCodexHomeDedupsToOne(t *testing.T) {
	skipIfNoSymlinks(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COPILOT_HOME", "")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))

	// CODEX_HOME is a symlink pointing at ~/.claude - same physical location,
	// different textual path. Both rows pass Detect(); without canonical dedup
	// Targets would return two entries that Acquire would then self-conflict.
	codexLink := filepath.Join(home, "codex-alias")
	require.NoError(t, os.Symlink(filepath.Join(home, ".claude"), codexLink))
	t.Setenv("CODEX_HOME", codexLink)

	targets, err := Targets("all", ScopeUser, "")
	require.NoError(t, err)
	require.Len(t, targets, 1, "symlinked CODEX_HOME must deduplicate to exactly one target")
	assert.Equal(t, "claude", targets[0].Harness, "first row (claude) must win the canonical identity")

	lockDir := t.TempDir()
	leases, err := Acquire(lockDir, targets, "install", false)
	require.NoError(t, err, "Acquire must succeed on the deduplicated single target")
	ReleaseAll(leases)
}

// TestTargetsUserScopeCaseAliasDedupsToOne proves that on case-insensitive
// filesystems (Darwin/APFS by default) a CODEX_HOME path that is a case
// variant of ~/.claude is deduplicated to a single target labeled "claude".
// Linux is case-sensitive so the alias does not exist there; Windows is
// excluded because symlink privilege detection is already covered by the
// sibling test above and TempDir returns a fixed case path.
func TestTargetsUserScopeCaseAliasDedupsToOne(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("case-alias dedup requires a case-insensitive filesystem (darwin)")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COPILOT_HOME", "")
	claudeDir := filepath.Join(home, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o755))

	// Upper-case variant of the same path - physically the same directory on
	// APFS but a different string. canonicalTarget must fold both to the same
	// canonical key so Targets deduplicates them.
	t.Setenv("CODEX_HOME", strings.ToUpper(claudeDir))

	targets, err := Targets("all", ScopeUser, "")
	require.NoError(t, err)
	require.Len(t, targets, 1, "case-aliased CODEX_HOME must deduplicate to exactly one target")
	assert.Equal(t, "claude", targets[0].Harness, "first row (claude) must win the canonical identity")
}
