package token

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// refusalSite is one place a sign-in prompt refuses because nobody can answer
// it, with what reaches it: the --auth-provider value that picks the auth
// config of its flow and the prompter that makes it refuse. callSite names the
// interactiveLoginRequired call the row reaches. The last three fields are the
// evidence that it did, which differs between the sites: the prompts the
// prompter was asked before the refusal (the start of each label: the legacy
// shim's username prompt names a default), whether --no-input is the reason the
// refusal records, and the auth config it names.
type refusalSite struct {
	name        string
	callSite    string
	value       string
	prompter    func() runtime.Prompter
	wantPrompts []string
	wantNoInput bool
	wantConfig  string
}

var refusalSites = []refusalSite{
	{"SAML under --no-input", "loginAndGenerateCred SAML", "okta", func() runtime.Prompter { return ui.Discard() }, nil, true, "okta"},
	{"no terminal", "basicAuth first check", "local", func() runtime.Prompter { return noTerminal() }, nil, false, "local"},
	{"--no-input with a terminal", "basicAuth first check", "local", func() runtime.Prompter { return noInputWithTerminal() }, nil, true, "local"},
	{"the username prompt refused", "basicAuth username", "local", func() runtime.Prompter { return usernameRefused{} }, []string{"Enter username"}, false, "local"},
	{"the password prompt refused", "basicAuth password", "local", func() runtime.Prompter { return usernameOnly{} }, []string{"Enter username", "Enter password"}, false, "local"},
	{"OAuth under --no-input", "oauthAuth", "azuread", func() runtime.Prompter { return ui.Discard() }, nil, true, "azuread"},
}

// recordingPrompter records the labels of the prompts it is asked.
type recordingPrompter struct {
	runtime.Prompter
	asked []string
}

func (r *recordingPrompter) Prompt(ctx context.Context, label string) (string, error) {
	r.asked = append(r.asked, label)
	return r.Prompter.Prompt(ctx, label)
}

func (r *recordingPrompter) PromptSecret(ctx context.Context, label string) (string, error) {
	r.asked = append(r.asked, label)
	return r.Prompter.PromptSecret(ctx, label)
}

// recorded is the site's prompter wrapped to record its prompts.
func (s refusalSite) recorded() *recordingPrompter {
	return &recordingPrompter{Prompter: s.prompter()}
}

// assertReached asserts the refusal err is the one of the site: the auth
// config it names, whether --no-input is its reason, and the prompts asked
// before it. A row whose prompter or value reaches another site fails here.
func (s refusalSite) assertReached(t *testing.T, err error, p *recordingPrompter) {
	t.Helper()
	nt, ok := errors.AsType[*needsTerminal](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, s.wantConfig, nt.authConfig, "the auth config of the site %q", s.callSite)
	assert.Equal(t, s.wantNoInput, nt.noInput, "--no-input as the reason at the site %q", s.callSite)
	require.Len(t, p.asked, len(s.wantPrompts), "the prompts asked before the site %q: %q", s.callSite, p.asked)
	for i, want := range s.wantPrompts {
		assert.True(t, strings.HasPrefix(p.asked[i], want), "prompt %d before the site %q is %q, want one starting %q", i, s.callSite, p.asked[i], want)
	}
}

// goldenFlags are the flags the schema golden lists for command, with their
// dashes, and the global flags.
func goldenFlags(t *testing.T, command string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "cli", "testdata", "schema.golden.json"))
	require.NoError(t, err)
	var golden struct {
		Commands []struct {
			Name  string `json:"name"`
			Flags []struct {
				Name string `json:"name"`
			} `json:"flags"`
		} `json:"commands"`
		GlobalFlags []struct {
			Name string `json:"name"`
		} `json:"globalFlags"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	flags := map[string]bool{}
	for _, g := range golden.GlobalFlags {
		for _, f := range regexp.MustCompile(`--[a-z][a-z0-9-]*`).FindAllString(g.Name, -1) {
			flags[f] = true
		}
	}
	found := false
	for _, c := range golden.Commands {
		if c.Name == command {
			found = true
			for _, f := range c.Flags {
				flags["--"+f.Name] = true
			}
		}
	}
	require.True(t, found, "the schema has no command %q", command)
	return flags
}

var pastedCommandRE = regexp.MustCompile(regexp.QuoteMeta(clierr.Rancher) + `[^;]*`)
var flagInProseRE = regexp.MustCompile(`--[a-z][a-z0-9-]*`)

// assertFlagsExist asserts every flag the prose of text names is a flag of
// command. A pasted rancher command, which runs to the next semicolon, is
// checked against its own command by the conformance tests.
func assertFlagsExist(t *testing.T, command, text string) {
	t.Helper()
	have := goldenFlags(t, command)
	for _, f := range flagInProseRE.FindAllString(pastedCommandRE.ReplaceAllString(text, ""), -1) {
		assert.True(t, have[f], "%s names %s, which %s does not have: %q", "the refusal", f, command, text)
	}
}

// TestEveryRefusalToPromptNamesOnlyFlagsOfTheCommandThatShowsIt asserts the
// rule for every call of interactiveLoginRequired, the refusal of a sign-in
// prompt nobody can answer: the text each command shows names only flags
// that command has, as the schema lists them. The rows run the real
// refusals - auth login's text is the one AuthLoginRemedy gives the error of
// each site, auth get-token's comes out of the exec plugin, through its
// sign-in lanes (the cluster-scoped sign-in, the legacy shim without a cluster
// and the plugin-created login) - so a command that stops adding its own
// remedy shows a text with no flags or the wrong ones. The prompts that
// refuse after a terminal answered need a prompter that can prompt, which only
// a test of the package can give. Mutations: name --name in an auth login
// text (its rows fail); return the error unchanged from the legacy shim's
// sign-in (its rows fail); give a row the prompter of another site, or swap
// the okta and azuread values of the two --no-input rows (the evidence of the
// site, assertReached, fails).
func TestEveryRefusalToPromptNamesOnlyFlagsOfTheCommandThatShowsIt(t *testing.T) {
	isolateSignIn(t)
	opts := testrancher.Options{EnableIdP: true, AuthConfigs: []testrancher.AuthConfig{{Name: "okta", Type: "oktaProvider"}}}

	t.Run("auth login", func(t *testing.T) {
		x := newOwnerFixture(t, false, opts)
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: x.serverID()}}}
		for _, site := range refusalSites {
			t.Run(site.name, func(t *testing.T) {
				p := site.recorded()
				_, err := loginAndGenerateCred(t.Context(), x.srv.Client(), &loginInput{caFlag: "--cacert", server: x.srv.URL, authProvider: site.value}, p)
				require.Error(t, err)
				site.assertReached(t, err, p)
				got := AuthLoginRemedy(err, cfg, "prod", x.serverID(), "")
				ce, ok := errors.AsType[*clierr.Error](got)
				require.True(t, ok, "got %v", got)
				assert.Contains(t, ce.Message, "to log in with an API token, pipe it to: ", "the remedy of auth login")
				assertFlagsExist(t, "auth login", ce.Message)
				assertFlagsExist(t, "auth login", ce.Hint)
			})
		}
	})

	for _, lane := range []struct {
		name  string
		reuse bool
		call  authCall
	}{
		{"auth get-token, the cluster-scoped sign-in", false, authCall{cluster: "c-test", name: "n"}},
		{"auth get-token, the plugin-created login", true, authCall{cluster: "c-test", name: "n"}},
	} {
		t.Run(lane.name, func(t *testing.T) {
			x := newOwnerFixture(t, lane.reuse, opts)
			for _, site := range refusalSites {
				t.Run(site.name, func(t *testing.T) {
					c, p := lane.call, site.recorded()
					c.authConfig, c.prompter = site.value, p
					_, err := x.executeWith(c)
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok, "got %v", err)
					site.assertReached(t, err, p)
					assert.Contains(t, ce.Message, "this call has no terminal to answer it", "the remedy of the exec plugin")
					assertFlagsExist(t, "auth get-token", ce.Message)
					assertFlagsExist(t, "auth get-token", ce.Hint)
				})
			}
		})
	}

	t.Run("token, the legacy shim without a cluster", func(t *testing.T) {
		x := newOwnerFixture(t, false, opts)
		for _, site := range refusalSites {
			t.Run(site.name, func(t *testing.T) {
				dir := t.TempDir()
				cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
				p := site.recorded()
				_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "auth-provider": site.value}}, p)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				site.assertReached(t, err, p)
				assert.Contains(t, ce.Message, "this call has no terminal to answer it", "the remedy of the exec plugin")
				assertFlagsExist(t, "token", ce.Message)
				assertFlagsExist(t, "token", ce.Hint)
			})
		}
	})
}

// TestEveryCallOfInteractiveLoginRequiredHasARowInTheFlagGuard asserts the
// calls of interactiveLoginRequired in the package's own files, not counting
// its declaration, are the ones the rows of
// TestEveryRefusalToPromptNamesOnlyFlagsOfTheCommandThatShowsIt reach, so a
// new call cannot skip the table. Mutation: add a call with no row.
func TestEveryCallOfInteractiveLoginRequiredHasARowInTheFlagGuard(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	calls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "interactiveLoginRequired" {
					calls++
				}
			}
			return true
		})
	}
	var sites []string
	for _, s := range refusalSites {
		if !slices.Contains(sites, s.callSite) {
			sites = append(sites, s.callSite)
		}
	}
	assert.Equal(t, len(sites), calls, "every call of interactiveLoginRequired needs a row in refusalSites that reaches it")
}
