package configcmd

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// pausedEntryDir is a file-store directory whose entry prod carries every
// field an entry can, with its cache writes paused by a change that has
// not finished, beside a second entry.
func pausedEntryDir(t *testing.T) (string, config.Server) {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	prod := config.Server{
		URL: "https://prod.example.com", CA: cmdtest.CAPem1, CAFingerprint: "f0",
		Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"},
		TokenUserID: "u-1", CredentialExpiresAt: "2026-10-01T00:00:00Z", LoggedOutAt: "2026-09-01T00:00:00Z",
		SupersededTokens: []config.SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "old"}, SupersededAt: "2026-09-01T00:00:00Z"}},
		CacheBindingID:   "p1", CacheWritesPaused: config.PauseLocked,
	}
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": prod, "staging": {URL: "https://staging.example.com", CacheBindingID: "s1"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, prod
}

// TestAConfigWriteThatChangesNoCredentialKeepsThePause asserts one row per
// config writer that changes no credential: each loads the entry and
// writes it back, so the pause a change left is still there afterwards,
// with every other field the writer does not own.
func TestAConfigWriteThatChangesNoCredentialKeepsThePause(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir string)
		// want is prod as the writer leaves it.
		want func(prod config.Server) config.Server
	}{
		{name: "config use-server", run: func(t *testing.T, dir string) {
			t.Helper()
			_, err := NewServerSwitch(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"staging"}}, ui.Discard())
			require.NoError(t, err)
		}},
		{name: "config set-token-reuse", run: func(t *testing.T, dir string) {
			t.Helper()
			_, err := NewSetTokenReuse(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"on"}}, ui.Discard())
			require.NoError(t, err)
		}},
		{name: "config set-completion", run: func(t *testing.T, dir string) {
			t.Helper()
			_, err := NewSetCompletion(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"network": "true"}}, ui.Discard())
			require.NoError(t, err)
		}},
		{name: "config update-server with a new CA only", run: func(t *testing.T, dir string) {
			t.Helper()
			_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem2}, Pos: []string{"prod"}}, nil)
			require.NoError(t, err)
		}, want: func(p config.Server) config.Server {
			p.CA, p.CAFingerprint = cmdtest.CAPem2, ""
			return p
		}},
		{name: "the retirement pass's record write", run: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, config.UpdateServerEntry(t.Context(), dir, "prod", func(s *config.Server) bool {
				s.SupersededTokens = nil
				return true
			}))
		}, want: func(p config.Server) config.Server {
			p.SupersededTokens = nil
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, prod := pausedEntryDir(t)
			tc.run(t, dir)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			want := prod
			if tc.want != nil {
				want = tc.want(prod)
			}
			assert.Equal(t, want, cfg.Servers["prod"])
			assert.Equal(t, config.PauseLocked, cfg.Servers["prod"].CacheWritesPaused)
		})
	}
}

// TestConfigEditRefusesAChangeToACachePause asserts the one config writer
// that could clear a pause by hand: an edit that changes an entry in any
// way, its pause included, is refused.
func TestConfigEditRefusesAChangeToACachePause(t *testing.T) {
	t.Setenv("EDITOR", "fake-editor")
	dir := t.TempDir()
	seedConfigFile(t, dir, "currentServer: prod\nservers:\n  prod:\n    url: https://prod.example.com\n    cacheBindingId: p1\n    cacheWritesPaused: locked\n")

	edited := "currentServer: prod\nservers:\n  prod:\n    url: https://prod.example.com\n    cacheBindingId: p1\n"
	_, err := (&configEditCmd{dir: dir, launchEditor: writesContent(edited)}).Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "servers.prod changed")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, config.PauseLocked, cfg.Servers["prod"].CacheWritesPaused)
}

// TestAURLChangeKeepsItsPauseWhileAPinStepWouldMoveReaders asserts that
// `config update-server --url`, a change of the entry's credential, ends
// with the entry paused when the directory's store change has a pin step
// left that would move readers: its entry write resumes only what a
// resumer may. The command's own prelude retries the change first, so the
// pin step stays only behind a delete the keychain cannot run.
func TestAURLChangeKeepsItsPauseWhileAPinStepWouldMoveReaders(t *testing.T) {
	dir, store := cmdtest.FilePolicyDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://old.example.com")
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa1", URL: "https://old.example.com"}, "tok"))
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialStore = "auto"
	require.NoError(t, cfg.Write())
	require.NoError(t, config.WritePin(dir, "file"))
	t.Cleanup(config.SetKeyringBackendForTest(cmdtest.UnreachableKeyring{}))
	cmdtest.WriteChangeRecord(t, dir, true,
		cmdtest.ChangeStep{Kind: "delete", Entry: "gone", Source: "keychain", Token: "gone-old", SourceAccountID: "0123456789abcdef0123456789abcdef"},
		cmdtest.ChangeStep{Kind: "pin", Target: "keychain"})

	_, err = newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://new.example.com", after.Servers["qa1"].URL)
	assert.Equal(t, config.PauseLocked, after.Servers["qa1"].CacheWritesPaused, "the pin step left keeps the pause")
}

// TestADeleteServerPausesInItsFirstWriteAndResumesTheOthersInItsLast
// asserts delete-server's writes: its first advances the entry's id and
// pauses it with the credential still there, a failure of the write that
// removes the entry leaves it paused, and that write resumes every other
// entry a resumer may.
func TestADeleteServerPausesInItsFirstWriteAndResumesTheOthersInItsLast(t *testing.T) {
	seed := func(t *testing.T) string {
		t.Helper()
		dir, _ := seedDeleteServer(t, "acme", []string{"acme", "other"})
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		o := cfg.Servers["other"]
		o.CacheWritesPaused = config.PauseLocked
		cfg.Servers["other"] = o
		require.NoError(t, cfg.Write())
		return dir
	}
	t.Run("the first write fails", func(t *testing.T) {
		dir := seed(t)
		t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
			if step == "cachebinding" {
				return errors.New("config directory is full")
			}
			return nil
		}))
		_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
		require.ErrorContains(t, err, "config directory is full")
		tok, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme"})
		require.NoError(t, gerr)
		assert.Equal(t, "id:sec", tok, "nothing was deleted")
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.False(t, cfg.Servers["acme"].Paused())
	})
	t.Run("the write that removes the entry fails", func(t *testing.T) {
		dir := seed(t)
		var atFirst string
		t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
			if step == "cachebinding" {
				tok, _ := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme"})
				atFirst = tok
			}
			if step == "remove" {
				return errors.New("config directory is full")
			}
			return nil
		}))
		_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
		require.ErrorContains(t, err, "config directory is full")
		assert.Equal(t, "id:sec", atFirst, "the first write lands while the credential is still there")
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.Equal(t, config.PauseLocked, cfg.Servers["acme"].CacheWritesPaused, "the entry left behind stays paused")
	})
	t.Run("success", func(t *testing.T) {
		dir := seed(t)
		_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
		require.NoError(t, err)
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		_, present := cfg.Servers["acme"]
		assert.False(t, present)
		assert.False(t, cfg.Servers["other"].Paused(), "the last write resumes the other entries")
	})
}

// TestApplyUpdateKeepsTheWholeEntryButTheCAOnACAOnlyUpdate asserts the
// entry write of a CA-only update: every field of the loaded entry but
// the CA and its fingerprint is kept, the credential's fields and the
// pause included.
func TestApplyUpdateKeepsTheWholeEntryButTheCAOnACAOnlyUpdate(t *testing.T) {
	dir, prod := pausedEntryDir(t)
	_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem2}, Pos: []string{"prod"}}, nil)
	require.NoError(t, err)
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	want := prod
	want.CA, want.CAFingerprint = cmdtest.CAPem2, ""
	assert.Equal(t, want, cfg.Servers["prod"])
}

// TestConfigStatusListsEveryPausedEntryWithItsReason asserts the report a
// pause nobody cleared has: each paused entry with the reason it stays -
// a pin step left that would move readers, a change record that cannot
// be read, a pause made without the lock, or nothing, when the next cache
// miss or credential change resumes it - in the text and as
// cacheWritesPaused in the JSON, and no such key when nothing is paused.
func TestConfigStatusListsEveryPausedEntryWithItsReason(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name   string
		pause  string
		setup  func(t *testing.T, dir string)
		reason string
		text   string
	}{
		{name: "nothing holds it", pause: config.PauseLocked, reason: config.PauseReasonNone,
			text: "prod: none - the next exec-credential cache miss or credential change resumes it"},
		{name: "made without the lock", pause: config.PauseNoLock, reason: config.PauseReasonNoLock,
			text: "prod: noLock - the change ran with RANCHER_CLI_CONFIG_NO_LOCK; a login or logout of the entry clears it: rancher auth login 'prod'"},
		{name: "a pin step left", pause: config.PauseLocked, reason: config.PauseReasonPinStep,
			text: "prod: pinStep - a credential store change still has to write its pin, which switches where credentials are read from; the next auth login, auth logout, config delete-server or config update-server --url finishes it",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte("version: 1\nid: "+id+"\nsteps:\n- kind: pin\n  target: keychain\n"), 0o600))
			}},
		{name: "a change record that cannot be read", pause: config.PauseLocked, reason: config.PauseReasonChangeRecordUnreadable,
			text: "cannot be read; inspect it, then restore a valid copy or move it aside; credentialStoreChange says why it cannot be read",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte("not: [yaml"), 0o600))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvCredentialStore, "")
			dir := t.TempDir()
			cfg := &config.Config{CredentialStore: "auto", Servers: map[string]config.Server{
				"prod":    {URL: "https://prod.example.com", CacheWritesPaused: tc.pause},
				"staging": {URL: "https://staging.example.com"},
			}}
			if tc.setup != nil {
				cfg.CredentialTransitionID = id
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			require.NoError(t, config.WritePin(dir, "file"))
			if tc.setup != nil {
				tc.setup(t, dir)
			}

			res := execConfigInfoResult(t, dir)
			assert.Contains(t, res.Text, "cacheWritesPaused:\n  "+"prod: ")
			assert.Contains(t, styleConfigInfoText(res.Text, style.Style{Enabled: true}), "\x1b[1mcacheWritesPaused:\x1b[0m\n",
				"the section label is bold, like the other sections")
			assert.Contains(t, res.Text, tc.text)
			raw, err := json.Marshal(res.Data.(map[string]any)["cacheWritesPaused"])
			require.NoError(t, err)
			var got []map[string]string
			require.NoError(t, json.Unmarshal(raw, &got))
			assert.Equal(t, []map[string]string{{"entry": "prod", "reason": tc.reason}}, got)
		})
	}

	t.Run("nothing paused", func(t *testing.T) {
		dir, _ := cmdtest.FilePolicyDir(t)
		addServerForUpdateTests(t, dir, "qa1", "https://qa1.example.com")
		res := execConfigInfoResult(t, dir)
		_, present := res.Data.(map[string]any)["cacheWritesPaused"]
		assert.False(t, present)
		assert.NotContains(t, res.Text, "cacheWritesPaused")
	})
}

// TestUpdateServerURLClearsTheRecordedAuthConfigAndACAOnlyUpdateKeepsIt
// asserts the entry write of a URL change leaves the entry no auth config
// - it was made through a login of the old address - and a CA-only update,
// which changes no credential, keeps it with the owner.
func TestUpdateServerURLClearsTheRecordedAuthConfigAndACAOnlyUpdateKeepsIt(t *testing.T) {
	record := func(t *testing.T, dir string) {
		t.Helper()
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		s := cfg.Servers["prod"]
		s.TokenAuthConfig = "corp-ldap"
		cfg.Servers["prod"] = s
		require.NoError(t, cfg.Write())
	}
	t.Run("a URL change", func(t *testing.T) {
		dir, _ := pausedEntryDir(t)
		record(t, dir)
		_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://moved.example.com"}, Pos: []string{"prod"}}, nil)
		require.NoError(t, err)
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.Empty(t, cfg.Servers["prod"].TokenAuthConfig)
		assert.Empty(t, cfg.Servers["prod"].TokenUserID)
	})
	t.Run("a CA-only update", func(t *testing.T) {
		dir, _ := pausedEntryDir(t)
		record(t, dir)
		_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": cmdtest.CAPem2}, Pos: []string{"prod"}}, nil)
		require.NoError(t, err)
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.Equal(t, "corp-ldap", cfg.Servers["prod"].TokenAuthConfig)
		assert.Equal(t, "u-1", cfg.Servers["prod"].TokenUserID)
	})
}
