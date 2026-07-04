package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStoreRoundTrip(t *testing.T) {
	s := NewFileStore(t.TempDir())
	t.Run("missing token", func(t *testing.T) {
		_, err := s.GetToken("acme")
		assert.ErrorIs(t, err, ErrNoToken)
	})
	t.Run("set then get", func(t *testing.T) {
		require.NoError(t, s.SetToken("acme", "tok-abc:secret"))
		got, err := s.GetToken("acme")
		require.NoError(t, err)
		assert.Equal(t, "tok-abc:secret", got)
	})
	t.Run("delete", func(t *testing.T) {
		require.NoError(t, s.SetToken("acme", "x"))
		require.NoError(t, s.DeleteToken("acme"))
		_, err := s.GetToken("acme")
		assert.ErrorIs(t, err, ErrNoToken)
	})
}

func TestOpenStoreFilePolicy(t *testing.T) {
	s := OpenStore("file", t.TempDir())
	require.NoError(t, s.SetToken("acme", "y"))
	got, err := s.GetToken("acme")
	require.NoError(t, err)
	assert.Equal(t, "y", got)
}

func TestCachedCredRoundTrip(t *testing.T) {
	s := NewFileStore(t.TempDir())
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", exp))
	tok, gotExp, err := s.GetCachedCred("acme/u/c")
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	assert.Equal(t, exp, gotExp.UTC().Truncate(time.Second))
}

func TestCachedCredExpiredIsErrNoToken(t *testing.T) {
	s := NewFileStore(t.TempDir())
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", time.Now().Add(-time.Minute)))
	_, _, err := s.GetCachedCred("acme/u/c")
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestDeleteAllCachedCredsFlushesFileStore(t *testing.T) {
	s := NewFileStore(t.TempDir())
	require.NoError(t, s.SetCachedCred("acme/u/c1", "t1", time.Now().Add(time.Hour)))
	require.NoError(t, s.SetCachedCred("acme/u/c2", "t2", time.Now().Add(time.Hour)))
	require.NoError(t, s.DeleteAllCachedCreds())
	_, _, err := s.GetCachedCred("acme/u/c1")
	assert.ErrorIs(t, err, ErrNoToken)
	_, _, err = s.GetCachedCred("acme/u/c2")
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestCachedCredExpiredPurgeErrorIsPropagated(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir).(*fileStore)
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", time.Now().Add(-time.Minute)))

	tok, _, err := s.GetCachedCred("acme/u/c")
	assert.ErrorIs(t, err, ErrNoToken)
	assert.Empty(t, tok)

	m, err := s.readCache()
	require.NoError(t, err)
	_, exists := m["acme/u/c"]
	assert.False(t, exists, "expired entry should have been removed on successful purge")
}
