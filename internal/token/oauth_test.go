package token

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallbackPortLegacyEnvFallback(t *testing.T) {
	t.Setenv(config.EnvOAuthCallbackPort, "")
	t.Setenv(config.EnvLegacyOAuthCallbackPort, "18080")
	port, err := getCallbackPort(ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, 18080, port)
}

func TestCallbackPortNewEnvWinsOverLegacy(t *testing.T) {
	t.Setenv(config.EnvOAuthCallbackPort, "18081")
	t.Setenv(config.EnvLegacyOAuthCallbackPort, "18080")
	port, err := getCallbackPort(ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, 18081, port)
}

// TestNativeGetTokenHonorsCattleOAuthAuthFlow is the fix-2 missing native-path
// case: with only CATTLE_OAUTH_AUTH_FLOW set (no flag, no
// RANCHER_CLI_OAUTH_AUTH_FLOW), the OAuth auth-flow resolver must pick it up.
// Before fix 2 resolveAuthFlow didn't exist; after fix 2 it is the single
// source of truth for both the native path and the shim.
func TestNativeGetTokenHonorsCattleOAuthAuthFlow(t *testing.T) {
	t.Setenv(config.EnvOAuthAuthFlow, "")
	t.Setenv(config.EnvLegacyOAuthAuthFlow, "authcode")
	assert.Equal(t, "authcode", resolveAuthFlow(""))
}
