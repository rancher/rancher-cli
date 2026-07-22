package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
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

func (s stubStore) GetToken(ServerRef) (string, error) { return s.token, s.tokenErr }
func (s stubStore) SetToken(ServerRef, string) error   { return nil }
func (s stubStore) DeleteToken(ServerRef) error        { return nil }
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
	require.NoError(t, store.SetToken(ServerRef{Name: "acme"}, "id:secret"))
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
		t.Setenv("RANCHER_CLI_TOKEN", "env-id:env-secret")
		t.Setenv("RANCHER_CLI_CONTEXT", "c-env:p-env")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "env-id:env-secret", eff.Token)
		assert.Equal(t, "c-env", eff.Cluster)
		assert.Equal(t, "p-env", eff.Project)
	})
	t.Run("bare RANCHER_CLI_CONTEXT (no colon) overrides cluster only, keeps file project", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONTEXT", "c-env")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-env", eff.Cluster)
		assert.Equal(t, "p-y", eff.Project, "colon-less env value must not clobber the file's project axis")
	})
}

// Round-3 repro: a RANCHER_CLI_URL override pointing at a DIFFERENT server than
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
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "id:secret"))
	c := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "prod"}},
		Servers:        map[string]Server{"prod": {URL: "https://prod.example.com", CA: "PROD-CA"}},
	}

	t.Run("divergent RANCHER_CLI_URL clears the stored CA and server name", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_URL", "https://staging.example.com")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "https://staging.example.com", eff.URL)
		assert.Empty(t, eff.CA, "must never pair the stored CA with a server it was never issued for")
		assert.Empty(t, eff.ServerName, "must not carry the stored display name to a different server")
	})

	t.Run("RANCHER_CLI_CACERT still applies after a divergent RANCHER_CLI_URL clears the stored CA", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_URL", "https://staging.example.com")
		t.Setenv("RANCHER_CLI_CACERT", "STAGING-CA")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "STAGING-CA", eff.CA)
	})

	t.Run("trailing slash on the same server is not divergence", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_URL", "https://prod.example.com/")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "PROD-CA", eff.CA, "same server (trailing slash) must keep the stored CA")
		assert.Equal(t, "prod", eff.ServerName)
	})

	t.Run("case-different scheme/host on the same server is not divergence", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_URL", "HTTPS://PROD.example.com")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "PROD-CA", eff.CA)
		assert.Equal(t, "prod", eff.ServerName)
	})

	t.Run("no RANCHER_CLI_URL at all is unaffected", func(t *testing.T) {
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
	t.Setenv("RANCHER_CLI_URL", "https://env.ex")
	t.Setenv("RANCHER_CLI_TOKEN", "id:sec")
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
		t.Setenv("RANCHER_CLI_CONTEXT", "c-env:p-env")
		SetContextOverride("c-flag:p-flag")
		t.Cleanup(func() { SetContextOverride("") })
		eff, err := cfg.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "c-flag", eff.Cluster)
		assert.Equal(t, "p-flag", eff.Project)
	})

	t.Run("no override falls back to env then file", func(t *testing.T) {
		SetContextOverride("")
		t.Setenv("RANCHER_CLI_CONTEXT", "")
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

// TestResolveDirWindowsDefault exercises the GOOS=windows final-fallback
// branch cross-platform via the goos seam: with no flag/env/XDG, the
// default must be os.UserConfigDir()/rancher (=%APPDATA%\rancher on a
// real Windows host) instead of ~/.config/rancher.
func TestResolveDirWindowsDefault(t *testing.T) {
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })

	got, err := ResolveDir("")
	require.NoError(t, err)
	ucd, err := os.UserConfigDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ucd, "rancher"), got)
}

// TestResolveDirWindowsPrecedenceUnchanged pins that the Windows branch
// sits BELOW flag/env/XDG: an explicit XDG_CONFIG_HOME still wins.
func TestResolveDirWindowsPrecedenceUnchanged(t *testing.T) {
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "xdg"))
	orig := goos
	goos = "windows"
	t.Cleanup(func() { goos = orig })

	got, err := ResolveDir("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "rancher"), got)
}

func TestReadOnlyOverrideStash(t *testing.T) {
	t.Cleanup(func() { SetReadOnlyOverride(false, nil) })

	active, sources := ReadOnlyOverride()
	assert.False(t, active, "default stash state is inactive")
	assert.Empty(t, sources)

	SetReadOnlyOverride(true, []string{"config readOnly", "--read-only"})
	active, sources = ReadOnlyOverride()
	assert.True(t, active)
	assert.Equal(t, []string{"config readOnly", "--read-only"}, sources)
}

// effectiveTestConfig is a one-server config with a resolving current context.
func effectiveTestConfig(name, url string) *Config {
	return &Config{
		CurrentContext: name,
		Servers:        map[string]Server{name: {URL: url}},
		Contexts:       map[string]Context{name: {Server: name}},
	}
}

// recordingStore serves one token and counts GetToken calls; the other
// CredStore methods are inherited from an embedded file store that is never
// reached in these tests.
type recordingStore struct {
	CredStore
	name, token string
	getCalls    int
}

func (r *recordingStore) GetToken(ref ServerRef) (string, error) {
	r.getCalls++
	if ref.Name == r.name {
		return r.token, nil
	}
	return "", ErrNoToken
}

// failingStore errors on every token read — stands in for a broken helper.
type failingStore struct{ CredStore }

func (failingStore) GetToken(ServerRef) (string, error) {
	return "", errors.New("helper exploded")
}

func TestEffectiveEnvTokenSkipsStore(t *testing.T) {
	t.Setenv(EnvToken, "env-id:env-secret")
	cfg := effectiveTestConfig("prod", "https://rancher.example.com")
	eff, err := cfg.Effective(failingStore{})
	require.NoError(t, err, "env token must bypass the store entirely")
	assert.Equal(t, "env-id:env-secret", eff.Token)
}

func TestEffectiveDivergentURLDoesNotReadStoredToken(t *testing.T) {
	t.Setenv(EnvURL, "https://other.example.com")
	cfg := effectiveTestConfig("prod", "https://rancher.example.com")
	st := &recordingStore{name: "prod", token: "stored-id:stored-secret"}
	eff, err := cfg.Effective(st)
	require.NoError(t, err)
	assert.Empty(t, eff.Token, "stored token must not leak to a divergent host")
	assert.Zero(t, st.getCalls, "store must not be read at all on divergence")
}

func TestEffectiveEquivalentURLStillReadsToken(t *testing.T) {
	t.Setenv(EnvURL, "HTTPS://RANCHER.example.com:443")
	cfg := effectiveTestConfig("prod", "https://rancher.example.com")
	st := &recordingStore{name: "prod", token: "stored-id:stored-secret"}
	eff, err := cfg.Effective(st)
	require.NoError(t, err)
	assert.Equal(t, "stored-id:stored-secret", eff.Token)
}

func TestSameServerURLMatchesCanonicalForm(t *testing.T) {
	t.Parallel()
	// Equivalence per SameServerURL must be exactly equality of
	// credhelper.CanonicalServerURL outputs — one algorithm, no drift.
	a, b := "HTTPS://EXAMPLE.com:443/path", "https://example.com"
	ca, err := credhelper.CanonicalServerURL(a)
	require.NoError(t, err)
	cb, err := credhelper.CanonicalServerURL(b)
	require.NoError(t, err)
	assert.Equal(t, ca, cb)
	assert.True(t, SameServerURL(a, b))
	// Malformed inputs fall back to exact equality.
	assert.True(t, SameServerURL("not a url", "not a url"))
	assert.False(t, SameServerURL("not a url", "https://example.com"))
}
