package config

import (
	"testing"

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
