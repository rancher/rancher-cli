package provider

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

// redirectingServer is a TLS server that answers every request with a
// redirect of status to elsewhere, and elsewhere counts what it receives.
func redirectingServer(t *testing.T, status int) (srv *httptest.Server, caPEM string, elsewhereHits *atomic.Int32) {
	t.Helper()
	elsewhereHits = &atomic.Int32{}
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhereHits.Add(1) }))
	t.Cleanup(elsewhere.Close)
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, status)
	}))
	t.Cleanup(srv.Close)
	caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return srv, caPEM, elsewhereHits
}

// TestPreflightRefusesCrossOriginRedirect asserts the user-visible contract
// through the public path: a redirected preflight is a non-retryable
// GENERIC refusal naming both origins, never the retryable "cannot reach
// server" connectivity wording, and nothing reaches the other origin.
func TestPreflightRefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	srv, caPEM, hits := redirectingServer(t, http.StatusTemporaryRedirect)
	eff := config.Effective{URL: srv.URL, CA: caPEM, Token: "id:secret"}
	_, err := Preflight(t.Context(), eff)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "refused to follow a redirect from "+srv.URL)
	assert.NotContains(t, err.Error(), "cannot reach server")
	assert.Zero(t, hits.Load())
}

// Round-2 repro: Preflight (behind `auth status`) used to rewrap every
// RESTConfig error as AUTH, so `auth status` reported a different code/exit
// than every other command for the identical missing-context/missing-token
// state. RESTConfig's own typed errors must pass through unchanged.
func TestPreflightPassesThroughRESTConfigErrors(t *testing.T) {
	t.Parallel()
	t.Run("no context and no RANCHER_CLI_URL stays NOT_LOGGED_IN with no hint", func(t *testing.T) {
		t.Parallel()
		_, err := Preflight(t.Context(), config.Effective{})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
		assert.Empty(t, ce.Hint)
	})
	t.Run("configured tokenless server stays NOT_LOGGED_IN with a concrete login hint", func(t *testing.T) {
		t.Parallel()
		_, err := Preflight(t.Context(), config.Effective{ServerName: "qa1", URL: "https://r.ex"})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
		assert.Equal(t, "rancher auth login 'qa1'", ce.Hint)
	})
	t.Run("env-only URL resolved but no token stays AUTH with no hint", func(t *testing.T) {
		t.Parallel()
		_, err := Preflight(t.Context(), config.Effective{URL: "https://r.ex"})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, ce.Hint)
	})
}

func TestDiscoveryConfigBoundsTimeout(t *testing.T) {
	t.Parallel()
	in := &rest.Config{Host: "https://r.ex", BearerToken: "tok"}
	out := discoveryConfig(in)
	assert.Equal(t, requestTimeout, out.Timeout, "discovery config must carry the request timeout")
	assert.Equal(t, "https://r.ex", out.Host, "other fields must be preserved")
	assert.Zero(t, in.Timeout, "input config must not be mutated")
}

func TestMapPreflightErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantRetryable bool
	}{
		{
			name:     "unauthorized maps to AUTH",
			err:      apierrors.NewUnauthorized("bad token"),
			wantCode: clierr.CodeAuth,
		},
		{
			name:          "unreachable maps to GENERIC retryable",
			err:           errors.New("dial tcp: timeout"),
			wantCode:      clierr.CodeGeneric,
			wantRetryable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var ce *clierr.Error
			require.ErrorAs(t, mapPreflightErr(tt.err), &ce)
			assert.Equal(t, tt.wantCode, ce.Code)
			assert.Equal(t, tt.wantRetryable, ce.Retryable)
		})
	}
}

// TestPreflightRedirectLoopIsNotRetryable asserts that a same-origin redirect
// loop hitting the hop cap is rendered as the structured, non-retryable
// refusal, never as the retryable "cannot reach server" connectivity
// wording whose remedy (check the CA) cannot apply.
func TestPreflightRedirectLoopIsNotRetryable(t *testing.T) {
	t.Parallel()
	var srvURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvURL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	_, err := Preflight(t.Context(), config.Effective{URL: srv.URL, CA: caPEM, Token: "id:secret"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "stopped after 10 redirects from "+srv.URL)
	assert.NotContains(t, err.Error(), "cannot reach server")
}
