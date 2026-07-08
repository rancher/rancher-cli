package config

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubStore is a CredStore whose GetToken returns a fixed token/error, for
// exercising Effective's credential-store error handling.
type stubStore struct {
	token    string
	tokenErr error
}

func (s stubStore) GetToken(string) (string, error) { return s.token, s.tokenErr }
func (s stubStore) SetToken(string, string) error   { return nil }
func (s stubStore) DeleteToken(string) error        { return nil }
func (s stubStore) GetCachedCred(string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (s stubStore) SetCachedCred(string, string, time.Time, string, string, string) error {
	return nil
}
func (s stubStore) DeleteCachedCred(string) error              { return nil }
func (s stubStore) DeleteAllCachedCreds() error                { return nil }
func (s stubStore) ListCachedCreds() ([]CachedCredInfo, error) { return nil, nil }

func TestEffectiveSurfacesStoreError(t *testing.T) {
	t.Parallel()
	c := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "acme"}},
		Servers:        map[string]Server{"acme": {URL: "https://r.ex"}},
	}
	t.Run("non-ErrNoToken failure surfaces as credential store error", func(t *testing.T) {
		t.Parallel()
		_, err := c.Effective(stubStore{tokenErr: errors.New("keyring: dbus unavailable")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "credential store:")
	})
	t.Run("ErrNoToken leaves an empty token and no error", func(t *testing.T) {
		t.Parallel()
		eff, err := c.Effective(stubStore{tokenErr: ErrNoToken})
		require.NoError(t, err)
		assert.Empty(t, eff.Token)
	})
}

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
