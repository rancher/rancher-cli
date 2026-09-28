package config

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestACacheWriteRefusesAPausedOrUnbuiltBinding asserts the refusal every
// exec-credential cache write passes through: a binding whose load showed
// its entry's cache writes paused, whatever the pause's value, writes
// nothing and says so with ErrCacheWritesPaused; one no constructor built
// from a load, or one with no directory, writes nothing either.
func TestACacheWriteRefusesAPausedOrUnbuiltBinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	canon := CanonicalDir(dir)
	cfg := &Config{Servers: map[string]Server{
		"locked":  {URL: "https://r.ex", CacheBindingID: "b1", CacheWritesPaused: PauseLocked},
		"nolock":  {URL: "https://r.ex", CacheBindingID: "b2", CacheWritesPaused: PauseNoLock},
		"unknown": {URL: "https://r.ex", CacheBindingID: "b3", CacheWritesPaused: "someday"},
		"open":    {URL: "https://r.ex", CacheBindingID: "b4"},
	}}
	for _, tc := range []struct {
		name    string
		binding CacheBinding
		want    error
	}{
		{"paused under the lock", CacheBindingFor(canon, cfg, "locked"), ErrCacheWritesPaused},
		{"paused without the lock", CacheBindingFor(canon, cfg, "nolock"), ErrCacheWritesPaused},
		{"paused with a value this release does not know", CacheBindingFor(canon, cfg, "unknown"), ErrCacheWritesPaused},
		{"the zero value", CacheBinding{}, errCacheBindingUnset},
		{"an entry the load does not have", CacheBindingFor(canon, cfg, "gone"), errCacheBindingUnset},
		{"no load at all", CacheBindingFor(canon, nil, "open"), errCacheBindingUnset},
		{"no directory", CacheBindingFor("", cfg, "open"), errCacheBindingUnset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewFileStore(t.TempDir())
			err := store.SetCachedCred("k", "tok", time.Now().Add(time.Hour), "n", "https://r.ex", "c", tc.binding)
			require.ErrorIs(t, err, tc.want)
			creds, lerr := store.ListCachedCreds()
			require.NoError(t, lerr)
			assert.Empty(t, creds, "nothing was written")
		})
	}

	t.Run("an entry whose cache writes are not paused", func(t *testing.T) {
		t.Parallel()
		store := NewFileStore(dir)
		b := CacheBindingFor(canon, cfg, "open")
		require.NoError(t, store.SetCachedCred("k", "tok", time.Now().Add(time.Hour), "n", "https://r.ex", "c", b))
		assert.Equal(t, "open", b.Entry())
		assert.Equal(t, "b4", b.BindingID(), "the id is the entry's in the load the binding was built from")
		assert.False(t, b.Paused())
	})
}

// TestAStoreThatForwardsItsCacheRefusesThePausedBindingToo asserts the
// refusal holds whichever store the write reaches: the keychain and helper
// stores forward every cache method to a file store, and the lazy store
// forwards to whichever one it resolved.
func TestAStoreThatForwardsItsCacheRefusesThePausedBindingToo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheWritesPaused: PauseLocked}}}
	paused := CacheBindingFor(CanonicalDir(dir), cfg, "prod")
	lazy, err := NewLazyStore(&Config{CredentialStore: "file"}, dir)
	require.NoError(t, err)
	for name, store := range map[string]CredStore{
		"keychain": newPinnedKeyringStore(dir, false),
		"helper":   newExecStore("fake", dir),
		"lazy":     lazy,
	} {
		err := store.SetCachedCred("k", "tok", time.Now().Add(time.Hour), "n", "https://r.ex", "c", paused)
		require.ErrorIs(t, err, ErrCacheWritesPaused, name)
	}
	creds, lerr := NewFileStore(dir).ListCachedCreds()
	require.NoError(t, lerr)
	assert.Empty(t, creds)
}

// TestALoginsOutcomeBindsToTheEntryItsLastWriteWrote asserts the binding
// the plugin-created login hands its own cache record: the entry as the
// login's last write wrote it, id included, and the zero binding for a
// login that did not get that far.
func TestALoginsOutcomeBindsToTheEntryItsLastWriteWrote(t *testing.T) {
	dir := settledFixture(t)
	out, err := PersistLoginCredential(t.Context(), dir, settledInput())
	require.NoError(t, err)
	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	b := out.CacheBinding(CanonicalDir(dir))
	assert.Equal(t, "prod", b.Entry())
	assert.Equal(t, cfg.Servers["prod"].CacheBindingID, b.BindingID())
	assert.Equal(t, cfg.Servers["prod"].Paused(), b.Paused())

	in := settledInput()
	in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
	failed, ferr := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, ferr)
	require.ErrorIs(t, NewFileStore(dir).SetCachedCred("k", "tok", time.Now().Add(time.Hour), "n", "https://r.ex", "c", failed.CacheBinding(CanonicalDir(dir))),
		errCacheBindingUnset, "a login that failed binds nothing")
}

// TestTheStoreCheckComparesTheStoreReadWithWhatReadersResolve asserts the
// check every record the exec plugin writes after a store read passes:
// the store a reader resolves from the binding's own load - its
// declaration, then the pin or the unpinned decision read at the check -
// has to be the store the credential was read from. A resolution that
// fails refuses, and so does a credential whose store is not known.
func TestTheStoreCheckComparesTheStoreReadWithWhatReadersResolve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		decl   string
		pin    string
		env    string
		setup  func(t *testing.T, dir string)
		source string
		// noEntry binds the record to no entry, as a record for a server
		// the directory has no entry for is.
		noEntry bool
		ok      bool
	}{
		{name: "a declared store, read from it", decl: "keychain", source: backendKeychain, ok: true},
		{name: "a declared store, read from the one it left", decl: "file", source: backendKeychain},
		{name: "a pin, read from it", decl: "auto", pin: backendKeychain, source: backendKeychain, ok: true},
		{name: "a pin, read from the store it moved readers off", decl: "auto", pin: backendKeychain, source: backendFile},
		{name: "unpinned with file tokens, read from the file", decl: "auto", source: backendFile, ok: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "tok"))
			}},
		{name: "unpinned with an identity record, read from the file", decl: "auto", source: backendFile,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				seedKeychainCredential(t, dir, "prod", "tok")
			}},
		{name: "a helper, read from it", decl: "exec:vault", source: "exec:vault", ok: true},
		{name: "a helper, read from another", decl: "exec:vault", source: "exec:other"},
		{name: "a helper, read from the file store it left", decl: "exec:vault", source: backendFile},
		{name: "credentials in both stores", decl: "auto", source: backendFile,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "tok"))
				seedKeychainCredential(t, dir, "staging", "tok")
			}},
		{name: "an unreadable credentials file", decl: "auto", source: backendFile,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Mkdir(CredentialsFilePath(dir), 0o700))
			}},
		{name: "the variable against the declaration", decl: "file", env: "keychain", source: backendFile},
		{name: "a store that is not known", decl: "file", source: ""},
		{name: "a record bound to no entry", decl: "file", source: "", noEntry: true, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(EnvCredentialStore, tc.env)
			dir := t.TempDir()
			cfg := &Config{CredentialStore: tc.decl, Servers: map[string]Server{
				"prod": {URL: "https://r.ex"}, "staging": {URL: "https://s.ex"},
			}}
			if tc.pin != "" {
				require.NoError(t, WritePin(dir, tc.pin))
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			b := CacheBindingFor(dir, cfg, "prod")
			if tc.noEntry {
				b = CacheBindingWithoutEntry(dir, cfg)
			}
			b.source = tc.source
			err := b.CheckStore()
			if tc.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrCacheStoreChanged)
		})
	}
}

// TestEveryStoreSaysWhichBackendItReads asserts what the store check
// compares on the writer's side: each physical store names its backend,
// and a lazy store names the one its first credential call resolved, and
// none before that call or after a resolution that failed.
func TestEveryStoreSaysWhichBackendItReads(t *testing.T) {
	keyring.MockInit()
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir()
	backendOf := func(s CredStore) string {
		b, fixed := s.ResolvedBackend()
		require.True(t, fixed)
		return b
	}
	assert.Equal(t, backendFile, backendOf(NewFileStore(dir)))
	assert.Equal(t, backendKeychain, backendOf(newPinnedKeyringStore(dir, true)))
	assert.Equal(t, "exec:vault", backendOf(newExecStore("vault", dir)))

	require.NoError(t, WritePin(dir, backendKeychain))
	lazy, err := NewLazyStore(&Config{CredentialStore: "auto"}, dir)
	require.NoError(t, err)
	_, fixed := lazy.ResolvedBackend()
	assert.False(t, fixed, "nothing is read before the first credential call")
	_, _ = lazy.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	assert.Equal(t, backendKeychain, backendOf(lazy))
	require.NoError(t, WritePin(dir, backendFile))
	assert.Equal(t, backendKeychain, backendOf(lazy), "and it never resolves again")

	t.Setenv(EnvCredentialStore, "keychain")
	conflicted, err := NewLazyStore(&Config{CredentialStore: "file"}, dir)
	require.NoError(t, err)
	_, _ = conflicted.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	b, fixed := conflicted.ResolvedBackend()
	assert.True(t, fixed, "a resolution that failed is settled")
	assert.Empty(t, b, "and reads from no store")
}

// TestThePluginLoginsRecordIsCheckedAgainstTheStoreItsCommitReached
// asserts the store check on the plugin-created login's own record: the
// store the commit reached against a reader's resolution of the config
// the login's last write wrote, with the pin read at the check.
func TestThePluginLoginsRecordIsCheckedAgainstTheStoreItsCommitReached(t *testing.T) {
	dir := repinFixture(t)
	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	b := out.CacheBinding(CanonicalDir(dir))
	require.NoError(t, b.CheckStore(), "the commit reached the keychain, which the pin now names")

	require.NoError(t, WritePin(dir, backendFile))
	require.ErrorIs(t, b.CheckStore(), ErrCacheStoreChanged, "a pin that moved readers off the commit's store refuses")
}

// TestTheStoreCheckKnowsWhetherAStoreWasRead asserts the three ways the
// store a record's credential came from can be unknown: a lazy store no
// credential call resolved read nothing, so there is no copy to be stale
// and the record is written; a lazy store whose resolution failed, and a
// login's outcome that names no store its commit reached, refuse.
func TestTheStoreCheckKnowsWhetherAStoreWasRead(t *testing.T) {
	keyring.MockInit()
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "file", Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}

	unread, err := NewLazyStore(cfg, dir)
	require.NoError(t, err)
	require.NoError(t, CacheBindingFor(dir, cfg, "prod").ReadFrom(unread).CheckStore(), "a store never read")

	t.Setenv(EnvCredentialStore, "keychain")
	failed, err := NewLazyStore(cfg, dir)
	require.NoError(t, err)
	_, _ = failed.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.ErrorIs(t, CacheBindingFor(dir, cfg, "prod").ReadFrom(failed).CheckStore(), ErrCacheStoreChanged, "a store whose resolution failed")
	t.Setenv(EnvCredentialStore, "")

	out := PersistLoginOutcome{Name: "prod", written: cfg}
	require.ErrorIs(t, out.CacheBinding(dir).CheckStore(), ErrCacheStoreChanged, "an outcome that names no store")
	out.committedTo = backendFile
	require.NoError(t, out.CacheBinding(dir).CheckStore())
}
