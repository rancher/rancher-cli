package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogoutCompleteArgsReturnsServerNames(t *testing.T) {
	// No keyring.MockInit: logoutCmd.CompleteArgs calls view.ServerNames() only,
	// never touches keyring (round-2 finding 9: unnecessary keyring setup removed).
	t.Parallel()
	dir, _ := seedLogout(t, "acme", "acme", "prod")
	cmd := NewLogout(dir)
	sc, ok := cmd.(runtime.ShellCompleter)
	require.True(t, ok, "logoutCmd must implement ShellCompleter")
	got := sc.CompleteArgs(context.Background(), testView{srvNames: []string{"acme", "prod"}})
	assert.Equal(t, []string{"acme", "prod"}, got)
}
