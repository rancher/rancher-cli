package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveCA(t *testing.T) {
	t.Parallel()
	pem := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	got, err := ResolveCA(pem)
	require.NoError(t, err)
	assert.Equal(t, pem, got, "inline PEM passes through")

	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte(pem), 0600))
	got, err = ResolveCA(path)
	require.NoError(t, err)
	assert.Equal(t, pem, got, "path is read to PEM")

	got, err = ResolveCA("")
	require.NoError(t, err)
	assert.Empty(t, got)
}

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
	t.Run("bare RANCHER_CONTEXT (no colon) overrides cluster only, keeps file project", func(t *testing.T) {
		t.Setenv("RANCHER_CONTEXT", "c-env")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-env", eff.Cluster)
		assert.Equal(t, "p-y", eff.Project, "colon-less env value must not clobber the file's project axis")
	})
}

// Round-3 repro: a RANCHER_URL override pointing at a DIFFERENT server than
// the stored context's must never carry that stored server's CA or display
// name into the new Effective — every command through Effective (get,
// create, patch, delete, auth check, kubeconfig) shares this one choke
// point, not just kubeconfig.
// Round-4 repro: an explicit default port (:443 https, :80 http) spelled out
// on one side and omitted on the other is the same server, not divergence —
// the third normalization axis to recur here (after case, then trailing
// slash), pinned in one exported matrix so a fourth lands in one place too.
func TestSameServerURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "https://r.ex", "https://r.ex", true},
		{"trailing slash", "https://r.ex", "https://r.ex/", true},
		{"scheme case", "HTTPS://r.ex", "https://r.ex", true},
		{"host case", "https://R.EX", "https://r.ex", true},
		{"different host", "https://r.ex", "https://other.ex", false},
		{"different scheme", "https://r.ex", "http://r.ex", false},

		{"implicit vs explicit default https port", "https://r.ex", "https://r.ex:443", true},
		{"implicit vs explicit default http port", "http://r.ex", "http://r.ex:80", true},
		{"explicit non-default port vs none", "https://r.ex", "https://r.ex:8443", false},
		{"explicit default port on the wrong scheme is not default", "https://r.ex:80", "https://r.ex", false},
		{"both explicit default port", "https://r.ex:443", "https://r.ex:443", true},
		{"both explicit same non-default port", "https://r.ex:8443", "https://r.ex:8443", true},

		{"IPv6 no port both sides", "https://[::1]", "https://[::1]", true},
		{"IPv6 implicit vs explicit default port", "https://[::1]", "https://[::1]:443", true},
		{"IPv6 explicit non-default port vs none", "https://[::1]", "https://[::1]:8443", false},
		{"IPv6 both explicit same non-default port", "https://[::1]:8443", "https://[::1]:8443", true},

		{"empty vs non-empty (no stored context)", "https://r.ex", "", false},
		{"both empty", "", "", true},
		{"malformed falls back to exact equality — mismatch", "not a url: ::", "https://r.ex", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, SameServerURL(tt.a, tt.b), "%q vs %q", tt.a, tt.b)
		})
	}
}

func TestEffectiveClearsCAAndNameOnDivergentEnvURL(t *testing.T) {
	store := NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken("prod", "id:secret"))
	c := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "prod"}},
		Servers:        map[string]Server{"prod": {URL: "https://prod.example.com", CA: "PROD-CA"}},
	}

	t.Run("divergent RANCHER_URL clears the stored CA and server name", func(t *testing.T) {
		t.Setenv("RANCHER_URL", "https://staging.example.com")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "https://staging.example.com", eff.URL)
		assert.Empty(t, eff.CA, "must never pair the stored CA with a server it was never issued for")
		assert.Empty(t, eff.ServerName, "must not carry the stored display name to a different server")
	})

	t.Run("RANCHER_CACERT still applies after a divergent RANCHER_URL clears the stored CA", func(t *testing.T) {
		t.Setenv("RANCHER_URL", "https://staging.example.com")
		t.Setenv("RANCHER_CACERT", "STAGING-CA")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "STAGING-CA", eff.CA)
	})

	t.Run("trailing slash on the same server is not divergence", func(t *testing.T) {
		t.Setenv("RANCHER_URL", "https://prod.example.com/")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "PROD-CA", eff.CA, "same server (trailing slash) must keep the stored CA")
		assert.Equal(t, "prod", eff.ServerName)
	})

	t.Run("case-different scheme/host on the same server is not divergence", func(t *testing.T) {
		t.Setenv("RANCHER_URL", "HTTPS://PROD.example.com")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "PROD-CA", eff.CA)
		assert.Equal(t, "prod", eff.ServerName)
	})

	t.Run("no RANCHER_URL at all is unaffected", func(t *testing.T) {
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "PROD-CA", eff.CA)
		assert.Equal(t, "prod", eff.ServerName)
	})
}

func TestSplitContext(t *testing.T) {
	t.Parallel()
	t.Run("cluster and project", func(t *testing.T) {
		cluster, project, hasProject := SplitContext("c-1:p-1")
		assert.Equal(t, "c-1", cluster)
		assert.Equal(t, "p-1", project)
		assert.True(t, hasProject)
	})
	t.Run("bare cluster has no project", func(t *testing.T) {
		cluster, project, hasProject := SplitContext("c-1")
		assert.Equal(t, "c-1", cluster)
		assert.Empty(t, project)
		assert.False(t, hasProject)
	})
	t.Run("trailing colon explicitly clears the project", func(t *testing.T) {
		cluster, project, hasProject := SplitContext("c-1:")
		assert.Equal(t, "c-1", cluster)
		assert.Empty(t, project)
		assert.True(t, hasProject, "a trailing colon must still report hasProject so callers can clear the axis")
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

func TestEffectiveContextOverridePrecedence(t *testing.T) {
	store := NewFileStore(t.TempDir())
	cfg := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "acme", Cluster: "c-file", Project: "p-file"}},
		Servers:        map[string]Server{"acme": {URL: "https://r.ex"}},
	}

	t.Run("flag beats env and file", func(t *testing.T) {
		t.Setenv("RANCHER_CONTEXT", "c-env:p-env")
		SetContextOverride("c-flag:p-flag")
		t.Cleanup(func() { SetContextOverride("") })
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-flag", eff.Cluster)
		assert.Equal(t, "p-flag", eff.Project)
	})

	t.Run("no override falls back to env then file", func(t *testing.T) {
		SetContextOverride("")
		t.Setenv("RANCHER_CONTEXT", "")
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-file", eff.Cluster)
	})

	t.Run("--context cluster: (trailing colon) explicitly clears the project axis", func(t *testing.T) {
		SetContextOverride("c-flag:")
		t.Cleanup(func() { SetContextOverride("") })
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-flag", eff.Cluster)
		assert.Empty(t, eff.Project, "a trailing colon must clear the project even though the file/context has one")
	})
}
