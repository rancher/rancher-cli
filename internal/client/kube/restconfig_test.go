package kube

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestRESTConfigDefaultsAndAuth(t *testing.T) {
	t.Run("explicit cluster + CA", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{
			URL: "https://r.ex/v3", Token: "id:secret", CA: "PEM", Cluster: "c-9",
		})
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex/k8s/clusters/c-9", rc.Host)
		assert.Equal(t, "id:secret", rc.BearerToken)
		assert.Equal(t, []byte("PEM"), rc.CAData)
	})
	t.Run("empty cluster defaults to local", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{URL: "https://r.ex", Token: "t", CA: "PEM"})
		require.NoError(t, err)
		assert.Equal(t, "https://r.ex/k8s/clusters/local", rc.Host)
	})
	t.Run("no CA uses system roots (secure by default)", func(t *testing.T) {
		rc, err := RESTConfig(config.Effective{
			URL: "https://r.ex", Token: "id:secret", Cluster: "c-9",
		})
		require.NoError(t, err)
		assert.False(t, rc.Insecure)
		assert.Nil(t, rc.CAData)
	})
	t.Run("empty URL returns CONTEXT_NOT_SET with a login-or-env hint", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{Token: "t", CA: "PEM"})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
		assert.Equal(t, config.LoginOrEnvHint, ce.Hint)
	})
	t.Run("empty token returns error", func(t *testing.T) {
		_, err := RESTConfig(config.Effective{URL: "https://r.ex", CA: "PEM"})
		assert.ErrorContains(t, err, "no token")
	})
}
