package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// declaredCAFlag is the CA flag a command declares, with its dashes. It
// fails when the command declares none or both spellings.
func declaredCAFlag(t *testing.T, d runtime.Descriptor) string {
	t.Helper()
	var found []string
	for _, f := range d.Flags {
		if f.Name == "cacert" || f.Name == "cacerts" {
			found = append(found, "--"+f.Name)
		}
	}
	require.Len(t, found, 1, "the CA flags %s declares", d.Name)
	return found[0]
}

// idpCall is one way a command reaches the sign-in: the command, its flags
// and whether token reuse is on.
type idpCall struct {
	name  string
	shim  bool
	flags map[string]string
	reuse bool
}

// idpCalls are the commands of this package that sign in through an identity
// provider: auth get-token with and without the plugin-created login, and the
// legacy token command without --cluster (its own sign-in) and with it and
// token reuse on (the plugin-created login).
var idpCalls = []idpCall{
	{name: "auth get-token", flags: map[string]string{"cluster": "c-1", "name": "prod-c-1"}},
	{name: "auth get-token, plugin login", flags: map[string]string{"cluster": "c-1", "name": "prod-c-1"}, reuse: true},
	{name: "legacy token", shim: true, flags: map[string]string{"user": "alice"}},
	{name: "legacy token, plugin login", shim: true, flags: map[string]string{"user": "alice", "cluster": "c-1"}, reuse: true},
}

// runIdPCall runs the call against srv with a prompter that has no terminal
// and returns its error and what the call wrote to stderr.
func runIdPCall(t *testing.T, c idpCall, srv *httptest.Server, extra map[string]string) (runtime.Descriptor, string, error) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rancher")
	cfg := &config.Config{CredentialStore: "file", TokenReuse: c.reuse}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var cmd runtime.ReadCommand
	if c.shim {
		cmd = NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
	} else {
		cmd = NewTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
	}
	flags := map[string]string{"server": srv.URL, "auth-provider": "azureADProvider"}
	maps.Copy(flags, c.flags)
	maps.Copy(flags, extra)
	var stderr bytes.Buffer
	_, err := cmd.Execute(t.Context(), tin{s: flags}, ui.New(&stderr, strings.NewReader(""), false, false))
	require.Error(t, err)
	return cmd.Describe(), stderr.String(), err
}

func isolateIdPCalls(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(config.EnvOAuthAuthFlow, "")
	t.Setenv(config.EnvLegacyOAuthAuthFlow, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
}

// TestIdPCertificateNamesTheCAFlagOfTheCommand asserts the refusal for an
// identity provider whose certificate the operating system does not trust
// names the CA flag the command that ran declares: --cacert for auth
// get-token, --cacerts for the legacy token command, on its own sign-in and
// through the plugin-created login.
func TestIdPCertificateNamesTheCAFlagOfTheCommand(t *testing.T) {
	isolateIdPCalls(t)
	for _, c := range idpCalls {
		t.Run(c.name, func(t *testing.T) {
			desc, _, err := runIdPCall(t, c, deviceFailsServer(t), nil)
			flag := declaredCAFlag(t, desc)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			require.ErrorContains(t, err, "certificate not trusted by the operating system trust store; "+flag+" applies only to the Rancher server")
			if c.shim {
				assert.NotContains(t, ce.Message, "--cacert ", "the legacy command has no --cacert")
			}
		})
	}
}

// TestInvalidAuthFlowFromTheEnvironmentIsAValidationRefusal asserts a value
// of RANCHER_CLI_OAUTH_AUTH_FLOW or CATTLE_OAUTH_AUTH_FLOW outside the
// declared values is a VALIDATION refusal that names the variable and the
// allowed values and never prints the value, on every command of this
// package that signs in, in the error string, in its text form and in the
// error a JSON consumer reads.
func TestInvalidAuthFlowFromTheEnvironmentIsAValidationRefusal(t *testing.T) {
	const secret = "s3cr3t"
	isolateIdPCalls(t)
	for _, variable := range []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow} {
		for _, c := range idpCalls {
			t.Run(variable+", "+c.name, func(t *testing.T) {
				isolateIdPCalls(t)
				t.Setenv(variable, secret)
				_, stderr, err := runIdPCall(t, c, deviceFailsServer(t), nil)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, 2, ce.ExitCode())
				want := variable + " value is not one of " + runtime.OneOf(AuthFlowValues())
				require.ErrorContains(t, err, want)
				assert.NotContains(t, stderr, secret)
				assert.NotContains(t, err.Error(), secret)

				var text bytes.Buffer
				require.NoError(t, runtime.RenderError(&text, "table", ce))
				assert.Contains(t, text.String(), want)
				assert.NotContains(t, text.String(), secret)

				var rendered bytes.Buffer
				require.NoError(t, runtime.RenderError(&rendered, "json", ce))
				var env struct {
					Data struct {
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
						} `json:"error"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rendered.Bytes(), &env), "%s", rendered.Bytes())
				assert.Equal(t, clierr.CodeValidation, env.Data.Error.Code)
				assert.Contains(t, env.Data.Error.Message, want)
				assert.NotContains(t, rendered.String(), secret)
			})
		}
	}
}

// TestAuthFlowFromTheEnvironmentIsCaseInsensitive asserts a variable set to
// AUTHCODE is read as authcode: the call gets past the check into the
// auth-code flow, which an unusable callback port stops before it opens a
// browser.
func TestAuthFlowFromTheEnvironmentIsCaseInsensitive(t *testing.T) {
	for _, variable := range []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow} {
		t.Run(variable, func(t *testing.T) {
			isolateIdPCalls(t)
			t.Setenv(variable, "AUTHCODE")
			t.Setenv(config.EnvOAuthCallbackPort, "not-a-port")
			so := splitOriginFacade(t)
			providers, _, _, err := getAuthProviders(t.Context(), so.rancherClient, so.rancherURL)
			require.NoError(t, err)
			provider, err := selectAuthProvider(t.Context(), providers, nil, "azureADProvider", false, ui.Discard())
			require.NoError(t, err)
			input := &loginInput{caFlag: "--cacert", server: so.rancherURL, authProvider: "azureADProvider", idpClient: so.idpClient}
			_, err = oauthAuth(t.Context(), so.rancherClient, input, provider, interactivePrompter())
			require.ErrorContains(t, err, "invalid callback port value")
			assert.Equal(t, authCodeFlow, input.authFlow)
		})
	}
}

// TestAuthFlowValuesIsANewSliceOfTheFlows asserts the list the commands
// declare is the two flows in declaration order and that changing a returned
// slice changes no later one.
func TestAuthFlowValuesIsANewSliceOfTheFlows(t *testing.T) {
	got := AuthFlowValues()
	assert.Equal(t, []string{deviceAuthFlow, authCodeFlow}, got)
	got[0] = "changed"
	assert.Equal(t, []string{deviceAuthFlow, authCodeFlow}, AuthFlowValues())
}

// TestASignInWithNoCAFlagIsRefusedBeforeAnyRequest asserts a sign-in whose
// caller left out its CA flag name is an internal error naming the field,
// returned before any request, from both entry points.
func TestASignInWithNoCAFlagIsRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		run   func(srv *httptest.Server) error
	}{
		{"AcquireLoginCredential", "LoginRequest.CAFlag", func(srv *httptest.Server) error {
			_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider"}, 0, noTerminal())
			return err
		}},
		{"loginAndGenerateCred", "loginInput.caFlag", func(srv *httptest.Server) error {
			_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{server: srv.URL, authProvider: "localProvider"}, noTerminal())
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(srv.Close)
			err := tc.run(srv)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, strings.HasPrefix(ce.Message, "internal error: "+tc.field+" is empty"), "message %q", ce.Message)
			assert.Zero(t, requests.Load())
		})
	}
}
