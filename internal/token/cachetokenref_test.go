package token

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// cachedInfo returns the one cache record written for name.
func cachedInfo(t *testing.T, dir, name string) config.CachedCredInfo {
	t.Helper()
	creds, err := config.NewFileStore(dir).ListCachedCreds()
	require.NoError(t, err)
	var found []config.CachedCredInfo
	for _, c := range creds {
		if c.Name == name {
			found = append(found, c)
		}
	}
	require.Len(t, found, 1, "one record for %s", name)
	return found[0]
}

// newRecordFixtureOpts is newRecordFixture with the fixture Rancher built
// from opts.
func newRecordFixtureOpts(t *testing.T, reuse bool, opts testrancher.Options) *recordFixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvCredentialStore, "")
	opts.Users = entryTableUsers()
	f := newFacadeWithClusters(&opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: reuse, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return &recordFixture{t: t, f: f, srv: srv, dir: dir, store: config.NewFileStore(dir)}
}

// TestTheCacheRecordNamesTheTokenTheCallCreated asserts that the exec
// plugin records the ref and owner of a cluster token it created in the
// same call, on each of the three paths that create one: from a stored
// login, through the plugin-created login, and by an interactive sign-in.
// Rancher answers a sign-in with the v3 login body, which names no owner.
// The ref names the very token cached.
func TestTheCacheRecordNamesTheTokenTheCallCreated(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reuse     bool
		opts      testrancher.Options
		setup     func(r *recordFixture)
		terminal  bool
		wantKind  TokenKind
		wantOwner string
	}{
		{"a stored login's cluster token", false, testrancher.Options{}, func(r *recordFixture) { r.login("prod", "", "alice", false) }, false, TokenKindExt, "user-alice"},
		{"the plugin-created login's cluster token", true, testrancher.Options{}, func(*recordFixture) {}, true, TokenKindExt, "user-carol"},
		{"an interactive sign-in", false, testrancher.Options{LoginShape: "v3"}, func(*recordFixture) {}, true, TokenKindV3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecordFixtureOpts(t, tc.reuse, tc.opts)
			tc.setup(r)
			_, err := r.call("prod", tc.terminal)
			require.NoError(t, err)

			rec := cachedInfo(t, r.dir, "prod")
			require.NotNil(t, rec.TokenRef, "the record names its token")
			assert.Equal(t, tc.wantKind, rec.TokenRef.Kind)
			owner, ok := r.f.TokenOwner(rec.TokenRef.Name)
			require.True(t, ok, "the ref names a token the server issued")
			assert.NotEmpty(t, owner)
			assert.Equal(t, tc.wantOwner, rec.TokenUserID)
			tok, _, gerr := r.store.GetCachedCred(rec.Key, anchorFor(r.dir))
			require.NoError(t, gerr)
			assert.True(t, usableBearer(rec.TokenRef.Kind, rec.TokenRef.Name, tok), "the ref is the cached token's own")
		})
	}
}

// TestTheCachedStoredLoginIsNeverNamed asserts that a Rancher without ext
// tokens, where the plugin caches the stored login token itself, leaves a
// record with no ref and no owner: the plugin did not create that token.
func TestTheCachedStoredLoginIsNeverNamed(t *testing.T) {
	r := newRecordFixtureOpts(t, false, testrancher.Options{Legacy: true})
	r.login("prod", "", "alice", false)
	_, err := r.call("prod", false)
	require.NoError(t, err)

	rec := cachedInfo(t, r.dir, "prod")
	assert.Nil(t, rec.TokenRef)
	assert.Empty(t, rec.TokenUserID)
}

// TestRecordBindingNamesOnlyAKnownCreate asserts that a cache record names
// its token only for a create the client identified: an unidentified
// create, a create without a ref and a token the call did not create
// leave the record unnamed.
func TestRecordBindingNamesOnlyAKnownCreate(t *testing.T) {
	t.Parallel()
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-1"}
	for _, tc := range []struct {
		name   string
		create *CreateResult
		named  bool
	}{
		{"a known create", &CreateResult{Outcome: CreateKnown, Ref: ref, UserID: "user-a"}, true},
		{"an unidentified create", &CreateResult{Outcome: CreateUnidentified, Ref: ref}, false},
		{"a known create without a ref", &CreateResult{Outcome: CreateKnown}, false},
		{"a token the call did not create", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tok := Token{binding: config.CacheBindingForTest(config.CanonicalDir(dir), "prod", "b1")}
			if tc.create != nil {
				tok.fresh = &freshToken{create: *tc.create}
			}
			store := config.NewFileStore(dir)
			require.NoError(t, store.SetCachedCred("k", "ext/token-1:s", time.Now().Add(time.Hour), "k", "https://r.ex", "c-1", tok.recordBinding()))
			rec := cachedInfo(t, dir, "k")
			if tc.named {
				assert.Equal(t, ref, rec.TokenRef)
				assert.Equal(t, "user-a", rec.TokenUserID)
				return
			}
			assert.Nil(t, rec.TokenRef)
		})
	}
}
