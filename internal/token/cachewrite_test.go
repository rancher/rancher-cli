package token

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file assert where an exec-credential cache record's
// binding comes from and what refuses a write under it.

// newMadeFrom starts a testrancher.MadeFrom for the test and closes it
// when the test ends.
func newMadeFrom(t *testing.T) *testrancher.MadeFrom {
	t.Helper()
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	return m
}

// writeEntries replaces dir's config with these entries, declared file.
func writeEntries(t *testing.T, dir string, reuse bool, servers map[string]config.Server) {
	t.Helper()
	cfg := &config.Config{CredentialStore: "file", TokenReuse: reuse, Servers: servers}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// TestEveryRecordIsBoundToTheEntryItsCredentialCameFrom asserts where each
// site that produces a cache record's binding takes it from: the stored
// credential's own entry, the entry a later lookup would choose when no
// stored credential produced the token, and for each the id and the pause
// that entry had in the same load. An entry whose cache writes are paused
// there hands on a binding every write refuses.
func TestEveryRecordIsBoundToTheEntryItsCredentialCameFrom(t *testing.T) {
	m := newMadeFrom(t)
	for _, paused := range []string{"", config.PauseLocked} {
		t.Run("paused="+paused, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(config.EnvCredentialStore, "")
			// "a-guess" ranks first among the entries at this URL and holds
			// nothing; prod holds the credential.
			writeEntries(t, dir, false, map[string]config.Server{
				"a-guess": {URL: m.URL, CacheBindingID: "guess-id", CacheWritesPaused: paused},
				"prod":    {URL: m.URL, CacheBindingID: "prod-id", CacheWritesPaused: paused},
			})
			store := config.NewFileStore(dir)
			cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}

			// No stored credential: the guess.
			_, _, guess, err := cmd.storedCredToken(t.Context(), m.Client(), m.URL, "local", "", false, ui.Discard())
			require.ErrorIs(t, err, errNoStoredCredential)
			assert.Equal(t, "a-guess", guess.Entry())
			assert.Equal(t, "guess-id", guess.BindingID())
			assert.Equal(t, paused != "", guess.Paused())

			// A stored credential: its own entry.
			require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: m.URL}, "alice"))
			tok, _, stored, err := cmd.storedCredToken(t.Context(), m.Client(), m.URL, "local", "", false, ui.Discard())
			require.NoError(t, err)
			assert.Equal(t, "prod", stored.Entry())
			assert.Equal(t, "prod-id", stored.BindingID())
			assert.Equal(t, paused != "", stored.Paused())
			assert.Equal(t, stored, tok.binding, "the token carries the binding it was made under")
		})
	}
}

// TestThePluginLoginsRecordsAreBoundToTheEntryTheirCredentialCameFrom
// asserts the plugin lane's two sites: a waiter that finds another
// process's login already stored takes the binding from its own second
// read, and the plugin-created login takes it from the entry its last
// write wrote.
func TestThePluginLoginsRecordsAreBoundToTheEntryTheirCredentialCameFrom(t *testing.T) {
	t.Run("the plugin-created login", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		tok, _, err := x.command(x.store).pluginLogin(t.Context(), x.command(x.store).pluginInput(x.url, x.url, "", "c-test", x.client(),
			runParams{Name: "n", AuthProvider: "localProvider"}), localPrompter())
		require.NoError(t, err)
		entry, ok := x.entry(t)
		require.True(t, ok)
		assert.Equal(t, x.entryName(), tok.binding.Entry())
		assert.Equal(t, entry.CacheBindingID, tok.binding.BindingID())
		assert.Equal(t, entry.Paused(), tok.binding.Paused())
	})

	t.Run("a waiter that finds the login stored", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		_, err := x.getToken(t, "c-test", "n", localPrompter())
		require.NoError(t, err)
		// Another process's cache writes for the entry are paused now.
		cfg, lerr := config.Load(config.FilePath(x.dir))
		require.NoError(t, lerr)
		e := cfg.Servers[x.entryName()]
		e.CacheWritesPaused = config.PauseLocked
		cfg.Servers[x.entryName()] = e
		require.NoError(t, cfg.Write())

		tok, _, err := x.command(x.store).pluginLogin(t.Context(), x.command(x.store).pluginInput(x.url, x.url, "", "c-test", x.client(),
			runParams{Name: "n", AuthProvider: "localProvider"}), ui.Discard())
		require.NoError(t, err)
		assert.Equal(t, x.entryName(), tok.binding.Entry())
		assert.Equal(t, e.CacheBindingID, tok.binding.BindingID())
		assert.True(t, tok.binding.Paused(), "from the waiter's own read, which shows the pause")
	})
}

// pausedPlugin is a file-store directory whose entry prod, at url, holds
// alice's credential with its cache writes paused as a change that did
// not clear them leaves them.
func pausedPlugin(t *testing.T, url string) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(config.EnvConfigNoLock, "")
	dir := t.TempDir()
	writeEntries(t, dir, false, map[string]config.Server{
		"prod": {URL: url, CacheBindingID: "prod-id", CacheWritesPaused: config.PauseLocked},
	})
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: url}, "alice"))
	return dir
}

// pausedOnDisk reports whether prod's cache writes are paused in
// config.yaml.
func pausedOnDisk(t *testing.T, dir string) bool {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	return cfg.Servers["prod"].Paused()
}

// TestAMissOnAPausedEntryResumesWhatNobodyCleared asserts the exec
// plugin's side of a pause: its own record is never written, since its
// credential was read before any lock; with the config lock free and
// nothing left that could move readers it clears the pause, so the next
// call caches; with the lock held, without the flock, or with a pin step
// left that would move readers, it leaves the pause and says the change
// has not finished. The credential is returned every time.
func TestAMissOnAPausedEntryResumesWhatNobodyCleared(t *testing.T) {
	m := newMadeFrom(t)
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir string)
		resumed bool
		// status says the notice points at config status, which names
		// what holds the pause.
		status bool
	}{
		{name: "the lock is free", resumed: true},
		{name: "a delete step for the entry is left", resumed: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedUnfinishedDelete(t, dir, "prod")
		}},
		{name: "another process holds the lock", setup: func(t *testing.T, dir string) {
			t.Helper()
			other := flock.New(config.LockPath(dir))
			ok, err := other.TryLock()
			require.NoError(t, err)
			require.True(t, ok)
			t.Cleanup(func() { _ = other.Unlock() })
		}},
		{name: "the lock is skipped", setup: func(t *testing.T, _ string) {
			t.Helper()
			t.Setenv(config.EnvConfigNoLock, "1")
		}},
		{name: "a pin step would move readers", status: true, setup: func(t *testing.T, dir string) {
			t.Helper()
			seedChange(t, dir, `[{kind: pin, target: keychain}]`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := pausedPlugin(t, m.URL)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			var notices bytes.Buffer
			p := ui.New(&notices, strings.NewReader(""), true, false)
			tok, err := kubectl(t, dir, m.URL, p, nil)
			require.NoError(t, err)
			assert.Equal(t, testrancher.MadeFromToken("alice"), tok, "the credential is returned")
			assert.Empty(t, served(t, dir, m.URL), "and no record was written for it")
			assert.Equal(t, !tc.resumed, pausedOnDisk(t, dir))
			if tc.resumed {
				assert.NotContains(t, notices.String(), "not caching")
				_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
				require.NoError(t, err)
				assert.Equal(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL), "the next call caches")
				return
			}
			assert.Contains(t, notices.String(), "not caching: a credential change for prod has not finished")
			assert.Equal(t, tc.status, strings.Contains(notices.String(), "(see `rancher config status`)"))
		})
	}
}

// TestAPausedMissNeverWaitsOnTheCacheLock asserts the lock order on a
// paused miss: the refusal comes before the token-cache lock, so the
// config lock the resume tries is never taken while that one is held,
// and a cache lock another process holds does not delay the call.
func TestAPausedMissNeverWaitsOnTheCacheLock(t *testing.T) {
	m := newMadeFrom(t)
	dir := pausedPlugin(t, m.URL)
	other := flock.New(filepath.Join(dir, "token-cache.lock"))
	ok, err := other.TryLock()
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = other.Unlock() })

	start := time.Now()
	_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the call never waited for the cache lock")
	assert.False(t, pausedOnDisk(t, dir), "and it resumed the entry")
}

// TestThePluginLoginsOwnRecordWaitsForItsLastWrite asserts that the record
// the plugin-created login writes for the token it made is bound to its
// entry as the login's last write left it: when that write failed, the
// entry is still paused, the record is refused like any other, and the
// same call's resume clears the pause for the next one.
func TestThePluginLoginsOwnRecordWaitsForItsLastWrite(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	failed := false
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "resume" && !failed {
			failed = true
			return errors.New("config directory is full")
		}
		return nil
	})
	var notices bytes.Buffer
	res, err := x.getToken(t, "c-test", "n", ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true))
	restore()
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "stay paused", "the login says its last write failed")
	cmd := tokenCmd{dir: x.dir, store: x.store, anchor: anchorFor(x.dir)}
	_, _, _, hit := cmd.probeFlagCache("n", x.url, "c-test", ui.Discard())
	assert.False(t, hit, "the login's own record was not written while its entry was paused")
	entry, ok := x.entry(t)
	require.True(t, ok)
	assert.False(t, entry.Paused(), "the same call's resume cleared it")
}
