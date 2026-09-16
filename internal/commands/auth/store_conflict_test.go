package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declareStore points a seeded directory at one credential store, so a
// variable naming another one is a conflict rather than a fresh
// directory's choice.
func declareStore(t *testing.T, dir, declared string) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialStore = declared
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// TestLogoutRefusesAStoreConflict asserts a logout stops on a declaration
// the variable disagrees with. Deleting through the store the environment
// names would report success while the credential the directory declares
// stays live, which is the opposite of what logout promises.
func TestLogoutRefusesAStoreConflict(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	declareStore(t, dir, "file")
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"}, "tok-acme"))
	t.Setenv(config.EnvCredentialStore, "keychain")

	cmd := &logoutCmd{dir: dir}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, config.EnvCredentialStore)

	tok, gerr := store.GetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok-acme", tok, "nothing was deleted")
}

// TestAuthCheckFailsOnAStoreConflict asserts `auth status` answers like
// every other command that reads a credential: the read happens inside
// Effective, and the conflict stops it there.
func TestAuthCheckFailsOnAStoreConflict(t *testing.T) {
	dir := seedAuthedDir(t)
	declareStore(t, dir, "file")
	t.Setenv(config.EnvCredentialStore, "keychain")

	store, serr := config.NewLazyStore("file", dir)
	require.NoError(t, serr)
	cmd := &authCheckCmd{
		dir:       dir,
		store:     store,
		preflight: func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil },
	}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.Error(t, err)
	require.ErrorContains(t, err, config.EnvCredentialStore)
}

// TestAuthCheckReportsAStoreConflictUnderAnEnvToken asserts the one path
// that reaches the report: RANCHER_CLI_TOKEN short-circuits the store, so
// the command has an answer to give and names the conflict in it rather
// than failing on a store it never opened.
func TestAuthCheckReportsAStoreConflictUnderAnEnvToken(t *testing.T) {
	dir := seedAuthedDir(t)
	declareStore(t, dir, "file")
	t.Setenv(config.EnvCredentialStore, "keychain")
	t.Setenv(config.EnvToken, "id:secret")

	store, serr := config.NewLazyStore("file", dir)
	require.NoError(t, serr)
	cmd := &authCheckCmd{
		dir:       dir,
		store:     store,
		preflight: func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil },
	}
	res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	cs, ok := data["credentialStore"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "file", cs["policy"], "the directory's own answer")
	conflict, ok := cs["conflict"].(string)
	require.True(t, ok, "the conflict is reported")
	assert.Contains(t, conflict, "keychain")
	assert.Contains(t, res.Text, "conflict: ", "the text view names the conflict beside the policy")
	assert.Contains(t, res.Text, "keychain")
}

// TestLoginAdvancesTheBindingStamp asserts the login's own config writes
// advance the directory's stamp: the entry write creates an entry, which
// changes the answer to the question an empty-entry cache record is a hit
// against, so a reader that loaded the config before it must not keep
// serving one.
func TestLoginAdvancesTheBindingStamp(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	before, _ := config.ReadBindingStamp(dir)

	runLogin(t, dir, "acme", "https://acme.example")

	after, _ := config.ReadBindingStamp(dir)
	assert.NotEqual(t, before, after)
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, after, cfg.BindingStamp, "the file and the config agree after the login")
}
