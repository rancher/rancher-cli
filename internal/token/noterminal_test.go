package token

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// localProviderServer is a Rancher whose one auth provider signs in with a
// username and password, which needs a terminal to ask for them.
func localProviderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noTerminal is the prompter kubectl gives the plugin when it runs it
// with no terminal attached.
func noTerminal() *ui.Prompter { return ui.New(io.Discard, strings.NewReader(""), false, false) }

// TestTheExecPluginNamesTheRemediesWithoutATerminal asserts what the exec
// plugin says when it has no stored credential and no terminal to sign in
// with: the ways to get one without a terminal - a token piped to auth
// login, filled in with the plugin's --name, the server URL and, for a new
// entry, the CA the call resolved, kubectl
// run once in a terminal, or a kubeconfig that carries its own token -
// from a bare install with nothing prepared as much as from a directory
// in use. The login is the hint only when it runs as written.
func TestTheExecPluginNamesTheRemediesWithoutATerminal(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	srv := localProviderServer(t)
	url := config.CanonicalServerIdentity(srv.URL)
	note := "a credential store change is unfinished; see it with: " + clierr.Rancher + " config status"
	caPEM := selfSignedCAPEM(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(caPEM), 0o600))

	for _, tc := range []struct {
		name string
		// setup prepares the directory; nil is a bare install.
		setup    func(t *testing.T, dir string)
		flags    map[string]string
		login    string
		hint     bool
		withNote bool
		// unreadable: the change record cannot be read, so the login is
		// refused on it and the report is the hint.
		unreadable bool
		// reuse: the directory has token reuse on, so with the cluster every
		// row names the sign-in would store a login credential.
		reuse bool
		// inlineCA: the plugin was given its CA inline, so the login needs
		// a file the message asks for, and is no hint.
		inlineCA bool
	}{
		{name: "a bare install with no --name", login: clierr.Rancher + " auth login <name> --url " + clierr.ShellQuote(url) + " --token-stdin"},
		{name: "a bare install with --name", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true},
		{name: "a --name auth login would refuse", flags: map[string]string{"name": config.ExecLoginPrefix + "x"},
			login: clierr.Rancher + " auth login <name> --url " + clierr.ShellQuote(url) + " --token-stdin"},
		{name: "a --name the directory has at this server, logged out", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod-c-1' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod-c-1": {URL: url}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "an entry-cluster --name, the entry registered here, logged out", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod": {URL: url}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "an entry-cluster --name, the entry registered here, with an inline --cacert", flags: map[string]string{"name": "prod-c-1", "cacert": caPEM},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod": {URL: url, CA: caPEM}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "an entry-cluster --name, the entry registered here, with token reuse on", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true, reuse: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", TokenReuse: true, Servers: map[string]config.Server{"prod": {URL: url}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "a directory whose change record cannot be read", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", unreadable: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte("not: [yaml"), 0o600))
				cfg := &config.Config{CredentialStore: "file", CredentialTransitionID: "0123456789abcdef0123456789abcdef"}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "a directory in use while a store change is unfinished", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true, withNote: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				const id = "0123456789abcdef0123456789abcdef"
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte("version: 1\nid: "+id+"\nsteps:\n- kind: pin\n  target: file\n"), 0o600))
				cfg := &config.Config{CredentialStore: "file", CredentialTransitionID: id, Servers: map[string]config.Server{"other": {URL: "https://other.ex"}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "no --name, with token reuse on", reuse: true,
			login: clierr.Rancher + " auth login <name> --url " + clierr.ShellQuote(url) + " --token-stdin",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", TokenReuse: true}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "a --cacert path", flags: map[string]string{"name": "prod-c-1", "cacert": caPath},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --cacert " + clierr.ShellQuote(caPath) + " --token-stdin", hint: true},
		{name: "an inline --cacert", flags: map[string]string{"name": "prod-c-1", "cacert": caPEM}, inlineCA: true,
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --cacert <file> --token-stdin"},
		{name: "a --cacert path, the --name already registered here", flags: map[string]string{"name": "prod-c-1", "cacert": caPath},
			login: clierr.Rancher + " auth login 'prod-c-1' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true,
			setup: registeredHere(url, "")},
		{name: "an inline --cacert, the --name already registered here", flags: map[string]string{"name": "prod-c-1", "cacert": caPEM},
			login: clierr.Rancher + " auth login 'prod-c-1' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true,
			setup: registeredHere(url, caPEM)},
		{name: "a --cacert path, a reserved --name registered here", flags: map[string]string{"name": config.ExecLoginPrefix + "x", "cacert": caPath},
			login: clierr.Rancher + " auth login <name> --url " + clierr.ShellQuote(url) + " --cacert " + clierr.ShellQuote(caPath) + " --token-stdin",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				// The login the message names is a new entry, so it
				// carries the CA.
				cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{config.ExecLoginPrefix + "x": {URL: url}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "a CA from another entry at this server, for a new --name", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --cacert " + clierr.ShellQuote(caPath) + " --token-stdin", hint: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"other": {URL: url, CA: caPath}}}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
		{name: "the plugin's own login, with token reuse on", flags: map[string]string{"name": "prod-c-1"},
			login: clierr.Rancher + " auth login 'prod' --url " + clierr.ShellQuote(url) + " --token-stdin", hint: true, reuse: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg := &config.Config{CredentialStore: "file", TokenReuse: true}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A bare install has no config directory at all.
			dir := filepath.Join(t.TempDir(), "rancher")
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			flags := map[string]string{"server": srv.URL, "cluster": "c-1"}
			maps.Copy(flags, tc.flags)
			cmd := NewTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
			_, err := cmd.Execute(t.Context(), tin{s: flags}, noTerminal())
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			require.ErrorContains(t, err, "interactive login required for auth config local; this call has no terminal to answer it")
			require.ErrorContains(t, err, "to log in without a terminal, pipe an API token to: "+tc.login)
			switch {
			case tc.reuse:
				require.ErrorContains(t, err, "or run kubectl once in a terminal and sign in")
				assert.NotContains(t, err.Error(), "until the cached token expires", "the plugin's own login keeps a login credential, --name or not")
			case tc.flags["name"] == "":
				assert.NotContains(t, err.Error(), "run kubectl once in a terminal", "with no --name nothing a terminal sign-in makes is kept")
				require.ErrorContains(t, err, "this exec entry sets no --name, and a sign-in in a terminal would not be kept; give it one, for example by regenerating the kubeconfig with: "+clierr.Rancher+" kubeconfig --cluster 'c-1'")
			default:
				require.ErrorContains(t, err, "or run kubectl once in a terminal and sign in, which lasts until the cached token expires")
			}
			// No row's current server holds a login for the call's server, so
			// create kubeconfig, which uses that login, is not offered.
			assert.NotContains(t, err.Error(), "create kubeconfig")
			switch {
			case tc.unreadable:
				assert.Equal(t, clierr.Rancher+" config status", ce.Hint, "the login would be refused on the record")
				require.ErrorContains(t, err, "the credential store change record cannot be read")
				assert.Equal(t, 1, strings.Count(err.Error(), tc.login), "a remedy the message already names is not added again")
			case tc.inlineCA:
				require.ErrorContains(t, err, "this exec entry carries the CA inline and --cacert needs a file; "+config.CAFileMeaning+"; to log in without a terminal")
				assert.Empty(t, ce.Hint, "the login needs a file only the operator can name")
			case tc.hint:
				assert.Equal(t, tc.login, ce.Hint, "the login runs as written")
			default:
				require.ErrorContains(t, err, "<name> stands for a name of your choice")
				assert.Empty(t, ce.Hint, "a command with a placeholder is not a hint")
			}
			switch {
			case tc.withNote:
				require.ErrorContains(t, err, note)
			case !tc.unreadable:
				assert.NotContains(t, err.Error(), "config status", "nothing unfinished to point at")
			}
		})
	}
}

// registeredHere is a setup that registers prod-c-1 at url with the CA
// selector ca, as a login or add-server would leave it: auth login for
// that name uses the entry's own CA and refuses a different --cacert.
func registeredHere(url, ca string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod-c-1": {URL: url, CA: ca}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
	}
}

// TestTheLegacyShimNamesTheRemediesWithoutATerminal asserts the legacy
// shim, the exec plugin of Rancher-generated kubeconfigs, gives the same
// remedies. Its name is a cache slot under a prefix auth login refuses,
// so the login names <name> and no hint is given.
func TestTheLegacyShimNamesTheRemediesWithoutATerminal(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	srv := localProviderServer(t)
	for _, tc := range []struct {
		name  string
		reuse bool
	}{
		{name: "a bare install"},
		// With no cluster the plugin-created login does not run, so a
		// terminal sign-in keeps only the cluster token, as with reuse off.
		{name: "token reuse on, no --cluster", reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "rancher")
			if tc.reuse {
				cfg := &config.Config{CredentialStore: "file", TokenReuse: true}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}
			cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
			_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "user": "alice"}}, noTerminal())
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			require.ErrorContains(t, err, "<name> stands for a name of your choice; to log in without a terminal, pipe an API token to: "+clierr.Rancher+" auth login <name> --url "+clierr.ShellQuote(config.CanonicalServerIdentity(srv.URL))+" --token-stdin")
			require.ErrorContains(t, err, "or run kubectl once in a terminal and sign in, which lasts until the cached token expires")
			assert.Empty(t, ce.Hint, "the shim's slot is no name auth login accepts")
		})
	}
}

// TestAuthLoginKeepsItsOwnNoTerminalRemedy asserts the sign-in prompts
// the plugin shares with auth login name no remedy themselves - each
// command adds its own - and carry the cause both recognize.
func TestAuthLoginKeepsItsOwnNoTerminalRemedy(t *testing.T) {
	t.Parallel()
	srv := localProviderServer(t)
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL}, 0, noTerminal())
	require.Error(t, err)
	require.EqualError(t, err, "interactive login required for auth config local")
	assert.NotContains(t, err.Error(), "create kubeconfig")
	_, ok := errors.AsType[*needsTerminal](err)
	assert.True(t, ok)
}

// TestEverySignInPromptSaysItNeedsATerminal asserts each of the four
// sign-in prompts - SAML and OAuth under --no-input, and the username and
// password prompts with no terminal - refuses with the cause the plugin
// maps to its remedies.
func TestEverySignInPromptSaysItNeedsATerminal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		provider string
		p        *ui.Prompter
	}{
		{"a SAML provider under --no-input", "oktaProvider", ui.Discard()},
		{"an OAuth provider under --no-input", "azureADProvider", ui.Discard()},
		{"the username with no terminal", "localProvider", noTerminal()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"type":"` + tc.provider + `"}]}`))
			}))
			defer srv.Close()
			_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL}, tc.p)
			nt, ok := errors.AsType[*needsTerminal](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, providerName(tc.provider), nt.authConfig)
		})
	}
	t.Run("the password with no terminal", func(t *testing.T) {
		t.Parallel()
		_, err := basicAuth(t.Context(), failingTransportClient(t), &loginInput{caFlag: "--cacert", server: "https://r.ex", authProvider: "localProvider"}, usernameOnly{})
		nt, ok := errors.AsType[*needsTerminal](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, "local", nt.authConfig)
	})
}

// usernameOnly can prompt and answers the username prompt, and refuses the
// password prompt with ErrNoInput, as a terminal that closed does.
type usernameOnly struct{}

func (usernameOnly) Prompt(context.Context, string) (string, error) { return "admin", nil }
func (usernameOnly) PromptSecret(context.Context, string) (string, error) {
	return "", ui.ErrNoInput
}
func (usernameOnly) ReadLine(context.Context) (string, error) { return "", ui.ErrNoInput }
func (usernameOnly) Notice(string)                            {}
func (usernameOnly) CanPrompt() bool                          { return true }
func (usernameOnly) NoInput() bool                            { return false }

// TestNoTerminalRemedyDoesNotSuggestANameRegisteredElsewhere asserts the
// remedy builder alone: a --name the directory registers at another server
// is not suggested as the entry to sign in to, since auth login refuses it.
// The exec plugin itself stops earlier with a typed error for that name, so
// this reaches the builder directly.
func TestNoTerminalRemedyDoesNotSuggestANameRegisteredElsewhere(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod-c-1": {URL: "https://elsewhere.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir)}
	err := cmd.withNoTerminalRemedies(interactiveLoginRequired("localProvider", false),
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.ex"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	require.ErrorContains(t, err, clierr.Rancher+" auth login <name> --url 'https://r.ex' --token-stdin")
	assert.Empty(t, ce.Hint, "a login that would be refused is no hint")
}
