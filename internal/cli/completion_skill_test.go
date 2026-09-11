package cli_test

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestSkillAgentFlagCompletion(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvConfigDir, t.TempDir())

	got := runNameLikeCompletion(t, []string{"skill", "install", "--agent"}, "")
	tokens := extractTokens(got)
	assert.ElementsMatch(t, []string{"claude", "codex", "copilot", "agents", "all"}, tokens)
}

func TestSkillScopeFlagCompletion(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvConfigDir, t.TempDir())

	got := runNameLikeCompletion(t, []string{"skill", "status", "--scope"}, "")
	tokens := extractTokens(got)
	assert.ElementsMatch(t, []string{"user", "project"}, tokens)
}
