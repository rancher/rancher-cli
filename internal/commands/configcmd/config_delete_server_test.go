package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// seedDeleteServer sets up a temp config dir with a real FileStore and
// in-memory keychain (keyring.MockInit). current is the CurrentServer pointer;
// servers lists server names to populate.
func seedDeleteServer(t *testing.T, current string, servers []string) (string, config.CredStore) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: current,
		Servers:       map[string]config.Server{},
	}
	for _, s := range servers {
		cfg.Servers[s] = config.Server{URL: "https://" + s + ".ex"}
		require.NoError(t, store.SetToken(config.ServerRef{Name: s}, "id:sec"))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestConfigDeleteServerDeleteFailureChangesOnlyTheIdAndThePause(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme", "other"})
	path := config.FilePath(dir)
	before, err := config.Load(path)
	require.NoError(t, err)

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	deleteErr := errors.New("keychain locked")
	cmd.delete = func(_ context.Context, dir string, ref config.ServerRef, policy string) ([]string, []string, []string, error) {
		return nil, nil, nil, deleteErr
	}

	_, err = cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	require.ErrorContains(t, err, "keychain locked")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	got, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server entry must still be present after a failed delete (benign failure direction: creds gone, server still configured - never the reverse)")
	assert.Equal(t, "acme", cfg.CurrentServer, "current server must be untouched too")
	assert.NotEqual(t, before.Servers["acme"].CacheBindingID, got.CacheBindingID, "the first write advanced the id")
	assert.Equal(t, config.PauseLocked, got.CacheWritesPaused, "and paused the entry's cache writes")
	want := before.Servers["acme"]
	want.CacheBindingID, want.CacheWritesPaused = got.CacheBindingID, got.CacheWritesPaused
	assert.Equal(t, want, got, "and nothing else about the entry moved")
	assert.Equal(t, before.Servers["other"], cfg.Servers["other"])
}

func TestConfigDeleteServerClearsCurrentServerWhenDeletedServerWasCurrent(t *testing.T) {
	dir, store := seedDeleteServer(t, "acme", []string{"acme", "other"})

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "DeletedServer", res.Kind)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "acme", data["server"])
	assert.Equal(t, "https://acme.ex", data["url"])
	assert.True(t, data["currentServerCleared"].(bool), "current server pointer cleared when the deleted server was current")
	assert.Equal(t, map[string][]string{"backend": {"file"}}, data["deletedFrom"])
	assert.Equal(t, "server acme deleted\n"+
		"  was current - pointer cleared\n"+
		"  credentials removed (file)\n", res.Text)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok = cfg.Servers["acme"]
	assert.False(t, ok, "server entry removed")
	_, ok = cfg.Servers["other"]
	assert.True(t, ok, "unrelated server kept")
	assert.Empty(t, cfg.CurrentServer, "current server pointer cleared")

	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.ErrorIs(t, err, config.ErrNoToken, "token deleted")
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.NoError(t, err, "unrelated server's token kept")
}

func TestConfigDeleteServerDoesNotClearCurrentServerWhenDifferentServerDeleted(t *testing.T) {
	dir, _ := seedDeleteServer(t, "other", []string{"acme", "other"})

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.False(t, data["currentServerCleared"].(bool), "current server (\"other\") is untouched when a different server is deleted")
	assert.Equal(t, "server acme deleted\n  credentials removed (file)\n", res.Text)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "other", cfg.CurrentServer)
}

func TestConfigDeleteServerUnknownNameIsNotFoundWithCandidates(t *testing.T) {
	dir, _ := seedDeleteServer(t, "", []string{"acme"})

	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"nope"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	ids := make([]string, 0, len(ce.Candidates))
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Contains(t, ids, "acme")
}

func TestConfigDeleteServerDeletesExecCredentialCache(t *testing.T) {
	dir, store := seedDeleteServer(t, "", []string{"acme"})
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok", time.Time{}, "kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, data["deletedCache"])
	assert.Equal(t, "server acme deleted\n"+
		"  credentials removed (file)\n"+
		"  1 cached kubectl credentials removed\n", res.Text)

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), cmdtest.CacheAnchor(t, dir))
	assert.ErrorIs(t, err, config.ErrNoToken)
}

// TestConfigDeleteServerWritesNoCredstorePin is SUPPLEMENTARY coverage of
// the delete's own store-independence: it bypasses resolveDeps
// by construction, so it passes whichever way resolveDeps opens the store -
// the real resolveDeps regression lives in internal/cli's through-the-root
// tests. This one asserts that the command itself never
// resolves the injected store.
func TestConfigDeleteServerWritesNoCredstorePin(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	dir, _ := seedDeleteServer(t, "acme", []string{"acme"})

	_, err := NewConfigDeleteServer(dir).(*configDeleteServerCmd).Execute(
		t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)

	_, statErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(statErr), "delete must not write a credstore pin")
}

func TestConfigDeleteServerErasesThroughHelper(t *testing.T) {
	dir, statePath := cmdtest.SeedExecLogout(t, "acme", "acme")
	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, cmdtest.HelperCreds(t, statePath), "helper-held credential erased before config removal")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.False(t, ok, "server entry removed after a successful delete")
}

func TestConfigDeleteServerFailedEraseBlocksRemoval(t *testing.T) {
	dir, statePath := cmdtest.SeedExecLogout(t, "acme", "acme")
	t.Setenv("FAKEHELPER_MODE", "fail") // helper now refuses to erase
	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	assert.NotEmpty(t, cmdtest.HelperCreds(t, statePath), "credential still held by the helper")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "failed delete leaves the server config untouched")
}

// TestConfigDeleteServerCompleteArgsDelegatesToServerNames proves only the
// wiring: sort order is a ConfigView-level fact, asserted once at
// newConfigView in internal/cli.
func TestConfigDeleteServerCompleteArgsDelegatesToServerNames(t *testing.T) {
	t.Parallel()
	view := cmdtest.View{Servers: []string{"acme"}}
	got := configDeleteServerCmd{}.CompleteArgs(t.Context(), view, nil)
	assert.Equal(t, []string{"acme"}, got)
}

// TestDeleteServerCancellationPassesThrough asserts, for
// config delete-server, that a cancellation surfacing from the delete must pass
// through unchanged, not be rewrapped as a generic delete failure.
func TestDeleteServerCancellationPassesThrough(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"acme": {URL: "https://acme.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := &configDeleteServerCmd{dir: dir, delete: func(context.Context, string, config.ServerRef, string) ([]string, []string, []string, error) {
		return nil, nil, nil, fmt.Errorf("records lock: %w", context.Canceled)
	}}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "the server entry is left in place")

	cfgAfter, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	_, ok := cfgAfter.Servers["acme"]
	assert.True(t, ok, "the server entry must be untouched after a cancelled delete")
}

// TestConfigDeleteServerDeleteLockTimeoutSurvives asserts that
// a classified LOCK_TIMEOUT from the delete
// keeps its code, retryability, and exit status through the
// "the server entry is left in place" wrap.
func TestConfigDeleteServerDeleteLockTimeoutSurvives(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme"})

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, []string, error) {
		return nil, nil, nil, clierr.Retryable(clierr.CodeLockTimeout,
			"locking the credential-helper records (another rancher process may be logging in/out): context deadline exceeded")
	}

	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 8, ce.ExitCode())
	assert.Contains(t, ce.Message, "the server entry is left in place")
}

// TestConfigDeleteServerCarriesNoticeAheadOfLaterStepFailure asserts the
// merge-order-under-failure scenario: a notice recorded by an earlier,
// successful step (a keychain-unreachable degrade during the token delete)
// rides the error produced when a LATER step (the exec-credential cache
// delete) fails - the pipeline discards the command's Result on error, so
// WithWarnings is the notice's only ride. Default format prints the warning
// ahead of the error text; json/yaml carry it in the same error envelope's
// top-level warnings field, never inside data.error.
func TestConfigDeleteServerCarriesNoticeAheadOfLaterStepFailure(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme"})

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	cacheErr := errors.New("exec-credential cache: read token-cache.yaml: disk full")
	cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, []string, error) {
		return []string{"file"}, nil,
			[]string{"keychain unreachable, deleted the file store only"},
			cacheErr
	}

	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, []string{"keychain unreachable, deleted the file store only"}, ce.Warnings())

	var textBuf, jsonBuf, yamlBuf bytes.Buffer
	require.NoError(t, runtime.RenderError(&textBuf, "", ce))
	require.NoError(t, runtime.RenderError(&jsonBuf, "json", ce))
	require.NoError(t, runtime.RenderError(&yamlBuf, "yaml", ce))

	assert.Equal(t,
		"# warning: keychain unreachable, deleted the file store only\n"+
			"error (GENERIC): failed to delete credentials for \"acme\"; the server entry is left in place, with only its cache binding id advanced and its cache writes paused: "+cacheErr.Error()+"\n",
		textBuf.String(), "the warning must print ahead of the error text on the default format")

	var jsonEnv struct {
		Warnings []string `json:"warnings"`
		Data     struct {
			Error map[string]any `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(jsonBuf.Bytes(), &jsonEnv))
	assert.Equal(t, []string{"keychain unreachable, deleted the file store only"}, jsonEnv.Warnings)
	assert.NotContains(t, jsonEnv.Data.Error, "warnings", "a warning must never appear under data.error")

	var yamlEnv struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, yaml.Unmarshal(yamlBuf.Bytes(), &yamlEnv))
	assert.Equal(t, []string{"keychain unreachable, deleted the file store only"}, yamlEnv.Warnings)
}

// TestConfigDeleteServerReturnsRecoveryForCLIOwnedTokensThenRemoves asserts
// the result names the recovery sequence for the current token and every
// record, in the same invocation that removes the entry; an entry without
// CLI-owned tokens carries no recovery key; a failed credential delete
// still leaves the entry (today's rule).
func TestConfigDeleteServerReturnsRecoveryForCLIOwnedTokensThenRemoves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin,
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live"}, TokenUserID: "user-a",
			SupersededTokens: []config.SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, UserID: "user-b", SupersededAt: "2026-09-01T00:00:00Z"}}},
		"plain": {URL: "https://p.ex"},
		"noref": {URL: "https://n.ex", Provenance: clierr.ProvenanceDerived},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	cmd := &configDeleteServerCmd{dir: dir, delete: func(context.Context, string, config.ServerRef, string) ([]string, []string, []string, error) {
		return []string{"file"}, nil, nil, nil
	}}
	res, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"exec-login-r.ex"}}, ui.Discard())
	require.NoError(t, err)
	rec := res.Data.(map[string]any)["recovery"].(map[string]any)
	cur := rec["current"].(map[string]any)
	assert.Equal(t, "token-live", cur["ref"].(map[string]any)["name"])
	assert.Equal(t, "user-a", cur["userId"])
	assert.Equal(t, "rancher delete token token-live --apply --yes", cur["recovery"].([]string)[2])
	sup := rec["superseded"].([]map[string]any)
	require.Len(t, sup, 1)
	assert.Equal(t, "token-old", sup[0]["ref"].(map[string]any)["name"])
	assert.Contains(t, res.Text, "rancher auth login 'recover-r.ex' --url 'https://r.ex'   # log in as user 'user-b'")
	assert.Contains(t, res.Text, clierr.RecoverySequenceAssumption)
	cfg, _ = config.Load(config.FilePath(dir))
	_, present := cfg.Servers["exec-login-r.ex"]
	assert.False(t, present, "the entry is removed in the same invocation")

	res, err = cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"plain"}}, ui.Discard())
	require.NoError(t, err)
	_, has := res.Data.(map[string]any)["recovery"]
	assert.False(t, has)

	res, err = cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"noref"}}, ui.Discard())
	require.NoError(t, err)
	_, has = res.Data.(map[string]any)["recovery"]
	assert.False(t, has, "an entry that names no token has nothing to recover")
}

// TestConfigDeleteServerRunsFromConfigRecoveryCommandAndNamesAnUnwritableDir
// asserts the recovery a failed rollback hands the operator: the emitted
// command removes the entry the rollback left behind, and against an
// unwritable config directory it fails naming a path in that directory.
func TestConfigDeleteServerRunsFromConfigRecoveryCommandAndNamesAnUnwritableDir(t *testing.T) {
	if goruntime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not deny writes here")
	}
	dir, store := cmdtest.FilePolicyDir(t)
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "rollback" {
			return errors.New("read-only")
		}
		return nil
	})
	out, err := config.PersistLoginCredential(t.Context(), dir, config.PersistLoginInput{
		Name: "exec-login-r.ex", CollisionSuffix: true, CanonicalURL: "https://r.ex",
		Provenance: clierr.ProvenanceExecLogin, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"},
		Bearer: "ext/t:s", OwnsEntry: true, ReadOnlyStoreIsFailure: true,
		OpenStore: func(string, string) (config.CredStore, string, error) {
			return cmdtest.SetErrStore{CredStore: store, SetErr: errors.New("refused")}, "", nil
		},
	})
	restore()
	require.Error(t, err)
	require.NotNil(t, out.ConfigRecovery)
	name := strings.Trim(strings.TrimPrefix(clierr.ExpandRemedies(out.ConfigRecovery.Command, ""), "rancher config delete-server "), "'")
	cmd := NewConfigDeleteServer(dir).(runtime.ReadCommand)
	_, err = cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{name}}, ui.Discard())
	require.NoError(t, err)
	cfg, _ := config.Load(config.FilePath(dir))
	_, present := cfg.Servers[name]
	assert.False(t, present, "the emitted command removes the entry")

	cfg.Servers["exec-login-r.ex"] = config.Server{URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin}
	require.NoError(t, cfg.Write())
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err = cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"exec-login-r.ex"}}, ui.Discard())
	require.ErrorContains(t, err, dir, "the failure names the directory it could not write")
}

// TestDeleteServerClearsTheWayForAStoreChangeFromACreateOnlyHelper
// asserts the remedy a store change away from a credential helper names.
// A helper that creates credentials on demand stores nothing a logout can
// remove and keeps answering for the entry afterwards, so the change is
// refused while any other entry is configured; removing that entry with
// `config delete-server` works against such a helper - its erase succeeds
// on a credential it never stored - and after it the change goes on.
func TestDeleteServerClearsTheWayForAStoreChangeFromACreateOnlyHelper(t *testing.T) {
	for _, mode := range []string{"unsupported", "expiresat"} {
		t.Run(mode, func(t *testing.T) {
			dir, _ := cmdtest.SeedExecLogout(t, "prod", "prod", "staging")
			t.Setenv("FAKEHELPER_MODE", mode)
			t.Setenv("FAKEHELPER_EXPIRESAT", time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			_, err = config.LoginStoreChange(dir, cfg, "prod", "keychain", "keychain")
			require.ErrorContains(t, err, "rancher config delete-server <name>")

			_, err = NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(),
				cmdtest.Input{Pos: []string{"staging"}}, ui.Discard())
			require.NoError(t, err, "the helper's erase succeeds on a credential it never stored")

			cfg, err = config.Load(config.FilePath(dir))
			require.NoError(t, err)
			_, ok := cfg.Servers["staging"]
			require.False(t, ok, "the entry is gone")
			_, err = config.LoginStoreChange(dir, cfg, "prod", "keychain", "keychain")
			require.NoError(t, err, "with no other entry the change is not refused")
		})
	}
}
