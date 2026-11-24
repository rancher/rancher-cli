package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// probeWarningRows lists every error code a probe failure can carry. An empty
// value means the warning keeps the error's message; a non-empty value is the
// reason the warning words a refusal of that code its own way.
var probeWarningRows = map[string]string{
	clierr.CodeNotLoggedIn:                    "",
	clierr.CodeConfirmationRequired:           "",
	clierr.CodeDryRunNotApplicable:            "",
	clierr.CodeAmbiguousSelection:             "",
	clierr.CodeConflict:                       "",
	clierr.CodeLockTimeout:                    "",
	clierr.CodeAuth:                           "",
	clierr.CodeCredentialReplaced:             "",
	clierr.CodeLoginIncomplete:                "",
	clierr.CodeCredentialTransitionIncomplete: "",
	clierr.CodeCredentialStoreUnavailable:     "",
	clierr.CodeNotFound:                       "a 404 of the ext token or identity read means the server does not serve ext.cattle.io, and the warning says what the server needs; a 404 of the token read on a supported server is the rotated token advisory",
	clierr.CodeRBAC:                           "",
	clierr.CodeValidation:                     "",
	clierr.CodePrimeRequired:                  "",
	clierr.CodeTLSTrust:                       "",
	clierr.CodeCapabilityUnsupported:          "",
	clierr.CodeInterrupted:                    "",
	clierr.CodeGeneric:                        "",
}

// TestEveryProbeWarningKeepsTheErrorMessage asserts that for every error code
// the identity and token warnings of auth status carry the error's message,
// so a refusal shows what the server said. The probes are stubs that fail with
// the code, and the command runs for a v3 token and an ext token, with the
// version check unknown, below the floor and at it. Only the codes with a
// reason in probeWarningRows word the warning their own way. A code added to
// clierr.AllCodes fails here until it has a row.
func TestEveryProbeWarningKeepsTheErrorMessage(t *testing.T) {
	all := clierr.AllCodes()
	for _, code := range all {
		require.Contains(t, probeWarningRows, code, "every error code has a row")
	}
	for code := range probeWarningRows {
		assert.Contains(t, all, code, "a row for a code clierr.AllCodes does not list")
	}
	versions := map[string]func(context.Context, config.Effective) (string, error){
		"unknown": func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeGeneric, "no version")
		},
		"unsupported": func(context.Context, config.Effective) (string, error) { return "v2.13.0", nil },
		"supported":   alwaysOKVersion,
	}
	for _, bearer := range []string{"envtok:secret", "ext/x:secret"} {
		t.Run(bearer, func(t *testing.T) {
			t.Setenv(config.EnvToken, bearer)
			dir := seedAuthedDir(t)
			for _, code := range all {
				for versionName, version := range versions {
					t.Run(code+" "+versionName, func(t *testing.T) {
						message := "message of the " + code + " response"
						fail := func(context.Context, config.Effective, string) (provider.TokenMetadata, error) {
							return provider.TokenMetadata{}, clierr.New(code, message)
						}
						cmd := &authCheckCmd{
							dir: dir, store: config.NewFileStore(dir),
							preflight:      alwaysOKPreflight,
							rancherVersion: version,
							identity: func(context.Context, config.Effective) (string, error) {
								return "", clierr.New(code, message)
							},
							token: fail, v3token: fail,
						}
						res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
						require.NoError(t, err)
						identity := warningWithPrefix(res.Warnings, "identity metadata unavailable: ")
						token := warningWithPrefix(res.Warnings, "token metadata unavailable: ")
						if probeWarningRows[code] != "" {
							assert.Contains(t, identity, "ext.cattle.io not served by this server")
							if bearer == "envtok:secret" || versionName == "supported" {
								assert.Equal(t, "token metadata unavailable: the server has no such token (revoked or rotated); set RANCHER_CLI_TOKEN to another token", token)
							} else {
								assert.Contains(t, token, "ext.cattle.io not served by this server")
							}
							return
						}
						assert.Contains(t, identity, message)
						assert.Contains(t, token, message)
					})
				}
			}
		})
	}
}

// warningWithPrefix is the one warning that starts with prefix, empty when
// there is none.
func warningWithPrefix(warnings []string, prefix string) string {
	for _, w := range warnings {
		if strings.HasPrefix(w, prefix) {
			return w
		}
	}
	return ""
}

// TestProbeWarningRowsWithAReasonKeepTheirText asserts the wording of the
// codes that probeWarningRows allows to differ.
func TestProbeWarningRowsWithAReasonKeepTheirText(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	notFound := clierr.New(clierr.CodeNotFound, "the server could not find the requested resource")
	missing := "metadata unavailable: ext.cattle.io not served by this server"
	assert.Contains(t, degradeWarning("token", notFound), "token "+missing)
	assert.Contains(t, degradeWarning("identity", notFound), "identity "+missing)
	assert.Contains(t, tokenWarning(notFound, nil, false), "token "+missing)
	assert.Contains(t, tokenWarning(notFound, &no, false), "token "+missing)

	for _, fromVariable := range []bool{false, true} {
		advisory := rotatedTokenAdvisory(fromVariable)
		assert.Equal(t, advisory, tokenWarning(notFound, &yes, fromVariable))
		assert.Equal(t, advisory, tokenWarning(errV3TokenNotFound, nil, fromVariable))
	}
	assert.Contains(t, rotatedTokenAdvisory(true), "set RANCHER_CLI_TOKEN to another token")
	assert.Contains(t, rotatedTokenAdvisory(false), "re-authenticate with: ")
}
