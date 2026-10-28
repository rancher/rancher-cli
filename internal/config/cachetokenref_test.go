package config

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestACacheRecordNamesTheTokenItsBindingCarries asserts that a record
// written under a binding that names the created token carries its ref and
// owner on every store, and that the listing returns them; a binding that
// names none writes a record without either.
func TestACacheRecordNamesTheTokenItsBindingCarries(t *testing.T) {
	keyring.MockInit()
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}
	for _, tc := range []struct {
		name  string
		store func(dir string) CredStore
	}{
		{"file", func(dir string) CredStore { return NewFileStore(dir) }},
		{"keychain", func(dir string) CredStore { return newPinnedKeyringStore(dir, false) }},
		{"helper", func(dir string) CredStore { return newExecStore("fake", dir) }},
		{"lazy", func(dir string) CredStore {
			lazy, err := NewLazyStore(&Config{CredentialStore: "file"}, dir, false)
			require.NoError(t, err)
			return lazy
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := tc.store(dir)
			b := CacheBindingForTest(CanonicalDir(dir), "prod", "b1")
			require.NoError(t, store.SetCachedCred("named", "ext/token-abc:s", time.Now().Add(time.Hour), "named", "https://r.ex", "c-1",
				b.WithCreatedToken(ref, "user-alice")))
			require.NoError(t, store.SetCachedCred("unnamed", "t2", time.Now().Add(time.Hour), "unnamed", "https://r.ex", "c-1", b))

			creds, err := NewFileStore(dir).ListCachedCreds()
			require.NoError(t, err)
			require.Len(t, creds, 2)
			byKey := map[string]CachedCredInfo{}
			for _, c := range creds {
				byKey[c.Key] = c
			}
			assert.Equal(t, ref, byKey["named"].TokenRef)
			assert.Equal(t, "user-alice", byKey["named"].TokenUserID)
			assert.Nil(t, byKey["unnamed"].TokenRef)
			assert.Empty(t, byKey["unnamed"].TokenUserID)
		})
	}
}

// TestWithCreatedTokenKeepsTheBinding asserts that naming the token
// changes nothing a hit or a pause judges: the entry, its id and the
// refusal of a paused binding stay as they were.
func TestWithCreatedTokenKeepsTheBinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1", CacheWritesPaused: PauseLocked}}}
	b := CacheBindingFor(CanonicalDir(dir), cfg, "prod").WithCreatedToken(&clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}, "u")
	assert.Equal(t, "prod", b.Entry())
	assert.Equal(t, "b1", b.BindingID())
	require.ErrorIs(t, NewFileStore(dir).SetCachedCred("k", "tok", time.Now().Add(time.Hour), "n", "https://r.ex", "c", b), ErrCacheWritesPaused)
}
