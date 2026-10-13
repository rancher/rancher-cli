package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const exchangePrefix = "failed to exchange authorization code for token"

// idpAnswer starts an identity provider's token endpoint that answers every
// request with handler and returns an oauth2 config pointing at it and the
// server's client. A call passes that client to oauth2 in its context under
// oauth2.HTTPClient: without it oauth2 sends through http.DefaultClient. The
// server is closed when the test ends.
func idpAnswer(t *testing.T, handler http.HandlerFunc) (*oauth2.Config, *http.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &oauth2.Config{
		ClientID: "cli",
		Endpoint: oauth2.Endpoint{TokenURL: srv.URL + "/token", DeviceAuthURL: srv.URL + "/device"},
	}, srv.Client()
}

// TestIdpErrorRefusedSignInIsAuth asserts a 4xx answer from the identity
// provider is AUTH with the step, the status and the provider's error code,
// and the status stays readable.
func TestIdpErrorRefusedSignInIsAuth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{"400 invalid grant", http.StatusBadRequest, "invalid_grant", exchangePrefix + ": the identity provider refused the sign-in (400): invalid_grant"},
		{"401 invalid client", http.StatusUnauthorized, "invalid_client", exchangePrefix + ": the identity provider refused the sign-in (401): invalid_client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, client := idpAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeTestJSON(w, tc.status, map[string]string{"error": tc.code})
			})
			_, err := cfg.Exchange(context.WithValue(t.Context(), oauth2.HTTPClient, client), "code")
			require.Error(t, err)
			got := idpError(err, cfg.Endpoint.TokenURL, exchangePrefix)
			ce, ok := errors.AsType[*clierr.Error](got)
			require.True(t, ok, "%v", got)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.False(t, ce.Retryable)
			st, ok := clierr.HTTPStatus(got)
			assert.True(t, ok)
			assert.Equal(t, tc.status, st)
		})
	}
}

// TestIdpErrorTimeoutAndRateLimitAreRetryable asserts a 408 or 429 answer
// from the identity provider is retryable GENERIC with the step, the status
// and the provider's error code, the status stays readable, and a 400 from
// the same provider stays AUTH and not retryable.
func TestIdpErrorTimeoutAndRateLimitAreRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		code      string
		want      string
		wantCode  string
		retryable bool
	}{
		{"408 timeout", http.StatusRequestTimeout, "", exchangePrefix + ": the identity provider refused the sign-in (408)", clierr.CodeGeneric, true},
		{"429 slow down", http.StatusTooManyRequests, "slow_down", exchangePrefix + ": the identity provider refused the sign-in (429): slow_down", clierr.CodeGeneric, true},
		{"400 stays AUTH", http.StatusBadRequest, "invalid_request", exchangePrefix + ": the identity provider refused the sign-in (400): invalid_request", clierr.CodeAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, client := idpAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.code == "" {
					w.WriteHeader(tc.status)
					return
				}
				writeTestJSON(w, tc.status, map[string]string{"error": tc.code})
			})
			_, err := cfg.Exchange(context.WithValue(t.Context(), oauth2.HTTPClient, client), "code")
			require.Error(t, err)
			got := idpError(err, cfg.Endpoint.TokenURL, exchangePrefix)
			ce, ok := errors.AsType[*clierr.Error](got)
			require.True(t, ok, "%v", got)
			assert.Equal(t, tc.wantCode, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Equal(t, tc.want, ce.Message)
			st, ok := clierr.HTTPStatus(got)
			assert.True(t, ok)
			assert.Equal(t, tc.status, st)
		})
	}
}

// TestIdpErrorServerFailureIsRetryable asserts a 5xx answer, with no body
// and with a text body, is GENERIC and retryable, worded by the status
// table, and the status stays readable.
func TestIdpErrorServerFailureIsRetryable(t *testing.T) {
	t.Parallel()
	const want = exchangePrefix + ": the server is currently unable to handle the request - retrying may succeed"
	for name, body := range map[string]string{"no body": "", "text body": "upstream is down\n"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, client := idpAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(body))
			})
			_, err := cfg.Exchange(context.WithValue(t.Context(), oauth2.HTTPClient, client), "code")
			require.Error(t, err)
			got := idpError(err, cfg.Endpoint.TokenURL, exchangePrefix)
			ce, ok := errors.AsType[*clierr.Error](got)
			require.True(t, ok, "%v", got)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable)
			assert.Equal(t, want, ce.Message)
			st, ok := clierr.HTTPStatus(got)
			assert.True(t, ok)
			assert.Equal(t, http.StatusServiceUnavailable, st)
		})
	}
}

// TestIdpErrorDeviceAuthRefusedIsAuth asserts the device endpoint's 4xx
// answer is AUTH under its own step.
func TestIdpErrorDeviceAuthRefusedIsAuth(t *testing.T) {
	t.Parallel()
	const prefix = "failed to initiate device authorization"
	cfg, client := idpAnswer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	})
	_, err := cfg.DeviceAuth(context.WithValue(t.Context(), oauth2.HTTPClient, client))
	require.Error(t, err)
	got := idpError(err, cfg.Endpoint.DeviceAuthURL, prefix)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok, "%v", got)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, prefix+": the identity provider refused the sign-in (400): invalid_request", ce.Message)
}

// TestIdpErrorOtherErrorsKeepTheirText asserts an error that is no provider
// answer keeps its text under the step's prefix, and a 4xx answer with no
// error code names the status alone.
func TestIdpErrorOtherErrorsKeepTheirText(t *testing.T) {
	t.Parallel()
	err := idpError(errors.New("connection reset"), "https://idp.example.test/token", exchangePrefix)
	require.EqualError(t, err, exchangePrefix+": connection reset")
	_, ok := errors.AsType[*clierr.Error](err)
	assert.False(t, ok)

	cfg, client := idpAnswer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	_, xerr := cfg.Exchange(context.WithValue(t.Context(), oauth2.HTTPClient, client), "code")
	require.Error(t, xerr)
	got := idpError(xerr, cfg.Endpoint.TokenURL, exchangePrefix)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok, "%v", got)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, exchangePrefix+": the identity provider refused the sign-in (403)", ce.Message)
}
