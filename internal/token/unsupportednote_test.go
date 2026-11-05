package token

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/ui"
)

// apiTokenRemedy is the sentence every place that names the provider types
// this CLI cannot use ends with.
const apiTokenRemedy = "to sign in as a user of one of them, create an API token in the Rancher UI and log in with it using auth login --token-stdin"

// TestEveryPlaceThatNamesUnsupportedProviderTypesSaysHowToSignInWithAnAPIToken
// asserts the rule for every surface the note of unsupported provider types
// reaches: the refusal of a server with no auth config this CLI can sign in
// with, the miss under --user-id and the other miss, the single-entry notice,
// the menu, the refusal to choose with no terminal, the note the exec plugin
// builds its remedies with, and the refusal of an auth config this CLI cannot
// sign in with. Each says to create an API token in the Rancher UI and log in
// with it, once. Mutation: remove the sentence from unsupportedNote (every
// row fails).
func TestEveryPlaceThatNamesUnsupportedProviderTypesSaysHowToSignInWithAnAPIToken(t *testing.T) {
	t.Parallel()
	local := &LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}}
	ldap := &AuthProvider{Type: "openLdapProvider", ID: "corp-ldap"}
	gh := []unsupportedAuthConfig{{ID: "corp-gh", Type: "githubProvider"}}
	for _, tc := range []struct {
		name string
		text func(t *testing.T) string
	}{
		{"no auth config this CLI can sign in with", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), nil, gh, "", false, ui.Discard())
			return messageOf(t, err)
		}},
		{"a miss under --user-id", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), []typedProvider{local}, gh, "nope", true, ui.Discard())
			return messageOf(t, err)
		}},
		{"a miss", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), []typedProvider{local}, gh, "nope", false, ui.Discard())
			return messageOf(t, err)
		}},
		{"the single-entry notice", func(t *testing.T) string {
			t.Helper()
			var out strings.Builder
			_, err := selectAuthProvider(t.Context(), []typedProvider{local}, gh, "", false, ui.New(&out, blockingReader{}, false, true))
			require.NoError(t, err)
			return out.String()
		}},
		{"the menu", func(t *testing.T) string {
			t.Helper()
			var out strings.Builder
			_, err := selectAuthProvider(t.Context(), []typedProvider{local, ldap}, gh, "", false, ui.New(&out, newLineReader("0"), false, true))
			require.NoError(t, err)
			return out.String()
		}},
		{"the refusal to choose with no terminal", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), []typedProvider{local, ldap}, gh, "", false, noTerminal())
			return messageOf(t, err)
		}},
		{"the note the exec plugin builds its remedies with", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), []typedProvider{local, ldap}, gh, "", false, noTerminal())
			cause, ok := errors.AsType[*providerChoiceNeedsTerminal](err)
			require.True(t, ok, "got %v", err)
			return cause.note
		}},
		{"the refusal of an auth config this CLI cannot sign in with", func(t *testing.T) string {
			t.Helper()
			_, err := selectAuthProvider(t.Context(), []typedProvider{local}, gh, "githubProvider", false, ui.Discard())
			return messageOf(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.text(t)
			assert.Equal(t, 1, strings.Count(text, apiTokenRemedy), text)
			assert.Contains(t, text, "advertised by the server but not supported by this CLI: githubProvider (server change requested: rancher/rancher#57034); "+apiTokenRemedy)
		})
	}
}

func messageOf(t *testing.T, err error) string {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	return ce.Message
}
