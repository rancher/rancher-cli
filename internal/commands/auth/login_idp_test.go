package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// untrustedIdPRancher is a Rancher that lists an azureADProvider whose
// identity provider is on a TLS server this process does not trust.
func untrustedIdPRancher(t *testing.T) *httptest.Server {
	t.Helper()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(idp.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider","clientId":"c","scopes":["openid"],` +
				`"authUrl":"` + idp.URL + `/authorize","deviceAuthUrl":"` + idp.URL + `/devicecode","tokenUrl":"` + idp.URL + `/token"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runProviderLogin runs auth login for the entry prod at srv, signing in
// through the azureADProvider with no terminal, and returns the error and
// what the command wrote to stderr.
func runProviderLogin(t *testing.T, srv *httptest.Server) (runtime.Descriptor, string, error) {
	t.Helper()
	keyring.MockInit()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "azureADProvider"}, Pos: []string{"prod"}}
	var stderr bytes.Buffer
	cmd := newPlainLoginCmd(dir, store)
	_, err := cmd.Execute(t.Context(), in, ui.New(&stderr, strings.NewReader(""), false, false))
	require.Error(t, err)
	return cmd.Describe(), stderr.String(), err
}

// TestLoginIdPCertificateNamesItsCAFlag asserts the refusal for an identity
// provider whose certificate the operating system does not trust names the
// CA flag auth login declares.
func TestLoginIdPCertificateNamesItsCAFlag(t *testing.T) {
	t.Setenv(config.EnvOAuthAuthFlow, "")
	t.Setenv(config.EnvLegacyOAuthAuthFlow, "")
	desc, _, err := runProviderLogin(t, untrustedIdPRancher(t))
	var flag string
	for _, f := range desc.Flags {
		if f.Name == "cacert" || f.Name == "cacerts" {
			require.Empty(t, flag, "auth login declares one CA flag")
			flag = "--" + f.Name
		}
	}
	require.NotEmpty(t, flag)
	require.ErrorContains(t, err, "certificate not trusted by the operating system trust store; "+flag+" applies only to the Rancher server")
}

// TestLoginInvalidAuthFlowFromTheEnvironmentIsAValidationRefusal asserts a
// value of either variable outside the declared values is a VALIDATION
// refusal that names the variable and the allowed values and never prints
// the value, in the error string, in its text form and in the JSON error.
func TestLoginInvalidAuthFlowFromTheEnvironmentIsAValidationRefusal(t *testing.T) {
	const secret = "s3cr3t"
	for _, variable := range []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(config.EnvOAuthAuthFlow, "")
			t.Setenv(config.EnvLegacyOAuthAuthFlow, "")
			t.Setenv(variable, secret)
			_, stderr, err := runProviderLogin(t, untrustedIdPRancher(t))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			want := variable + " value is not one of " + runtime.OneOf(token.AuthFlowValues())
			require.ErrorContains(t, err, want)
			assert.NotContains(t, stderr, secret)
			assert.NotContains(t, err.Error(), secret)

			var text bytes.Buffer
			require.NoError(t, runtime.RenderError(&text, "table", ce))
			assert.Contains(t, text.String(), want)
			assert.NotContains(t, text.String(), secret)

			fields, exit := cmdtest.RenderedError(t, err)
			assert.Equal(t, clierr.CodeValidation, fields.Code)
			assert.Equal(t, 2, exit)
			assert.Contains(t, fields.Message, want)
			assert.NotContains(t, fields.Message, secret)
			assert.NotContains(t, fields.Hint, secret)
		})
	}
}

// TestLoginAuthFlowFromTheEnvironmentIsCaseInsensitive asserts a variable
// set to DEVICECODE is read as devicecode: the sign-in gets past the check
// and fails at the identity provider's certificate.
func TestLoginAuthFlowFromTheEnvironmentIsCaseInsensitive(t *testing.T) {
	t.Setenv(config.EnvOAuthAuthFlow, "DEVICECODE")
	t.Setenv(config.EnvLegacyOAuthAuthFlow, "")
	_, _, err := runProviderLogin(t, untrustedIdPRancher(t))
	require.ErrorContains(t, err, "certificate not trusted by the operating system trust store")
	assert.NotContains(t, err.Error(), "value is not one of")
}
