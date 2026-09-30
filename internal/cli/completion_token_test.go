package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// NOTE: tests below mutate storedConfig (package global) and env - NOT parallel.
func TestResolveCompletionTokenEnvWins(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	t.Setenv(config.EnvToken, "mytoken")
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	cap := resolveCompletionToken("")
	assert.Equal(t, "mytoken", cap.Token)
	assert.False(t, cap.IDOnly)
}

func TestResolveCompletionTokenExecCredIDOnly(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	// No env token, no file store token (CredentialStore not set to "file").
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-ctx", "https://rancher.example.com", "c-abc"),
		"exec-token", time.Now().Add(time.Hour),
		"k8s-ctx", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "k8s-ctx", "https://rancher.example.com", "c-abc"),
	))
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	cap := resolveCompletionToken("c-abc")
	assert.Equal(t, "exec-token", cap.Token)
	assert.True(t, cap.IDOnly)
}

func TestResolveCompletionTokenAmbiguousExecCredYieldsEmpty(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	// Two entries for the same server+cluster → ambiguous.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-a", "https://rancher.example.com", "c-abc"),
		"tok-a", time.Now().Add(time.Hour),
		"ctx-a", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "ctx-a", "https://rancher.example.com", "c-abc"),
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-b", "https://rancher.example.com", "c-abc"),
		"tok-b", time.Now().Add(time.Hour),
		"ctx-b", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "ctx-b", "https://rancher.example.com", "c-abc"),
	))
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	cap := resolveCompletionToken("c-abc")
	assert.Empty(t, cap.Token, "ambiguous exec-cred must yield empty")
}

// TestResolveCompletionTokenInvalidServerURLYieldsEmpty asserts that completion
// returns an empty capability for a configured server URL that fails HTTP(S)
// validation, never surfacing a stored record.
//
// Three sub-cases: unsupported scheme (ftp://), malformed URL (https://[::1),
// and empty hostname (https://:443). Each seeds both a file-store token and an
// exec-cache entry for the bad URL, then asserts the returned capability is empty.
func TestResolveCompletionTokenInvalidServerURLYieldsEmpty(t *testing.T) {
	cases := []struct {
		name      string
		serverURL string
	}{
		{"unsupported scheme ftp", "ftp://host"},
		{"malformed unclosed IPv6", "https://[::1"},
		{"empty hostname default port", "https://:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NOT parallel: mutates storedConfig and env globals.
			t.Setenv(config.EnvToken, "")
			keyring.MockInit()
			dir := t.TempDir()

			// Seed a file-store token for the server entry with the bad URL.
			fileStore := config.NewFileStore(dir)
			require.NoError(t, fileStore.SetToken(config.ServerRef{Name: "bad"}, "should-not-surface"))

			cfg := &config.Config{
				CurrentServer:   "bad",
				Servers:         map[string]config.Server{"bad": {URL: tc.serverURL}},
				CredentialStore: "file",
			}
			// Seed an exec-cache entry under the bad URL's identity.
			badID := config.CanonicalServerIdentity(tc.serverURL)
			require.NoError(t, fileStore.SetCachedCred(
				config.CacheKey("bad", badID, "c-1"),
				"also-should-not-surface", time.Now().Add(time.Hour),
				"bad", badID, "c-1",
				config.CacheBindingForSeed(dir, cfg, "bad", badID, "c-1"),
			))

			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			setStoredConfigForTest(t, cfg, dir)

			cap := resolveCompletionToken("c-1")
			assert.Empty(t, cap.Token,
				"invalid server URL %q must yield empty capability, not surface a stored credential",
				tc.serverURL)
		})
	}
}

// TestResolveCompletionTokenShimEntryPlusNativeResolvesNative asserts that a
// shim cache entry for the same server+cluster as a native entry must be
// excluded from the single-entry candidacy count so completion still resolves
// from the native entry (shim + 1 native → native).
func TestResolveCompletionTokenShimEntryPlusNativeResolvesNative(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	// Native entry ("ctx-native").
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-native", "https://rancher.example.com", "c-abc"),
		"native-tok", time.Now().Add(time.Hour),
		"ctx-native", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "ctx-native", "https://rancher.example.com", "c-abc"),
	))
	// Shim entry ("legacy:x") for the same server+cluster.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("legacy:x", "https://rancher.example.com", "c-abc"),
		"shim-tok", time.Now().Add(time.Hour),
		"legacy:x", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "legacy:x", "https://rancher.example.com", "c-abc"),
	))
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	cap := resolveCompletionToken("c-abc")
	assert.Equal(t, "native-tok", cap.Token,
		"shim entry must be excluded; native entry must resolve as the sole candidate")
	assert.True(t, cap.IDOnly)
}

// TestResolveCompletionTokenShimOnlyYieldsEmpty verifies the shim-only case:
// when only a legacy:-prefixed cache entry exists for a server+cluster,
// completion must yield an empty capability - the slot is kubectl's, not the
// operator's.
func TestResolveCompletionTokenShimOnlyYieldsEmpty(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	// Only a shim entry - no native entry.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("legacy:y", "https://rancher.example.com", "c-def"),
		"shim-only-tok", time.Now().Add(time.Hour),
		"legacy:y", "https://rancher.example.com", "c-def",
		config.CacheBindingForSeed(dir, cfg, "legacy:y", "https://rancher.example.com", "c-def"),
	))
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	cap := resolveCompletionToken("c-def")
	assert.Empty(t, cap.Token,
		"shim-only entry must yield empty capability (not a completion candidate)")
}

// TestFileStoreTokenCrossServerURLOverride asserts that
// RANCHER_CLI_URL pointing at server B must not send server A's token to B.
func TestFileStoreTokenCrossServerURLOverride(t *testing.T) {
	// NOT parallel: mutates storedConfig and env globals.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	// Config current server is server A (rancher-a.example.com).
	cfg := &config.Config{
		CurrentServer:   "srv-a",
		Servers:         map[string]config.Server{"srv-a": {URL: "https://rancher-a.example.com"}},
		CredentialStore: "file",
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Write a file-store token for server A using the real API.
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "srv-a"}, "token-for-a"))
	setStoredConfigForTest(t, cfg, dir)

	// RANCHER_CLI_URL points at server B.
	t.Setenv(config.EnvURL, "https://rancher-b.example.com")

	// Must return empty: server A's token must not be sent to server B.
	cap := resolveCompletionToken("")
	assert.Empty(t, cap.Token, "cross-server URL override must not send server A token to server B")
}

// TestResolveCompletionTokenFileStoreSameURLServers asserts the file-store
// lookup when several stored servers share one URL. The current server is
// the operator's own selection, so its token must be used whenever its URL
// is the effective URL; the URL scan with its ambiguity guard applies only
// when the effective URL points elsewhere (RANCHER_CLI_URL override).
func TestResolveCompletionTokenFileStoreSameURLServers(t *testing.T) {
	const sharedURL = "https://rancher.example.com"
	const otherURL = "https://other.example.com"

	setup := func(t *testing.T, current string) {
		t.Helper()
		// NOT parallel: mutates storedConfig and env globals.
		t.Setenv(config.EnvToken, "")
		t.Setenv(config.EnvURL, "")
		t.Setenv(config.EnvCredentialStore, "file")
		keyring.MockInit()
		dir := t.TempDir()
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetToken(config.ServerRef{Name: "alpha"}, "alpha-token"))
		require.NoError(t, store.SetToken(config.ServerRef{Name: "beta"}, "beta-token"))
		require.NoError(t, store.SetToken(config.ServerRef{Name: "gamma"}, "gamma-token"))
		require.NoError(t, store.SetToken(config.ServerRef{Name: "delta"}, "delta-token"))
		// epsilon shares the URL but has no stored login token.
		cfg := &config.Config{
			CurrentServer: current,
			Servers: map[string]config.Server{
				"alpha":   {URL: sharedURL},
				"beta":    {URL: sharedURL},
				"gamma":   {URL: otherURL},
				"delta":   {URL: otherURL},
				"epsilon": {URL: sharedURL},
			},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfigForTest(t, cfg, dir)
	}

	cases := []struct {
		name    string
		current string
		envURL  string
		want    string
	}{
		{"current server wins among same-URL entries", "alpha", "", "alpha-token"},
		{"other same-URL entry as current also wins", "beta", "", "beta-token"},
		{"url override matching the current server keeps its token", "alpha", sharedURL, "alpha-token"},
		{"url override to a different ambiguous url yields empty", "alpha", otherURL, ""},
		{"url override to an unknown url yields empty", "alpha", "https://unknown.example.com", ""},
		{"current server without a stored token yields empty, never a sibling's token", "epsilon", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setup(t, tc.current)
			if tc.envURL != "" {
				t.Setenv(config.EnvURL, tc.envURL)
			}
			cap := resolveCompletionToken("")
			assert.Equal(t, tc.want, cap.Token)
			if tc.want != "" {
				assert.False(t, cap.IDOnly)
			}
		})
	}
}

// TestFileStoreTokenSilentRefusesAStoreConflict asserts completion offers
// nothing through a store the directory did not declare. Completion has
// no way to report an error, so the only safe answer to a declaration the
// variable disagrees with is no token and no candidates: authenticating a
// live completion request with a credential the directory did not choose
// is the same wrong-identity read it is anywhere else.
// NOT parallel: mutates storedConfig package global and env.
func TestFileStoreTokenSilentRefusesAStoreConflict(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		CurrentServer:   "srv",
		Servers:         map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(
		config.ServerRef{Name: "srv", URL: "https://rancher.example.com"}, "file-token"))
	setStoredConfigForTest(t, cfg, dir)

	t.Setenv(config.EnvCredentialStore, "")
	assert.Equal(t, "file-token", fileStoreTokenSilent(dir, "https://rancher.example.com"),
		"the declared store is read when nothing disagrees with it")

	t.Setenv(config.EnvCredentialStore, "keychain")
	assert.Empty(t, fileStoreTokenSilent(dir, "https://rancher.example.com"),
		"a variable naming another store yields no token")
}

// TestFileStoreTokenSilentOnAnUnpinnedDirectory asserts completion's
// answer for an `auto` directory with no pin, which no read pins any more:
// the file token is offered when the directory's entries keep their
// credentials in the file store, and nothing when they keep them in the
// keychain or in both stores - the last is an error for every credential
// command, and completion has no way to report one.
// NOT parallel: mutates storedConfig package global and env.
func TestFileStoreTokenSilentOnAnUnpinnedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keychain bool
		want     string
	}{
		{name: "file tokens only", want: "file-token"},
		{name: "an identity record beside them", keychain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvToken, "")
			t.Setenv(config.EnvCredentialStore, "")
			dir := t.TempDir()
			cfg := &config.Config{
				CurrentServer: "srv",
				Servers: map[string]config.Server{
					"srv":   {URL: "https://rancher.example.com"},
					"other": {URL: "https://other.example.com"},
				},
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			require.NoError(t, config.NewFileStore(dir).SetToken(
				config.ServerRef{Name: "srv", URL: "https://rancher.example.com"}, "file-token"))
			if tc.keychain {
				// A record names a keychain credential; its content is
				// never read to answer where the directory keeps them.
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "credstore-id"), 0o700))
				sum := sha256.Sum256([]byte("other"))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "credstore-id", hex.EncodeToString(sum[:])), []byte("x"), 0o600))
			}
			setStoredConfigForTest(t, cfg, dir)

			assert.Equal(t, tc.want, fileStoreTokenSilent(dir, "https://rancher.example.com"))
			assert.NoFileExists(t, config.PinPath(dir), "completion writes no pin")
		})
	}
}

// setStoredConfigForTest installs the config snapshot AND the invocation's
// cache anchor, the pair the root hook installs together. A test that
// installs only the snapshot leaves every cache reader unable to judge a
// record, which is a miss: correct in production, and not what a test
// about completion's cache branch means to exercise.
func setStoredConfigForTest(t *testing.T, cfg *config.Config, dir string) {
	t.Helper()
	anchor := config.NewCacheAnchor(dir)
	setStoredConfig(cfg, dir)
	config.SetProcessCacheAnchor(anchor.WithConfig(cfg))
	t.Cleanup(func() { config.SetProcessCacheAnchor(nil) })
}

// TestResolveCompletionTokenWithoutADirectoryReadsNoFile asserts that an
// invocation with no config directory of its own answers nothing, with a
// usable cache file sitting in the working directory. Reading a path
// relative to the working directory is what the old code did, and the
// token it found belonged to nothing in particular.
//
// Two refusals stand between here and that file now, and this asserts the
// outcome rather than either one: the guard returns before the read, and
// a record naming another directory would not validate even if the read
// happened. Neither can be isolated - a record that could pass the
// binding rule with no directory does not exist, because a record and an
// anchor with no directory are both refused by name.
func TestResolveCompletionTokenWithoutADirectoryReadsNoFile(t *testing.T) {
	// NOT parallel: mutates the storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()

	// A cache file in the working directory, seeded so that a reader
	// that did look there would find a usable record.
	cwd := t.TempDir()
	store := config.NewFileStore(cwd)
	seen := &config.Config{Servers: map[string]config.Server{"srv": {URL: "https://rancher.example.com"}}}
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-ctx", "https://rancher.example.com", "c-abc"),
		"loose-token", time.Now().Add(time.Hour),
		"k8s-ctx", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(config.CanonicalDir(cwd), seen, "k8s-ctx", "https://rancher.example.com", "c-abc"),
	))
	t.Chdir(cwd)
	t.Setenv(config.EnvURL, "https://rancher.example.com")

	t.Run("no stored snapshot", func(t *testing.T) {
		setStoredConfig(nil, cwd)
		config.SetProcessCacheAnchor(nil)
		t.Cleanup(func() { setStoredConfig(nil, "") })
		assert.Empty(t, resolveCompletionToken("c-abc").Token,
			"a config the root hook could not read is not a reason to read a stranger's cache")
	})

	t.Run("no directory", func(t *testing.T) {
		cfg := &config.Config{Servers: map[string]config.Server{"srv": {URL: "https://rancher.example.com"}}}
		setStoredConfig(cfg, "")
		config.SetProcessCacheAnchor(nil)
		t.Cleanup(func() { setStoredConfig(nil, "") })
		assert.Empty(t, resolveCompletionToken("c-abc").Token,
			"with no directory resolved there is no file this invocation owns")
	})
}

// TestResolveCompletionTokenObeysTheCacheBinding asserts completion is
// the fourth reader and judges like the other three: a copied directory
// serves none of the records its source wrote, and a login that advanced
// the entry's binding id makes the previous login's record a miss.
// Completion is the reader that answers without being asked for a
// credential, so a record served here is the one nobody would notice.
func TestResolveCompletionTokenObeysTheCacheBinding(t *testing.T) {
	// NOT parallel: mutates the storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()

	seed := func(t *testing.T, dir string) *config.Config {
		t.Helper()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com", CacheBindingID: "b1"}},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		store := config.NewFileStore(dir)
		require.NoError(t, store.SetCachedCred(
			config.CacheKey("k8s-ctx", "https://rancher.example.com", "c-abc"),
			"exec-token", time.Now().Add(time.Hour),
			"k8s-ctx", "https://rancher.example.com", "c-abc",
			config.CacheBindingForSeed(dir, cfg, "k8s-ctx", "https://rancher.example.com", "c-abc"),
		))
		return cfg
	}

	t.Run("the directory that wrote the record serves it", func(t *testing.T) {
		dir := t.TempDir()
		cfg := seed(t, dir)
		setStoredConfigForTest(t, cfg, dir)
		assert.Equal(t, "exec-token", resolveCompletionToken("c-abc").Token)
	})

	t.Run("a copied directory serves nothing", func(t *testing.T) {
		source := t.TempDir()
		seed(t, source)

		// cp -r of the whole directory: config.yaml, the binding stamp
		// and token-cache.yaml travel together.
		copied := t.TempDir()
		entries, err := os.ReadDir(source)
		require.NoError(t, err)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(source, e.Name()))
			require.NoError(t, rerr)
			require.NoError(t, os.WriteFile(filepath.Join(copied, e.Name()), b, 0o600))
		}
		copiedCfg, lerr := config.Load(config.FilePath(copied))
		require.NoError(t, lerr)

		// The wiring, not just the record: the process anchor is the
		// source's - the state a reader reaches by resolving one
		// directory while an anchor for another is in hand - and
		// completion must discard it rather than judge the copy's
		// records by the source's stamp and config.
		sourceAnchor := config.NewCacheAnchor(source)
		sourceCfg, serr := config.Load(config.FilePath(source))
		require.NoError(t, serr)
		setStoredConfig(copiedCfg, copied)
		config.SetProcessCacheAnchor(sourceAnchor.WithConfig(sourceCfg))
		t.Cleanup(func() {
			setStoredConfig(nil, "")
			config.SetProcessCacheAnchor(nil)
		})

		assert.Empty(t, resolveCompletionToken("c-abc").Token,
			"the record names the directory that wrote it, and this is not it")
	})

	t.Run("a login since the record was written makes it a miss", func(t *testing.T) {
		dir := t.TempDir()
		cfg := seed(t, dir)

		// What a logout or a re-login leaves behind: the same entry,
		// a new cache binding id.
		entry := cfg.Servers["srv"]
		require.NoError(t, config.AdvanceCacheBinding(&entry))
		cfg.Servers["srv"] = entry
		require.NoError(t, cfg.Write())
		reloaded, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		setStoredConfigForTest(t, reloaded, dir)

		assert.Empty(t, resolveCompletionToken("c-abc").Token,
			"the record was made under the previous login")
	})
}

// TestLiveCompletionOperationCounts asserts what live completion's
// exec-credential cache branch costs a TAB press: the reads it makes to
// decide the file store does not answer, the record read and the stamp
// read that confirms it. No lock, no keychain call and no look at a
// credential store change record, which a directory that names none
// never opens.
func TestLiveCompletionOperationCounts(t *testing.T) {
	// NOT parallel: mutates the storedConfig package global, the process
	// anchor and the package-level observers.
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com", CacheBindingID: "b1"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetCachedCred(
		config.CacheKey("k8s-ctx", "https://rancher.example.com", "c-abc"),
		"exec-token", time.Now().Add(time.Hour),
		"k8s-ctx", "https://rancher.example.com", "c-abc",
		config.CacheBindingForSeed(dir, cfg, "k8s-ctx", "https://rancher.example.com", "c-abc"),
	))
	setStoredConfigForTest(t, cfg, dir)

	var token string
	c := cmdtest.CountOperations(t, func() { token = resolveCompletionToken("c-abc").Token })

	assert.Equal(t, "exec-token", token)
	assert.Equal(t, map[string]int{"pinread": 1, "dirstat": 1, "credentialsread": 1, "stampread": 1}, c.Ops,
		"the unpinned decision's look and read, then the confirmation of the record")
	assert.Zero(t, c.Keychain)
	assert.Zero(t, c.ConfigLocks)
	assert.Zero(t, c.Flocks)
	assert.Zero(t, c.TransitionLocks)
}
