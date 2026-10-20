package token

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/ui"
)

// ldapConfigs is a server with the default OpenLDAP config and a second one
// beside it, as Rancher v2.16 allows, and the local one.
func ldapConfigs() []typedProvider {
	return []typedProvider{
		&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}},
		&AuthProvider{Type: "openLdapProvider", ID: "openldap"},
		&AuthProvider{Type: "openLdapProvider", ID: "corp-ldap"},
	}
}

// TestProviderNameFollowsRancher asserts the provider name is Rancher's
// short name of a type: lower case, a trailing "config" and then "provider"
// dropped.
func TestProviderNameFollowsRancher(t *testing.T) {
	t.Parallel()
	for typ, want := range map[string]string{
		"localProvider":           "local",
		"openLdapProvider":        "openldap",
		"openLdapConfig":          "openldap",
		"freeIpaProvider":         "freeipa",
		"activeDirectoryProvider": "activedirectory",
		"azureADProvider":         "azuread",
		"keyCloakProvider":        "keycloak",
		"shibbolethProvider":      "shibboleth",
	} {
		assert.Equal(t, want, providerName(typ), typ)
	}
}

// TestSelectAuthProviderNamedConfig asserts a value equal to an entry's id
// picks that entry, whatever else is enabled.
func TestSelectAuthProviderNamedConfig(t *testing.T) {
	t.Parallel()
	got, err := selectAuthProvider(t.Context(), ldapConfigs(), nil, "corp-ldap", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "corp-ldap", got.GetID())
}

// TestSelectAuthProviderNamedConfigBeatsTheProviderName asserts the config
// named like a provider (openldap) is the one picked when another config of
// the type is enabled too: the value is an exact name before it is a
// provider name that matches both.
func TestSelectAuthProviderNamedConfigBeatsTheProviderName(t *testing.T) {
	t.Parallel()
	got, err := selectAuthProvider(t.Context(), ldapConfigs(), nil, "openldap", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "openldap", got.GetID())
}

// TestSelectAuthProviderProviderNameOfOneConfig asserts a provider name or
// type that matches one enabled config picks it.
func TestSelectAuthProviderProviderNameOfOneConfig(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{
		&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}},
		&AuthProvider{Type: "openLdapProvider", ID: "corp-ldap"},
	}
	for _, want := range []string{"openldap", "openLdapProvider"} {
		got, err := selectAuthProvider(t.Context(), ps, nil, want, false, ui.Discard())
		require.NoError(t, err, want)
		assert.Equal(t, "corp-ldap", got.GetID(), want)
	}
}

// TestSelectAuthProviderSeveralMatchesWithoutATerminal asserts a provider
// name or type that matches several configs fails with AMBIGUOUS_SELECTION, a
// message naming exactly those configs, and only those as candidates.
func TestSelectAuthProviderSeveralMatchesWithoutATerminal(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{
		&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}},
		&AuthProvider{Type: "openLdapProvider", ID: "corp-ldap"},
		&AuthProvider{Type: "openLdapProvider", ID: "eu-ldap"},
	}
	for _, want := range []string{"openLdapProvider", "openldap"} {
		_, err := selectAuthProvider(t.Context(), ps, nil, want, false, ui.Discard())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, "--auth-provider "+want+" matches 2 auth configs (corp-ldap, eu-ldap); pass one of those names instead", ce.Message)
		assert.Equal(t, []clierr.Candidate{{ID: "corp-ldap", Name: "corp-ldap (openldap)"}, {ID: "eu-ldap", Name: "eu-ldap (openldap)"}}, ce.Candidates)
		cause, ok := errors.AsType[*providerChoiceNeedsTerminal](err)
		require.True(t, ok, "the plugin builds its remedies from the cause")
		assert.Equal(t, []string{"corp-ldap", "eu-ldap"}, cause.configs, "only the matching configs")
	}
}

// TestSelectAuthProviderSeveralMatchesAsksAmongOnlyThose asserts that with a
// terminal a value matching several configs shows a menu of those and not of
// every config, and that the pick selects the entry shown at its number.
func TestSelectAuthProviderSeveralMatchesAsksAmongOnlyThose(t *testing.T) {
	t.Parallel()
	p := terminalLines("1")
	got, err := selectAuthProvider(t.Context(), ldapConfigs(), nil, "openLdapProvider", false, p)
	require.NoError(t, err)
	assert.Equal(t, "corp-ldap", got.GetID())
	require.GreaterOrEqual(t, p.at("notice: Auth configs:"), 0, "events %q", p.events)
	assert.Equal(t, "notice: Auth configs:\n0 - openldap\n1 - corp-ldap (openldap)\n", p.events[p.at("notice: Auth configs:")])
	assert.NotContains(t, strings.Join(p.events, "\n"), "local", "the menu has only the matching configs")
	assert.GreaterOrEqual(t, p.at("prompt: Select auth config: "), 0)
}

// TestTheMenuNamesAuthConfigs asserts the menu heading, its lines (the
// provider name only where it differs from the config's name; an entry with
// no id shows its provider name) and the prompt.
func TestTheMenuNamesAuthConfigs(t *testing.T) {
	t.Parallel()
	noID := &AuthProvider{Type: "oktaProvider"}
	ps := append(ldapConfigs(), noID)
	p := terminalLines("0")
	_, err := selectAuthProvider(t.Context(), ps, nil, "", false, p)
	require.NoError(t, err)
	require.GreaterOrEqual(t, p.at("notice: Auth configs:"), 0, "events %q", p.events)
	assert.Equal(t, "notice: Auth configs:\n0 - local\n1 - openldap\n2 - corp-ldap (openldap)\n3 - okta\n", p.events[p.at("notice: Auth configs:")])
	assert.Contains(t, p.events, "prompt: Select auth config: ")
}

// TestCandidatesCarryTheMenuLineAsTheirName asserts a candidate is the auth
// config's name with the menu line's form as its display name, and that an
// entry with no id is no candidate.
func TestCandidatesCarryTheMenuLineAsTheirName(t *testing.T) {
	t.Parallel()
	ps := append(ldapConfigs(), &AuthProvider{Type: "oktaProvider"})
	_, err := selectAuthProvider(t.Context(), ps, nil, "nope", false, ui.Discard())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []clierr.Candidate{
		{ID: "local", Name: "local"},
		{ID: "openldap", Name: "openldap"},
		{ID: "corp-ldap", Name: "corp-ldap (openldap)"},
	}, ce.Candidates)
}

// TestNoFlagSeveralConfigsText asserts the refusal to choose with no flag
// and no terminal names the auth configs and --auth-provider, and that the
// refusal of an invalid menu pick carries the same text after an unfiltered
// menu and the several-match text after a filtered one.
func TestNoFlagSeveralConfigsText(t *testing.T) {
	t.Parallel()
	noID := &AuthProvider{Type: "oktaProvider"}
	ps := append(ldapConfigs(), noID)

	_, err := selectAuthProvider(t.Context(), ps, nil, "", false, noTerminal())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "this server has several auth configs: local, openldap, corp-ldap; pass one of them with --auth-provider", ce.Message,
		"an entry with no id is not named")
	cause, ok := errors.AsType[*providerChoiceNeedsTerminal](err)
	require.True(t, ok)
	assert.Equal(t, ce.Message, cause.Error())

	_, err = selectAuthProvider(t.Context(), ps, nil, "", false, terminalLines("not a number"))
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, "this server has several auth configs: local, openldap, corp-ldap; pass one of them with --auth-provider", ce.Message)

	_, err = selectAuthProvider(t.Context(), ps, nil, "openLdapProvider", false, terminalLines("not a number"))
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, "--auth-provider openLdapProvider matches 2 auth configs (openldap, corp-ldap); pass one of those names instead", ce.Message,
		"after a filtered menu the pick fails with the several-match text")
}

// TestNoAuthConfigText asserts the refusal of a server that lists none this
// CLI can sign in with, and that it names the advertised types it cannot use.
func TestNoAuthConfigText(t *testing.T) {
	t.Parallel()
	_, err := selectAuthProvider(t.Context(), nil, nil, "", false, ui.Discard())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, "this server has no auth config this CLI can sign in with", ce.Message)

	_, err = selectAuthProvider(t.Context(), nil, []string{"githubProvider"}, "", false, ui.Discard())
	require.ErrorContains(t, err, "this server has no auth config this CLI can sign in with; advertised by the server but not supported by this CLI: githubProvider")
}

// TestAnEntryWithNoIDIsNamedByItsProviderName asserts a single entry
// without an id is chosen, shown and named in the notice by its provider
// name, and that it is never a candidate.
func TestAnEntryWithNoIDIsNamedByItsProviderName(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "openLdapProvider"}}
	var errBuf strings.Builder
	got, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, ui.New(&errBuf, blockingReader{}, false, true))
	require.NoError(t, err)
	assert.Equal(t, "openLdapProvider", got.GetType())
	assert.Contains(t, errBuf.String(), "Auth config: openldap\n")
	assert.Equal(t, "openldap", authConfigName(got))
	assert.Empty(t, authConfigCandidates(ps))
	assert.Empty(t, authConfigNames(ps))
}

// TestTheSingleEntryNoticeNamesTheAuthConfig asserts the notice before the
// prompt for a single entry carries the menu line's form of its name.
func TestTheSingleEntryNoticeNamesTheAuthConfig(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "openLdapProvider", ID: "corp-ldap"}}
	var errBuf strings.Builder
	_, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, ui.New(&errBuf, blockingReader{}, false, true))
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "Auth config: corp-ldap (openldap)\n")
}

// TestAnExactNameOfAnUnsupportedConfigIsRefused asserts an --auth-provider
// value that names an auth config of a type this CLI cannot sign in with is
// NOT_FOUND with the unsupported-types note, and never reaches the match by
// provider name: a GitHub config named openldap beside one OpenLDAP config
// corp-ldap must not sign in through corp-ldap. Mutation: drop the refusal
// before selectAuthProvider.
func TestAnExactNameOfAnUnsupportedConfigIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"githubProvider","id":"openldap"},{"type":"openLdapProvider","id":"corp-ldap"}]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{server: srv.URL, authProvider: "openldap"}, noTerminal())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider openldap names an auth config this CLI cannot sign in with; "), ce.Message)
	assert.Contains(t, ce.Message, "githubProvider")
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "corp-ldap", ce.Candidates[0].ID)
}

// TestAnEntryWithNoIDIsNeverNamedToThePlugin asserts that when nobody can
// answer the menu, an entry of the provider list with no id is left out of
// the exec plugin's cause: it is neither one of the auth configs the
// refusal names nor a candidate of the remedy that adds --auth-provider,
// since --auth-provider cannot name it. Mutation: keep entries with no id
// in the cause.
func TestAnEntryWithNoIDIsNeverNamedToThePlugin(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{
		&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}},
		&AuthProvider{Type: "oktaProvider"},
		&AuthProvider{Type: "azureADProvider", ID: "azuread"},
	}
	_, err := selectAuthProvider(t.Context(), ps, nil, "", false, noTerminal())
	var cause *providerChoiceNeedsTerminal
	require.ErrorAs(t, err, &cause)
	assert.Equal(t, []string{"local", "azuread"}, cause.configs)
	var ids []string
	for _, c := range cause.noTerminal {
		ids = append(ids, c.ID)
	}
	assert.Equal(t, []string{"azuread"}, ids)
}

// TestAnExactNameOnAServerWithOnlyUnsupportedConfigsIsRefused asserts a
// server whose only auth config is of a type this CLI cannot sign in with
// answers its exact name with NOT_FOUND and no candidates, not with the
// no-config error. Mutation: drop the refusal before selectAuthProvider.
func TestAnExactNameOnAServerWithOnlyUnsupportedConfigsIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"githubProvider","id":"github"}]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{server: srv.URL, authProvider: "github"}, noTerminal())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider github names an auth config this CLI cannot sign in with; "), ce.Message)
	assert.Empty(t, ce.Candidates)
}
