package auth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// unsupportedConfig is an auth config of a provider type this CLI cannot sign
// in with, with the three values --auth-provider can name it by: its id, its
// provider type and its provider name.
type unsupportedConfig struct{ id, typ, name string }

// unsupportedConfigs are the seven provider types the server lists that this
// CLI cannot use, and a type no Rancher lists today, each with an id unlike
// its provider name.
var unsupportedConfigs = []unsupportedConfig{
	{"corp-gh", "githubProvider", "github"},
	{"corp-ghapp", "githubAppProvider", "githubapp"},
	{"corp-google", "googleOAuthProvider", "googleoauth"},
	{"corp-oidc", "oidcProvider", "oidc"},
	{"corp-kc", "keyCloakOIDCProvider", "keycloakoidc"},
	{"corp-generic", "genericOIDCProvider", "genericoidc"},
	{"corp-cognito", "cognitoProvider", "cognito"},
	{"corp-foo", "fooProvider", "foo"},
}

func (u unsupportedConfig) values() map[string]string {
	return map[string]string{"id": u.id, "type": u.typ, "provider name": u.name}
}

// pipedLoginHint is the login of an API token piped to it, for the entry
// name at the server a test serves.
func pipedLoginHint(name, serverURL string) string {
	return clierr.Rancher + " auth login '" + name + "' --url '" + config.CanonicalServerIdentity(serverURL) + "' --token-stdin"
}

// TestEveryUnsupportedProviderTypeIsRefusedWithAPipedLoginByAuthLogin asserts
// the rule at auth login: for every provider type the server lists that this
// CLI cannot sign in with, --auth-provider as the auth config's id, its
// provider type and its provider name is CAPABILITY_UNSUPPORTED, exit 11,
// naming the auth config and its type, with the login that takes a piped API
// token in the message and as the hint, on a registered entry (the command
// ends the message) and on a new name with --url (the login comes first, the
// undo of the registration follows as the alternative). Mutations: refuse
// only by id (the type and provider name rows fail); read an unknown type as
// a miss (the fooProvider rows fail); drop the call of token.AuthLoginRemedy
// from authenticateAndStore (the registered-entry rows fail on the message
// and hint); drop the call of token.AuthLoginRemedyOf from its delegated
// path (the new-name rows fail).
func TestEveryUnsupportedProviderTypeIsRefusedWithAPipedLoginByAuthLogin(t *testing.T) {
	t.Parallel()
	var entries []string
	var types []string
	for _, u := range unsupportedConfigs {
		entries = append(entries, `{"id":"`+u.id+`","type":"`+u.typ+`"}`)
		types = append(types, u.typ)
	}
	slices.Sort(types)
	note := "advertised by the server but not supported by this CLI: " + strings.Join(types, ", ") +
		" (server change requested: rancher/rancher#57034); " +
		"to sign in as a user of one of them, create an API token in the Rancher UI and log in with it using auth login --token-stdin"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"},` + strings.Join(entries, ",") + `]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fake.Close)

	for _, u := range unsupportedConfigs {
		for form, value := range u.values() {
			for _, delegated := range []bool{false, true} {
				name := u.typ + " by " + form
				if delegated {
					name += ", a new name with --url"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					dir, store := cmdtest.FilePolicyDir(t)
					in := cmdtest.Input{Str: map[string]string{"auth-provider": value}, Pos: []string{"prod"}}
					entry := "prod"
					if delegated {
						in.Str["url"] = fake.URL
						entry = "new"
						in.Pos = []string{entry}
					} else {
						seedServer(t, dir, "prod", fake.URL, "", false)
					}
					_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, strings.NewReader(""), true, false))
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok, "got %v", err)
					assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
					assert.Equal(t, 11, ce.ExitCode())
					want := "--auth-provider " + value + " names an auth config this CLI cannot sign in with: " + u.id + " (" + u.typ + "); " + note +
						"; to log in with an API token, pipe it to: " + pipedLoginHint(entry, fake.URL)
					if delegated {
						want += "; or undo the registration with: " + clierr.Rancher + " config delete-server 'new'"
					}
					assert.Equal(t, want, ce.Message)
					assert.Equal(t, pipedLoginHint(entry, fake.URL), ce.Hint)
				})
			}
		}
	}
}

// TestTheFacadeListsEveryUnsupportedProviderTypeAndAuthLoginRefusesEach
// asserts the same rule against the facade, whose list of auth configs gives
// each extra provider type an id equal to its provider name: the seven types
// Rancher lists, each by id, provider type and provider name.
func TestTheFacadeListsEveryUnsupportedProviderTypeAndAuthLoginRefusesEach(t *testing.T) {
	t.Parallel()
	types := []string{"githubProvider", "githubAppProvider", "googleOAuthProvider", "oidcProvider", "keyCloakOIDCProvider", "genericOIDCProvider", "cognitoProvider"}
	srv := httptest.NewServer(testrancher.New(testrancher.Options{ExtraProviders: types}))
	t.Cleanup(srv.Close)
	for _, typ := range types {
		id := strings.ToLower(strings.TrimSuffix(typ, "Provider"))
		for form, value := range map[string]string{"id": id, "type": typ} {
			t.Run(typ+" by "+form, func(t *testing.T) {
				t.Parallel()
				dir, store := cmdtest.FilePolicyDir(t)
				seedServer(t, dir, "prod", srv.URL, "", false)
				in := cmdtest.Input{Str: map[string]string{"auth-provider": value}, Pos: []string{"prod"}}
				_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, strings.NewReader(""), true, false))
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
				assert.Equal(t, 11, ce.ExitCode())
				assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider "+value+" names an auth config this CLI cannot sign in with: "+id+" ("+typ+"); "), ce.Message)
				assert.Equal(t, pipedLoginHint("prod", srv.URL), ce.Hint)
			})
		}
	}
}
