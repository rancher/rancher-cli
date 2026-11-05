package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// loginFlags are the flags the schema golden lists for auth login, with their
// dashes, and the global flags. The golden is read by path because the
// package holding the schema tests imports this one.
func loginFlags(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "cli", "testdata", "schema.golden.json"))
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
		if c.Name == "auth login" {
			found = true
			for _, f := range c.Flags {
				flags["--"+f.Name] = true
			}
		}
	}
	require.True(t, found, "the schema has no auth login")
	return flags
}

var (
	pastedCommandRE = regexp.MustCompile(regexp.QuoteMeta(clierr.Rancher) + `[^;]*`)
	flagInProseRE   = regexp.MustCompile(`--[a-z][a-z0-9-]*`)
)

// assertOnlyLoginFlags asserts the prose of text, without its pasted rancher
// commands (which the conformance tests check against their own commands),
// names only flags auth login has.
func assertOnlyLoginFlags(t *testing.T, text string) {
	t.Helper()
	have := loginFlags(t)
	for _, f := range flagInProseRE.FindAllString(pastedCommandRE.ReplaceAllString(text, ""), -1) {
		assert.True(t, have[f], "the text names %s, which auth login does not have: %q", f, text)
	}
}

// refusalKind is a way auth login refuses a sign-in that carries a remedy:
// the --auth-provider value, whether --no-input is given, the code, the
// refusal's own text, and the part of the remedy that holds no command.
type refusalKind struct {
	name     string
	provider string
	noInput  bool
	code     string
	refusal  string
	lead     string
	fallback string
}

const (
	leadTerminal = "; run in a terminal, or to log in with an API token, pipe it to: "
	leadNoInput  = "; run without --no-input, or to log in with an API token, pipe it to: "
	leadAPIToken = "; to log in with an API token, pipe it to: "
)

var refusalKinds = []refusalKind{
	{"no terminal", "localProvider", false, clierr.CodeAuth,
		"interactive login required for auth config local", leadTerminal, "; run in a terminal"},
	{"--no-input at the password prompt", "localProvider", true, clierr.CodeAuth,
		"interactive login required for auth config local", leadNoInput, "; run without --no-input"},
	{"--no-input for a SAML auth config", "oktaProvider", true, clierr.CodeAuth,
		"interactive login required for auth config okta", leadNoInput, "; run without --no-input"},
	{"--no-input for an OAuth auth config", "azureADProvider", true, clierr.CodeAuth,
		"interactive login required for auth config azuread", leadNoInput, "; run without --no-input"},
	{"an auth config this CLI cannot sign in with", "corp-gh", true, clierr.CodeCapabilityUnsupported,
		"--auth-provider corp-gh names an auth config this CLI cannot sign in with: corp-gh (githubProvider); " +
			"advertised by the server but not supported by this CLI: githubProvider (server change requested: rancher/rancher#57034); " +
			"to sign in as a user of one of them, create an API token in the Rancher UI and log in with it using auth login --token-stdin",
		leadAPIToken, ""},
}

// refusalServer answers the list of auth configs with one of each kind of
// refusal of refusalKinds. onList runs before it answers, which is the window
// between the registration of a new entry and the recheck of it. A non-nil
// fix serves TLS with its certificate.
func refusalServer(t *testing.T, onList func(), fix *tlsFixture) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			if onList != nil {
				onList()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"local","type":"localProvider"},{"id":"okta","type":"oktaProvider"},` +
				`{"id":"azuread","type":"azureADProvider"},{"id":"corp-gh","type":"githubProvider"}]}`))
		case r.URL.Path == "/rancherversion":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.20.0"}`))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewUnstartedServer(h)
	if fix != nil {
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{fix.cert}}
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

// changeEntry changes the entry name in the config of dir through the config
// API, under its lock, as a concurrent command would: with a URL it is
// repointed, without one it is deleted.
func changeEntry(t *testing.T, dir, name, url string) func() {
	t.Helper()
	return func() {
		err := config.WithLock(t.Context(), dir, func() error {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return err
			}
			if url == "" {
				delete(cfg.Servers, name)
			} else {
				s := cfg.Servers[name]
				s.URL = url
				cfg.Servers[name] = s
			}
			return cfg.Write()
		})
		assert.NoError(t, err)
	}
}

// serverWithCA is refusalServer for a login that names the server's CA: a TLS
// server with a certificate of its own and the path of a file holding it, or a
// plain server and no path.
func serverWithCA(t *testing.T, onList func(), private bool) (*httptest.Server, string) {
	t.Helper()
	if !private {
		return refusalServer(t, onList, nil), ""
	}
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, []byte(fix.pem), 0o600))
	return refusalServer(t, onList, &fix), caFile
}

func refuse(t *testing.T, dir string, store config.CredStore, in cmdtest.Input, noInput bool) *clierr.Error {
	t.Helper()
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, strings.NewReader(""), noInput, false))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	return ce
}

// TestAuthLoginRefusalsNameOnlyItsFlagsAndCarryTheirRemedy asserts, for each
// refusal that carries a remedy - no terminal, --no-input, an auth config
// this CLI cannot sign in with - and for each path of auth login, the text
// auth login prints: every flag its prose names is a flag of auth login or a
// global flag (a pasted rancher command is left to the conformance tests),
// and the remedy is there. Paths: a registered entry; a new name with --url
// while the entry is as registered, where the piped login comes before the
// undo of the registration, and its hint is the piped login; a new name with
// --url whose entry was deleted or repointed before the recheck, with and
// without a private CA, where no command and no hint is valid and the message
// sends the person to list the entries. The sign-in prompts that refuse after a
// terminal answered need a prompter that can prompt; the token package's
// tests cover them. Mutations: a flag auth login lacks in a lead, in the
// text the decoration writes, or in a text added after the call that adds the
// remedy; the remedy call removed; the remedy applied after the decoration on
// every path.
func TestAuthLoginRefusalsNameOnlyItsFlagsAndCarryTheirRemedy(t *testing.T) {
	t.Parallel()
	piped := func(name, url string) string { return pipedLoginHint(name, url) }
	undo := "; or undo the registration with: " + clierr.Rancher + " config delete-server 'new'"
	for _, k := range refusalKinds {
		in := func(extra map[string]string, name string) cmdtest.Input {
			str := map[string]string{"auth-provider": k.provider}
			maps.Copy(str, extra)
			return cmdtest.Input{Str: str, Pos: []string{name}}
		}
		t.Run(k.name+", a registered entry", func(t *testing.T) {
			t.Parallel()
			dir, store := cmdtest.FilePolicyDir(t)
			srv := refusalServer(t, nil, nil)
			seedServer(t, dir, "prod", srv.URL, "", false)
			ce := refuse(t, dir, store, in(nil, "prod"), k.noInput)
			assert.Equal(t, k.code, ce.Code)
			assert.Equal(t, k.refusal+k.lead+piped("prod", srv.URL), ce.Message)
			assert.Equal(t, piped("prod", srv.URL), ce.Hint)
			assertOnlyLoginFlags(t, ce.Message)
		})
		for _, private := range []bool{false, true} {
			name := k.name + ", a new name with --url, the entry as registered"
			if private {
				name += ", a private CA"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir, store := cmdtest.FilePolicyDir(t)
				srv, caFile := serverWithCA(t, nil, private)
				extra := map[string]string{"url": srv.URL}
				if private {
					extra["cacert"] = caFile
				}
				ce := refuse(t, dir, store, in(extra, "new"), k.noInput)
				assert.Equal(t, k.code, ce.Code)
				// The registered entry carries the CA, so the login names none.
				assert.Equal(t, k.refusal+k.lead+piped("new", srv.URL)+undo, ce.Message)
				assert.Equal(t, piped("new", srv.URL), ce.Hint, "the login that works, not the one that was refused")
				assert.Less(t, strings.Index(ce.Message, "--token-stdin"), strings.Index(ce.Message, "or undo the registration"), "the login comes before the undo")
				assertOnlyLoginFlags(t, ce.Message)
			})
		}
		for _, change := range []struct {
			name string
			url  string
		}{
			{"deleted", ""},
			{"repointed", "https://other.example.com"},
		} {
			for _, private := range []bool{false, true} {
				name := k.name + ", a new name with --url, the entry " + change.name
				if private {
					name += ", a private CA"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					dir, store := cmdtest.FilePolicyDir(t)
					srv, caFile := serverWithCA(t, changeEntry(t, dir, "new", change.url), private)
					extra := map[string]string{"url": srv.URL}
					if private {
						extra["cacert"] = caFile
					}
					ce := refuse(t, dir, store, in(extra, "new"), k.noInput)
					assert.Equal(t, k.code, ce.Code)
					assert.Equal(t, k.refusal+k.fallback+
						`; server entry "new" may have changed since registration; check it with: `+clierr.Rancher+" config list-servers", ce.Message)
					assert.Empty(t, ce.Hint)
					assert.NotContains(t, ce.Message, clierr.Rancher+" auth login", "no piped login is named for an entry that changed")
					assertOnlyLoginFlags(t, ce.Message)
				})
			}
		}
	}
}

// remedyFor is a piped login remedy with every part set, for the tests that
// call the decoration directly.
func remedyFor() token.LoginRefusalRemedy {
	return token.LoginRefusalRemedy{
		Instruction: "run in a terminal, or to log in with an API token, pipe it to: " + pipedLoginHint("prod", "https://r.ex"),
		Hint:        pipedLoginHint("prod", "https://r.ex"),
		Fallback:    "run in a terminal",
	}
}

// TestTheDecorationReturnsAFailureItCannotRecheckUnchanged asserts the early
// exit of decorateDelegatedFailure: a failure that is not a structured error,
// and a structured one whose context is already cancelled, come back as the
// same error, with the message and hint they had and no remedy, though the
// entry matches its snapshot and the remedy is set. The row of the plain
// error only fails a change that turns it into a structured one; the
// cancelled row fails a remedy added before the exit, and a recheck that runs
// despite the cancellation. Mutations: apply the remedy before the early
// exit; drop the context check from it.
func TestTheDecorationReturnsAFailureItCannotRecheckUnchanged(t *testing.T) {
	t.Parallel()
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	snap := register.AuthSnapshot{Name: "prod", CanonicalURL: "https://r.ex"}

	failure := clierr.New(clierr.CodeCapabilityUnsupported, "refused").WithHint("keep this hint")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	got := decorateDelegatedFailure(cancelled, dir, failure, snap, "", remedyFor())
	assert.Same(t, failure, got)
	assert.Equal(t, "refused", failure.Message)
	assert.Equal(t, "keep this hint", failure.Hint)

	plain := errors.New("plain failure")
	assert.Same(t, plain, decorateDelegatedFailure(t.Context(), dir, plain, snap, "", remedyFor()))
}

// TestTheDecorationKeepsTheCallersAlternativeAndTheRemedy asserts that when a
// caller gives both its own alternative (extra) and a remedy, the message
// keeps both, extra first, as the entry is as registered (the undo ends the
// message and the remedy's hint is the hint) and as it changed (the
// command-free part of the remedy follows extra and no hint is set). No
// caller passes both today. Mutation: let the remedy replace extra in either
// branch.
func TestTheDecorationKeepsTheCallersAlternativeAndTheRemedy(t *testing.T) {
	t.Parallel()
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	const extra = "bypass verification with --no-verify"
	remedy := remedyFor()

	current := asClierr(t, decorateDelegatedFailure(t.Context(), dir, clierr.New(clierr.CodeGeneric, "failed"),
		register.AuthSnapshot{Name: "prod", CanonicalURL: "https://r.ex"}, extra, remedy))
	assert.Equal(t, "failed; "+extra+"; "+remedy.Instruction+"; or undo the registration with: "+clierr.Rancher+" config delete-server 'prod'", current.Message)
	assert.Equal(t, remedy.Hint, current.Hint)

	changed := asClierr(t, decorateDelegatedFailure(t.Context(), dir, clierr.New(clierr.CodeGeneric, "failed"),
		register.AuthSnapshot{Name: "prod", CanonicalURL: "https://other.example.com"}, extra, remedy))
	assert.Equal(t, "failed; "+extra+"; "+remedy.Fallback+`; server entry "prod" may have changed since registration; check it with: `+clierr.Rancher+" config list-servers", changed.Message)
	assert.Empty(t, changed.Hint)
}
