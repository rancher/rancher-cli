package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/clierr"
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
func (s stubStore) SetTokenWithOutcome(ServerRef, string) (CommitOutcome, error) {
	return CommitOutcome{Backend: clierr.BackendFile, State: CommitCommitted, PriorSource: clierr.PriorNone}, nil
}
func (s stubStore) DeleteToken(ServerRef) error { return nil }
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
		CurrentServer: "acme",
		Servers:       map[string]Server{"acme": {URL: "https://r.ex"}},
	}
	t.Run("non-ErrNoToken failure surfaces as credential store error", func(t *testing.T) {
		t.Parallel()
		_, err := c.Effective(stubStore{tokenErr: errors.New("keyring: dbus unavailable")})
		require.Error(t, err)
		assert.ErrorContains(t, err, "credential store:")
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
		CurrentServer: "acme",
		Servers:       map[string]Server{"acme": {URL: "https://r.ex", CA: "PEM"}},
	}
	t.Run("file values", func(t *testing.T) {
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex", eff.URL)
		assert.Equal(t, "id:secret", eff.Token)
	})
	t.Run("env token bypasses store and overrides", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_TOKEN", "env-id:env-secret")
		eff, err := c.Effective(store)
		require.NoError(t, err)
		assert.Equal(t, "env-id:env-secret", eff.Token)
	})
}

// Round-3 repro: a RANCHER_CLI_URL override pointing at a DIFFERENT server than
// the stored context's must never carry that stored server's CA or display
// name into the new Effective — every command through Effective (get,
// create, patch, delete, auth status, kubeconfig) shares this one choke
// point, not just kubeconfig.
// Round-4 repro: an explicit default port (:443 https, :80 http) spelled out
// on one side and omitted on the other is the same server, not divergence —
// the third normalization axis to recur here (after case, then trailing
// slash), asserted in one exported matrix so a fourth lands in one place too.
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
		CurrentServer: "prod",
		Servers:       map[string]Server{"prod": {URL: "https://prod.example.com", CA: "PROD-CA"}},
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

func TestEffectiveEnvOnlyNoFile(t *testing.T) {
	t.Setenv("RANCHER_CLI_URL", "https://env.ex")
	t.Setenv("RANCHER_CLI_TOKEN", "id:sec")
	eff, err := (&Config{}).Effective(NewFileStore(t.TempDir()))
	require.NoError(t, err)
	assert.Equal(t, "https://env.ex", eff.URL)
	assert.Equal(t, "id:sec", eff.Token)
}

// TestEffectiveUnsupportedSchemeSkipsStore is the final-re-review finding-2
// repro at the config-package level: Config.Effective must never read the
// credential store when the configured server URL uses an unsupported scheme.
// The token is left empty and no error is returned; the helper is never invoked.
func TestEffectiveUnsupportedSchemeSkipsStore(t *testing.T) {
	cfg := effectiveTestConfig("prod", "ftp://host")
	st := &recordingStore{name: "prod", token: "stored-token"}
	eff, err := cfg.Effective(st)
	require.NoError(t, err, "unsupported scheme must not cause an error from Effective")
	assert.Empty(t, eff.Token, "ftp:// URL must not trigger a store read")
	assert.Zero(t, st.getCalls, "store.GetToken must not be called for unsupported schemes")
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

// TestResolveDirWindowsPrecedenceUnchanged asserts that the Windows branch
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

// effectiveTestConfig is a one-server config with the current-server pointer set.
func effectiveTestConfig(name, url string) *Config {
	return &Config{
		CurrentServer: name,
		Servers:       map[string]Server{name: {URL: url}},
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

// TestEffectiveKeepsKnownServersWithResolvedContext asserts the server
// inventory survives context resolution: the resolved-context branch used
// to rebuild eff as a struct literal, zeroing KnownServers. The inventory
// must be present in BOTH states — resolved
// context and no context — because kube.RESTConfig consumes it exactly when
// the URL came out empty (e.g. a hand-edited server entry with no URL).
func TestEffectiveKeepsKnownServersWithResolvedContext(t *testing.T) {
	store := NewFileStore(t.TempDir())
	c := &Config{
		CurrentServer: "acme",
		Servers: map[string]Server{
			"acme":  {URL: "https://r.ex"},
			"other": {URL: "https://o.ex"},
		},
	}
	eff, err := c.Effective(store)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"acme": "https://r.ex", "other": "https://o.ex"},
		eff.KnownServers, "inventory survives the resolved-context branch")

	c.CurrentServer = ""
	eff, err = c.Effective(store)
	require.NoError(t, err)
	assert.Equal(t, "", eff.URL)
	assert.Len(t, eff.KnownServers, 2, "inventory present when no current server resolves")
}
