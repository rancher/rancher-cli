package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/runtime"
)

func TestLogoutCompleteArgsReturnsServerNames(t *testing.T) {
	// No keyring.MockInit: logoutCmd.CompleteArgs calls view.ServerNames() only,
	// never touches keyring.
	t.Parallel()
	dir, _ := seedLogout(t, "acme", "acme", "prod")
	cmd := NewLogout(dir)
	sc, ok := cmd.(runtime.ShellCompleter)
	require.True(t, ok, "logoutCmd must implement ShellCompleter")
	got := sc.CompleteArgs(t.Context(), cmdtest.View{Servers: []string{"acme", "prod"}}, nil)
	assert.Equal(t, []string{"acme", "prod"}, got)
}
