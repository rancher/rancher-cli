package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveFromFile(t *testing.T) {
	store := NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken("acme", "id:secret"))
	c := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "acme", Cluster: "c-x", Project: "p-y"}},
		Servers:        map[string]Server{"acme": {URL: "https://r.ex", CA: "PEM"}},
	}
	t.Run("file values", func(t *testing.T) {
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex", eff.URL)
		assert.Equal(t, "id:secret", eff.Token)
		assert.Equal(t, "c-x", eff.Cluster)
	})
	t.Run("env token bypasses store and overrides", func(t *testing.T) {
		t.Setenv("RANCHER_TOKEN", "env-id:env-secret")
		t.Setenv("RANCHER_CONTEXT", "c-env:p-env")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "env-id:env-secret", eff.Token)
		assert.Equal(t, "c-env", eff.Cluster)
		assert.Equal(t, "p-env", eff.Project)
	})
}

func TestEffectiveEnvOnlyNoFile(t *testing.T) {
	t.Setenv("RANCHER_URL", "https://env.ex")
	t.Setenv("RANCHER_TOKEN", "id:sec")
	eff, err := (&Config{}).Effective(NewFileStore(t.TempDir()))
	require.NoError(t, err)
	assert.Equal(t, "https://env.ex", eff.URL)
	assert.Equal(t, "id:sec", eff.Token)
}
