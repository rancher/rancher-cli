package kube

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestBaseURL(t *testing.T) {
	cases := map[string]string{
		"https://r.ex/v3":      "https://r.ex",
		"https://r.ex":         "https://r.ex",
		"https://r.ex/v3/":     "https://r.ex",
		"https://r.ex:8443/v3": "https://r.ex:8443",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, BaseURL(in))
		})
	}
}

const testCAPEM = "-----BEGIN CERTIFICATE-----\nPEM\n-----END CERTIFICATE-----\n"

func TestRESTConfigDefaultsAndAuth(t *testing.T) {
	t.Run("explicit cluster + inline PEM CA", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{
			URL: "https://r.ex/v3", Token: "id:secret", CA: testCAPEM,
		}, "c-9")
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex/k8s/clusters/c-9", rc.Host)
		assert.Equal(t, "id:secret", rc.BearerToken)
		assert.Equal(t, []byte(testCAPEM), rc.CAData)
	})
	t.Run("explicit local cluster produces local URL", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{URL: "https://r.ex", Token: "t", CA: testCAPEM}, "local")
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex/k8s/clusters/local", rc.Host)
	})
	t.Run("empty cluster id returns VALIDATION", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{URL: "https://r.ex", Token: "t"}, "")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
	// Round-4 repro: a stored `--cacert /path/to/ca.pem` login (a path, not
	// inline PEM) went straight to CAData as raw bytes, so client-go tried to
	// use the literal path string as a certificate and every cluster command
	// failed to parse it. eff.CA must resolve through config.ResolveCA, same
	// as kubeconfig.go/token.go already do for the identical pem-or-path value.
	t.Run("path-valued CA is read and resolved to its PEM bytes", func(t *testing.T) {
		dir := t.TempDir()
		caPath := filepath.Join(dir, "ca.pem")
		require.NoError(t, os.WriteFile(caPath, []byte(testCAPEM), 0o600))

		rc, err := RESTConfig(config.Effective{URL: "https://r.ex", Token: "t", CA: caPath}, "local")
		require.NoError(t, err)
		assert.Equal(t, []byte(testCAPEM), rc.CAData, "path-valued CA must be read from disk, not used as raw CAData")
	})
	t.Run("unreadable path-valued CA surfaces an error", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{URL: "https://r.ex", Token: "t", CA: filepath.Join(t.TempDir(), "does-not-exist.pem")}, "local")
		require.Error(t, err)
	})
	t.Run("no CA uses system roots (secure by default)", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{
			URL: "https://r.ex", Token: "id:secret",
		}, "c-9")
		require.NoError(t, err)
		assert.False(t, rc.Insecure)
		assert.Nil(t, rc.CAData)
	})
	t.Run("empty URL with no known servers returns NOT_LOGGED_IN with no hint", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{Token: "t", CA: "PEM"}, "local")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
		assert.Empty(t, ce.Hint, "the name and URL are unknown on a blank machine; guidance belongs in the body")
	})
	t.Run("configured tokenless server returns NOT_LOGGED_IN with a concrete login hint (fresh add / post-logout)", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{ServerName: "qa1", URL: "https://r.ex", CA: "PEM"}, "local")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
		assert.Equal(t, "rancher auth login 'qa1'", ce.Hint)
	})
	t.Run("env-only tokenless URL on a blank machine stays AUTH with no hint", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{URL: "https://r.ex", CA: "PEM"}, "local")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, ce.Hint, "an env-only URL carries no name to interpolate")
		assert.Contains(t, ce.Message, config.EnvToken)
	})
	t.Run("env-only tokenless URL overriding a different configured current server stays AUTH with no hint", func(t *testing.T) {
		// Effective clears ServerName when RANCHER_CLI_URL diverges from the
		// configured current server (resolve.go) — RESTConfig must not
		// resurrect a name that Effective deliberately dropped.
		_, err := RESTConfig(config.Effective{URL: "https://env.ex", CA: "PEM"}, "local")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, ce.Hint)
	})
}

// TestHTTPClientDoesNotMutateDefaultClient pins that the process-global
// default client never receives this policy, whatever client-go hands
// back for a bare config: the returned client is a distinct object with
// the policy, and the global's field is untouched. Not parallel: it reads
// and restores a global.
func TestHTTPClientDoesNotMutateDefaultClient(t *testing.T) {
	saved := http.DefaultClient.CheckRedirect
	t.Cleanup(func() { http.DefaultClient.CheckRedirect = saved })

	hc, err := HTTPClient(&rest.Config{})
	require.NoError(t, err)
	assert.NotSame(t, http.DefaultClient, hc)
	assert.NotNil(t, hc.CheckRedirect)
	assert.Nil(t, http.DefaultClient.CheckRedirect, "the shared default client must be untouched")
}

// TestHTTPClientCarriesPolicyWithCustomTransport pins that a config with a
// CA and timeout gets a dedicated client that still carries the policy and
// the timeout.
func TestHTTPClientCarriesPolicyWithCustomTransport(t *testing.T) {
	t.Parallel()
	rc := &rest.Config{Host: "https://rancher.example", Timeout: 3 * time.Second}
	rc.TLSClientConfig = rest.TLSClientConfig{Insecure: true}
	hc, err := HTTPClient(rc)
	require.NoError(t, err)
	assert.NotNil(t, hc.CheckRedirect)
	assert.Equal(t, 3*time.Second, hc.Timeout)
}

// TestHTTPClientSendsKubernetesUserAgent pins that requests through a
// kube-built client identify themselves: client-go installs its User-Agent
// only inside HTTPClientFor and only when the config names one, so the
// default must be applied before the client is built.
func TestHTTPClientSendsKubernetesUserAgent(t *testing.T) {
	t.Parallel()
	var agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	hc, err := HTTPClient(&rest.Config{Host: srv.URL})
	require.NoError(t, err)
	resp, err := hc.Get(srv.URL + "/version")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.NotEmpty(t, agent)
	assert.False(t, strings.HasPrefix(agent, "Go-http-client"), "the default Go agent must not leak: %q", agent)
	assert.Equal(t, rest.DefaultKubernetesUserAgent(), agent)
}
