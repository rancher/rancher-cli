package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestExecLoginURLChangeRemedyRunsAsWritten asserts, through the real CLI,
// that the commands a refused URL change on one of the exec plugin's own
// login entries names run as written and remove the entry and its login.
func TestExecLoginURLChangeRemedyRunsAsWritten(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	name := config.ExecLoginEntryName("https://r.ex")
	cfg := &config.Config{CredentialStore: "file", CurrentServer: name,
		Servers: map[string]config.Server{name: {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	ref := config.ServerRef{Name: name, URL: "https://r.ex"}
	require.NoError(t, config.NewFileStore(dir).SetToken(ref, "plugin-tok"))
	t.Setenv(config.EnvConfigDir, dir)

	exit, stdout, stderr := runRootJSON(t, "config", "update-server", name, "--url", "https://other.ex")
	require.Equal(t, 2, exit, "stdout: %s\nstderr: %s", stdout, stderr)
	got := cmdtest.DecodeErrorEnvelope(t, []byte(strings.TrimSpace(stderr)))
	assert.Equal(t, clierr.CodeValidation, got.Code)
	logout := "rancher auth logout " + clierr.ShellQuote(name)
	deleteServer := "rancher config delete-server " + clierr.ShellQuote(name)
	require.Contains(t, got.Message, logout)
	require.Contains(t, got.Message, deleteServer)

	for _, args := range [][]string{
		{"auth", "logout", name},
		{"config", "delete-server", name},
	} {
		exit, stdout, stderr = runRootJSON(t, args...)
		require.Equal(t, 0, exit, "%v\nstdout: %s\nstderr: %s", args, stdout, stderr)
	}
	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, after.Servers, name, "the entry is gone")
	_, terr := config.NewFileStore(dir).GetToken(ref)
	assert.ErrorIs(t, terr, config.ErrNoToken, "and so is its login")
}
