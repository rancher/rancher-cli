package token

import (
	"errors"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token/complete"
	"github.com/rancher/rancher-cli/internal/ui"
)

// signInThroughTheBrowser signs in against a facade built from opts with a
// deposit standing in for the browser, and asserts the result every SAML
// sign-in has: the sign-in picked the list entry with id wantID, the SAML
// handoff names the provider type, the token is the one the facade created,
// and the authtokens path saw a read and then a delete.
func signInThroughTheBrowser(t *testing.T, opts testrancher.Options, authProvider, wantID, wantType string) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		opts.EnableSAMLAuthTokens = true
		f := testrancher.New(opts)
		srv := httptest.NewTestServer(t, f)
		client := hostTransport{hostOf(rancherOrigin): srv.Client().Transport}.client()
		p := newHandoffPrompter(t, func(id, pub string) { f.DepositAuthToken(id, pub) })
		input := samlInput(rancherOrigin)
		input.authProvider = authProvider
		res, err := loginAndGenerateCred(t.Context(), client, input, p)
		require.NoError(t, err)
		assert.Equal(t, wantID, input.configName, "the list entry the sign-in picked")
		assert.Equal(t, wantType, input.authProvider)
		require.NotNil(t, res.Handoff)
		assert.Equal(t, wantType, res.Handoff.Provider)
		assert.Equal(t, CreateKnown, res.Create.Outcome)
		require.NotNil(t, res.Create.Ref)
		assert.Equal(t, TokenKindV3, res.Create.Ref.Kind)
		assert.NotEmpty(t, res.Create.Bearer)
		_, alive := f.TokenOwner(res.Create.Ref.Name)
		assert.True(t, alive, "the token is one the facade created")
		assert.Equal(t, 1, f.TokenCount())
		assertAuthTokensOnly(t, f)
		reqs := f.AuthTokensRequests()
		require.NotEmpty(t, reqs)
		assert.Equal(t, "GET", reqs[0].Method)
		assert.Equal(t, "DELETE", reqs[len(reqs)-1].Method, "the deposited copy is deleted after decryption")
	})
}

// TestGenericSAMLSignInByProviderName asserts a sign-in with --auth-provider
// genericsaml, Rancher's provider name of the Generic SAML type, goes through
// the browser handoff of the SAML providers.
func TestGenericSAMLSignInByProviderName(t *testing.T) {
	signInThroughTheBrowser(t, testrancher.Options{ExtraProviders: []string{"genericSAMLProvider"}}, "genericsaml", "genericsaml", "genericSAMLProvider")
}

// TestGenericSAMLSignInByConfigName asserts a sign-in with the name of a
// Generic SAML auth config picks the list entry of that name, beside another
// Generic SAML entry, and goes through the browser handoff.
func TestGenericSAMLSignInByConfigName(t *testing.T) {
	opts := testrancher.Options{
		ExtraProviders: []string{"genericSAMLProvider"},
		AuthConfigs:    []testrancher.AuthConfig{{Name: "corp-saml", Type: "genericSAMLProvider"}},
	}
	signInThroughTheBrowser(t, opts, "corp-saml", "corp-saml", "genericSAMLProvider")
}

// TestEverySAMLTypeTheFacadeListsSignsIn asserts every provider type the
// facade can list as a SAML provider is one the sign-in code treats as SAML:
// it signs in through the browser handoff and is in both the SAML provider
// types and the supported auth provider types, and the SAML provider types
// hold none the facade cannot list.
func TestEverySAMLTypeTheFacadeListsSignsIn(t *testing.T) {
	types := testrancher.SAMLProviderTypes()
	require.NotEmpty(t, types)
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			signInThroughTheBrowser(t, testrancher.Options{ExtraProviders: []string{typ}}, typ, strings.ToLower(strings.TrimSuffix(typ, "Provider")), typ)
		})
	}
	assert.ElementsMatch(t, types, slices.Collect(maps.Keys(samlProviders)), "the SAML provider types and the types the facade lists")
	for _, typ := range types {
		assert.True(t, supportedAuthProviders[typ], "%s is a supported auth provider type", typ)
	}
}

// TestAuthProviderValueSelectsAGenericSAMLConfig asserts every value the
// --auth-provider flag takes for a Generic SAML auth config selects it: every
// name the completion offers, and the provider name and the type when one
// config has that type.
func TestAuthProviderValueSelectsAGenericSAMLConfig(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{
		ExtraProviders: []string{"genericSAMLProvider"},
		AuthConfigs:    []testrancher.AuthConfig{{Name: "corp-saml", Type: "genericSAMLProvider"}},
	})
	srv := httptest.NewTestServer(t, f)
	client := hostTransport{hostOf(rancherOrigin): srv.Client().Transport}.client()
	providers, unsupported, err := getAuthProviders(t.Context(), client, rancherOrigin)
	require.NoError(t, err)
	assert.Empty(t, unsupported, "the CLI supports every type the server lists")

	conn := completion.NewConnection(client, "", rancherOrigin, false, 1)
	offered := complete.AuthConfigSource().Complete(t.Context(), conn, completion.Request{Flag: "auth-provider"}).Names
	require.Contains(t, offered, "genericsaml")
	require.Contains(t, offered, "corp-saml")
	for _, name := range offered {
		got, err := selectAuthProvider(t.Context(), providers, unsupported, name, false, ui.Discard())
		require.NoError(t, err, "the command accepts the offered value %q", name)
		assert.Equal(t, name, got.GetID())
	}

	// With one Generic SAML config the provider name and the type select it
	// whatever its id is.
	only := slices.DeleteFunc(slices.Clone(providers), func(p typedProvider) bool { return p.GetID() == "genericsaml" })
	for _, want := range []string{"genericsaml", "genericSAMLProvider"} {
		got, err := selectAuthProvider(t.Context(), only, unsupported, want, false, ui.Discard())
		require.NoError(t, err, want)
		assert.Equal(t, "genericSAMLProvider", got.GetType(), want)
		assert.Equal(t, "corp-saml", got.GetID(), want)
	}
}

// TestGenericSAMLSignInWithoutInputNeedsAnInteractiveLogin asserts a sign-in
// with no input over a Generic SAML config is refused as one that needs the
// browser, not as an auth config this CLI cannot sign in with.
func TestGenericSAMLSignInWithoutInputNeedsAnInteractiveLogin(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{
		EnableSAMLAuthTokens: true,
		AuthConfigs:          []testrancher.AuthConfig{{Name: "corp-saml", Type: "genericSAMLProvider"}},
	})
	srv := httptest.NewTestServer(t, f)
	client := hostTransport{hostOf(rancherOrigin): srv.Client().Transport}.client()
	input := samlInput(rancherOrigin)
	input.authProvider = "corp-saml"
	_, err := loginAndGenerateCred(t.Context(), client, input, ui.Discard())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	require.ErrorContains(t, err, "interactive login required for auth config corp-saml")
	assert.NotContains(t, err.Error(), "not supported by this CLI")
	nt, ok := errors.AsType[*needsTerminal](err)
	require.True(t, ok, "the error carries the cause the no-terminal remedies read: %v", err)
	assert.Equal(t, "corp-saml", nt.authConfig)
	assert.Empty(t, f.AuthTokensRequests(), "the CLI did not start a browser handoff")
}
