package configcmd

import (
	"errors"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedDeclaredServer writes a directory that declares one credential
// store and holds one entry with a token in the file store.
func seedDeclaredServer(t *testing.T, declared string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: declared,
		CurrentServer:   "acme",
		Servers:         map[string]config.Server{"acme": {URL: "https://acme.example"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"}, "tok"))
	return dir
}

// assertStoreConflict asserts the error is the declaration-against-variable
// refusal and not some other validation failure.
func assertStoreConflict(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "a typed error")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, config.EnvCredentialStore)
}

// TestDeleteServerRefusesAStoreConflict asserts a command that deletes a
// credential stops on a declaration the variable disagrees with, before
// it touches any backend: deleting from the store the environment names
// would leave the credential the directory declares untouched and live.
func TestDeleteServerRefusesAStoreConflict(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "keychain")

	cmd := &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	assertStoreConflict(t, err)

	tok, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok", tok, "nothing was deleted")
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Contains(t, after.Servers, "acme", "the entry stays: the delete never ran")
}

// TestUpdateServerRefusesAStoreConflict asserts the same for the credential
// clearing a URL change forces.
func TestUpdateServerRefusesAStoreConflict(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "keychain")

	cmd := &configUpdateServerCmd{dir: dir}
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Pos: []string{"acme"}, Str: map[string]string{"url": "https://moved.example"}},
		ui.Discard())
	assertStoreConflict(t, err)

	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "https://acme.example", after.Servers["acme"].URL, "config.yaml is untouched")
}

// TestConfigStatusReportsAStoreConflictWithoutFailing asserts the one
// command that never opens a store shows both answers instead: the
// operator needs to see what the directory says and what the environment
// asked for to fix it.
func TestConfigStatusReportsAStoreConflictWithoutFailing(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "keychain")

	data := execConfigInfo(t, dir)
	assert.Equal(t, "file", data["credentialStore"])
	conflict, ok := data["credentialStoreConflict"].(string)
	require.True(t, ok)
	assert.Contains(t, conflict, "keychain")
}

// TestConfigStatusReportsADamagedBindingStamp asserts the one report of
// a state that costs the directory every warm exec-credential hit and
// changes nothing else. Without it the directory looks healthy: commands
// keep working, the plugin keeps returning credentials, and each one is
// derived afresh.
func TestConfigStatusReportsADamagedBindingStamp(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "")

	healthy := execConfigInfo(t, dir)
	_, reported := healthy["cacheBindingStamp"]
	assert.False(t, reported, "a readable stamp is not worth a line")

	require.NoError(t, os.WriteFile(config.BindingStampPath(dir), []byte("not hex"), 0o600))

	damaged := execConfigInfo(t, dir)
	line, ok := damaged["cacheBindingStamp"].(string)
	require.True(t, ok, "a damaged stamp is reported")
	assert.Contains(t, line, "unreadable")
	assert.Equal(t, "file", damaged["credentialStore"], "and the command still answers")
}

// TestConfigStatusReportsAStampThatDisagreesWithTheConfig asserts the
// other half of the same report. A config write that died between its two
// steps leaves a readable stamp file holding a value config.yaml does not
// carry, and from then on no reader's two halves line up: every cached
// credential is a miss and every invocation pays a reload to find that
// out, indefinitely. Nothing else in the directory misbehaves, so this is
// the only place the state is visible.
func TestConfigStatusReportsAStampThatDisagreesWithTheConfig(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.NotEmpty(t, cfg.BindingStamp, "the fixture has written one")

	// The stamp file moved; config.yaml did not.
	require.NoError(t, os.WriteFile(config.BindingStampPath(dir),
		[]byte("0123456789abcdef0123456789abcdef\n"), 0o600))

	out := execConfigInfo(t, dir)
	line, ok := out["cacheBindingStamp"].(string)
	require.True(t, ok, "the disagreement is reported")
	assert.Contains(t, line, "disagrees with config.yaml")
	assert.Equal(t, "file", out["credentialStore"], "and the command still answers")
}

// TestDeleteServerAdvancesTheBindingStamp asserts the command that
// removes an entry advances the directory's binding stamp. A cache record
// naming a removed entry is already a miss by the entry rule, but a
// reader that loaded the config before the removal would still be holding
// the entry and would serve the record; the stamp is what tells it its
// load was overtaken.
func TestDeleteServerAdvancesTheBindingStamp(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	t.Setenv(config.EnvCredentialStore, "")
	before, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.NotEmpty(t, before.BindingStamp)

	cmd := &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
	_, derr := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, derr)

	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.NotEqual(t, before.BindingStamp, after.BindingStamp)
	onDisk, _ := config.ReadBindingStamp(dir)
	assert.Equal(t, after.BindingStamp, onDisk, "the file and the config agree")
}

// TestUseServerAdvancesTheBindingStamp asserts the pointer move advances
// the stamp. The current server decides which entry the exec plugin reads
// for a kubeconfig that names none, so a cached kubectl credential of such
// a call is judged against it, and a reader whose config load came before
// the move must see the stamp change and judge the record again.
func TestUseServerAdvancesTheBindingStamp(t *testing.T) {
	dir := seedDeclaredServer(t, "file")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["other"] = config.Server{URL: "https://other.example"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	before, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)

	runUseServer(t, dir, "other")

	after, aerr := config.Load(config.FilePath(dir))
	require.NoError(t, aerr)
	assert.Equal(t, "other", after.CurrentServer)
	assert.NotEqual(t, before.BindingStamp, after.BindingStamp)
	onDisk, _ := config.ReadBindingStamp(dir)
	assert.Equal(t, after.BindingStamp, onDisk, "the file and the config agree")
}

// TestAddServerAdvancesTheBindingStamp asserts registering an entry
// advances the stamp. An exec-credential cache record made while a server
// had no entry is a hit only while that stays true; a reader holding the
// old no-entry config would keep serving it after the entry exists, and
// the stamp is what makes that load detectably stale.
func TestAddServerAdvancesTheBindingStamp(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	before, _ := config.ReadBindingStamp(dir)

	c := newAddServerCmd(dir)
	_, aerr := c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, aerr)

	after, _ := config.ReadBindingStamp(dir)
	assert.NotEqual(t, before, after)
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, after, cfg.BindingStamp)
}
