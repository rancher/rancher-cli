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

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestLoginProviderFlowWithoutATerminalPipesAnAPIToken asserts the refusals
// of auth login's provider flow that no terminal or --no-input can answer,
// and the refusal of an auth config this CLI cannot sign in with, end in the
// way to a login that needs neither: the login that takes a piped API token
// for the entry, as the hint and in the message. The terminal and --no-input
// forms differ in what they say first. On a plain login the command ends the
// message. On the path of a new name with --url (a delegated login) the
// message gives the login first, then the undo of the registration as the
// alternative, and the hint is the login rather than the interactive login
// that was refused. Details of the plain refusals stay empty: nothing was
// signed in.
func TestLoginProviderFlowWithoutATerminalPipesAnAPIToken(t *testing.T) {
	t.Parallel()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"},{"type":"azureADProvider"},{"type":"githubProvider","id":"corp-gh"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fake.Close)
	piped := func(name string) string {
		return clierr.Rancher + " auth login '" + name + "' --url '" + config.CanonicalServerIdentity(fake.URL) + "' --token-stdin"
	}
	const (
		inTerminal  = "; run in a terminal, or to log in with an API token, pipe it to: "
		withoutFlag = "; run without --no-input, or to log in with an API token, pipe it to: "
		apiToken    = "; to log in with an API token, pipe it to: "
	)
	unsupported := "--auth-provider %s names an auth config this CLI cannot sign in with: corp-gh (githubProvider); " +
		"advertised by the server but not supported by this CLI: githubProvider (server change requested: rancher/rancher#57034); " +
		"to sign in as a user of one of them, create an API token in the Rancher UI and log in with it using auth login --token-stdin"

	for _, tc := range []struct {
		name     string
		provider string
		noInput  bool
		code     string
		refusal  string
		lead     string
	}{
		{"a username and password provider, no terminal", "localProvider", false, clierr.CodeAuth,
			"interactive login required for auth config local", inTerminal},
		{"an OAuth provider under --no-input", "azureADProvider", true, clierr.CodeAuth,
			"interactive login required for auth config azuread", withoutFlag},
		{"an auth config this CLI cannot sign in with, by its name", "corp-gh", false, clierr.CodeCapabilityUnsupported,
			strings.Replace(unsupported, "%s", "corp-gh", 1), apiToken},
		{"an auth config this CLI cannot sign in with, by its type", "githubProvider", false, clierr.CodeCapabilityUnsupported,
			strings.Replace(unsupported, "%s", "githubProvider", 1), apiToken},
		{"an auth config this CLI cannot sign in with, by its provider name", "github", true, clierr.CodeCapabilityUnsupported,
			strings.Replace(unsupported, "%s", "github", 1), apiToken},
	} {
		t.Run(tc.name+", a registered entry", func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", fake.URL, "", false)
			in := cmdtest.Input{Str: map[string]string{"auth-provider": tc.provider}, Pos: []string{"prod"}}
			p := ui.New(&bytes.Buffer{}, strings.NewReader(""), tc.noInput, false)
			_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.refusal+tc.lead+piped("prod"), ce.Message)
			assert.Equal(t, piped("prod"), ce.Hint)
			if tc.code == clierr.CodeAuth {
				assert.Empty(t, ce.Candidates)
				assert.Nil(t, ce.Details, "nothing was signed in: no login-failure details")
			}
		})
		t.Run(tc.name+", a new name with --url", func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			in := cmdtest.Input{Str: map[string]string{"auth-provider": tc.provider, "url": fake.URL}, Pos: []string{"new"}}
			p := ui.New(&bytes.Buffer{}, strings.NewReader(""), tc.noInput, false)
			_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.refusal+tc.lead+piped("new")+"; or undo the registration with: "+clierr.Rancher+" config delete-server 'new'", ce.Message)
			assert.Equal(t, piped("new"), ce.Hint)
		})
	}
}
