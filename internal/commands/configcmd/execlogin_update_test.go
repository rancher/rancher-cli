package configcmd

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// seedExecLoginEntry writes one of the exec plugin's own login entries,
// logged in through the file store, and returns its name and the
// directory.
func seedExecLoginEntry(t *testing.T) (string, string, config.CredStore) {
	t.Helper()
	dir, store := cmdtest.FilePolicyDir(t)
	name := config.ExecLoginEntryName("https://r.ex")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[name] = config.Server{URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin}
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: name, URL: "https://r.ex"}, "plugin-tok"))
	return name, dir, store
}

// TestUpdateServerRefusesAURLChangeOnAnExecLoginEntry asserts the refusal:
// a URL change on one of the exec plugin's own login entries is VALIDATION,
// exit 2, before the server is probed or anything is written, and its
// message names the commands that remove the entry, each valid as written.
func TestUpdateServerRefusesAURLChangeOnAnExecLoginEntry(t *testing.T) {
	name, dir, store := seedExecLoginEntry(t)
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	probed := false
	c := &configUpdateServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) {
		probed = true
		return "", nil
	}}

	_, err = c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://other.ex"}, Pos: []string{name}}, nil)
	got, exit := cmdtest.RenderedError(t, err)
	assert.Equal(t, clierr.CodeValidation, got.Code)
	assert.Equal(t, 2, exit)
	assert.False(t, got.Retryable)
	assert.Empty(t, got.Hint, "the remedy is two commands, the first only when the entry holds a login")
	assert.Equal(t,
		"'exec-login-r.ex' is one of the exec plugin's own login entries, so its URL cannot change; remove it with rancher auth logout 'exec-login-r.ex' if it holds a login, then rancher config delete-server 'exec-login-r.ex', and the exec plugin creates a new entry for the new URL",
		got.Message)
	assert.False(t, probed, "refused before the server is probed")

	after, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "nothing was written")
	tok, terr := store.GetToken(config.ServerRef{Name: name, URL: "https://r.ex"})
	require.NoError(t, terr)
	assert.Equal(t, "plugin-tok", tok, "the login is untouched")
}

// TestUpdateServerKeepsTheChangesAnExecLoginEntryCanTake asserts what stays
// allowed on one of the exec plugin's own login entries: a CA-only update,
// which keeps the login and the record that the plugin made the entry,
// and a --url that names the URL the entry already has.
func TestUpdateServerKeepsTheChangesAnExecLoginEntryCanTake(t *testing.T) {
	t.Run("a CA-only update", func(t *testing.T) {
		name, dir, store := seedExecLoginEntry(t)
		res, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem1}, Pos: []string{name}}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"ca"}, res.Data.(map[string]any)["changed"])
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.Equal(t, clierr.ProvenanceExecLogin, cfg.Servers[name].Provenance)
		tok, terr := store.GetToken(config.ServerRef{Name: name, URL: "https://r.ex"})
		require.NoError(t, terr)
		assert.Equal(t, "plugin-tok", tok)
	})

	t.Run("the URL it already has", func(t *testing.T) {
		name, dir, _ := seedExecLoginEntry(t)
		res, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://R.ex/"}, Pos: []string{name}}, nil)
		require.NoError(t, err)
		assert.Equal(t, true, res.Data.(map[string]any)["unchanged"])
	})
}
