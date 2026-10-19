package token

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestNoTerminalRemediesNameTheUserForACallWithUserID asserts every
// refusal for want of a terminal - a sign-in prompt, a menu of auth
// configs none of which can answer, and a menu some of which can - names a
// login of the call's user and a sign-in as that user, with or without
// --name, and without --cluster. The sign-in in a terminal is kept for
// later calls either way: with token reuse on as a login credential, and
// with it off, under the call's user, until the cached token expires. The
// call never says its exec entry sets no --name, and never offers create
// kubeconfig for a token for Rancher itself.
func TestNoTerminalRemediesNameTheUserForACallWithUserID(t *testing.T) {
	const (
		serverURL = "https://r.example.com"
		user      = "u-abc12"
	)
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: serverURL, TokenUserID: user}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: serverURL}, "id:secret"))
	cmd := tokenCmd{dir: dir, store: store}

	needs := interactiveLoginRequired("local", "x")
	none := clierr.New(clierr.CodeAmbiguousSelection, "x").WithCause(&providerChoiceNeedsTerminal{configs: []string{"local", "openldap"}})
	some := clierr.New(clierr.CodeAmbiguousSelection, "x").WithCause(&providerChoiceNeedsTerminal{
		configs: []string{"local", "okta"}, noTerminal: []clierr.Candidate{{ID: "okta", Name: "okta"}}})
	heads := []struct {
		name   string
		err    error
		code   string
		head   string
		choice string
	}{
		{"a sign-in prompt", needs, clierr.CodeAuth, "interactive login required for auth config local; this call has no terminal to answer it", ""},
		{"a menu no config can answer", none, clierr.CodeAuth, "interactive login required for one of the auth configs local, openldap; this call has no terminal to answer it", ""},
		{"a menu some configs can answer", some, clierr.CodeAmbiguousSelection, "this server has several auth configs and this call has no terminal to choose one",
			"; or add --auth-provider okta to the command (in a kubeconfig, to the exec entry's args) to sign in through okta in a browser"},
	}
	for _, h := range heads {
		for _, tc := range []struct {
			name       string
			call       noTerminalCall
			login      string
			clause     string
			kubeconfig bool
		}{
			{"a --name and a cluster, token reuse off", noTerminalCall{name: "prod-c-1", clusterID: "c-1"},
				"rancher auth login 'prod' --url '" + serverURL + "' --token-stdin", "; or run kubectl once in a terminal and sign in as '" + user + "', which lasts until the cached token expires", true},
			{"a --name and a cluster, token reuse on", noTerminalCall{name: "prod-c-1", clusterID: "c-1", keepsLogin: true},
				"rancher auth login 'prod' --url '" + serverURL + "' --token-stdin", "; or run kubectl once in a terminal and sign in as '" + user + "'", true},
			{"no --name, token reuse off", noTerminalCall{clusterID: "c-1"},
				"rancher auth login <name> --url '" + serverURL + "' --token-stdin, with a name of your choice for <name>",
				"; or run kubectl once in a terminal and sign in as '" + user + "', which lasts until the cached token expires", true},
			{"no --name, token reuse on", noTerminalCall{clusterID: "c-1", keepsLogin: true},
				"rancher auth login <name> --url '" + serverURL + "' --token-stdin, with a name of your choice for <name>",
				"; or run kubectl once in a terminal and sign in as '" + user + "'", true},
			{"no cluster, token reuse off", noTerminalCall{},
				"rancher auth login <name> --url '" + serverURL + "' --token-stdin, with a name of your choice for <name>",
				"; or run kubectl once in a terminal and sign in as '" + user + "', which lasts until the cached token expires", false},
			{"no cluster, token reuse on", noTerminalCall{keepsLogin: true},
				"rancher auth login <name> --url '" + serverURL + "' --token-stdin, with a name of your choice for <name>",
				"; or run kubectl once in a terminal and sign in as '" + user + "'", false},
		} {
			t.Run(h.name+", "+tc.name, func(t *testing.T) {
				call := tc.call
				call.serverURL, call.userID = serverURL, user
				err := cmd.withNoTerminalRemedies(h.err, call)
				ce, ok := clierrOf(err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, h.code, ce.Code)
				want := h.head + "; to log in without one, pipe an API token of Rancher user '" + user + "' to " + tc.login + h.choice + tc.clause
				if tc.kubeconfig {
					want += "; or use rancher create kubeconfig --clusters 'c-1' --apply --yes for a kubeconfig that carries its own token"
				}
				assert.Equal(t, want, clierr.ExpandRemedies(ce.Message, ""))
				assert.NotContains(t, ce.Message, "sets no --name")
			})
		}
	}
	t.Run("without --user-id and without --name the old sentence stands", func(t *testing.T) {
		err := cmd.withNoTerminalRemedies(needs, noTerminalCall{clusterID: "c-1", serverURL: serverURL})
		ce, ok := clierrOf(err)
		require.True(t, ok)
		assert.Contains(t, ce.Message, "this exec entry sets no --name, so a sign-in in a terminal would not be kept")
	})
}
