package auth

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

const flowSeed = "seed:secret"

// flowServer is a TLS Rancher whose certificate only the returned CA trusts,
// serving the token check and the local provider sign-in as well as the
// version preflight.
func flowServer(t *testing.T) (url, ca string) {
	t.Helper()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	f := testrancher.New(testrancher.Options{SeedToken: flowSeed, SelfSubjectReviews: true})
	srv := newTLSFacade(t, fix, f)
	return srv.URL, fix.pem
}

// productionLoginCmd wires the login as NewLogin does: the real preflight
// and the real token check, so a request under the variable goes through the
// production client with the CA the login chose.
func productionLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: register.TLSPreflight,
		verify:       provider.Owner,
	}
}

type loginCase struct {
	name  string
	known bool
	url   bool
}

var loginCases = []loginCase{
	{"--url, new entry", false, true},
	{"--url, known entry with a different saved CA", true, true},
	{"plain, known entry with a different saved CA", true, false},
}

func (c loginCase) setup(t *testing.T, url string) (string, config.CredStore, cmdtest.Input) {
	t.Helper()
	dir, store := cmdtest.FilePolicyDir(t)
	if c.known {
		seedServer(t, dir, "prod", url, otherCA(t), false)
	}
	in := plainLoginInput("prod")
	if c.url {
		in = urlLoginInput("prod", url)
	}
	return dir, store, in
}

// withLocalProvider turns a login input into a localProvider sign-in.
func withLocalProvider(in cmdtest.Input) cmdtest.Input {
	str := map[string]string{"auth-provider": "localProvider"}
	maps.Copy(str, in.Str)
	return cmdtest.Input{Str: str, Pos: in.Pos}
}

func (c loginCase) notice() string {
	if c.known {
		return differentCANotice("prod")
	}
	return noCANotice("prod")
}

// TestLoginUnderTheVariableChecksTheTokenWithTheVariablesCA asserts the
// token check of a login that connected with RANCHER_CLI_CACERT uses that CA
// and not the entry's saved one, which does not fit the server: the check
// succeeds, the login is verified, and the entry keeps its saved CA.
func TestLoginUnderTheVariableChecksTheTokenWithTheVariablesCA(t *testing.T) {
	for _, tc := range loginCases {
		t.Run(tc.name, func(t *testing.T) {
			url, ca := flowServer(t)
			t.Setenv(config.EnvCACert, ca)
			dir, store, in := tc.setup(t, url)
			before, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)

			res, err := productionLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter(flowSeed+"\n"))
			require.NoError(t, err)
			assert.Equal(t, true, res.Data.(map[string]any)["verified"])
			assert.True(t, hasNotice(res, tc.notice()), "warnings: %v", res.Warnings)
			after, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			assert.Equal(t, before.Servers["prod"].CA, after.Servers["prod"].CA, "the saved CA is unchanged")
			assert.Equal(t, testrancher.SeedOwner, after.Servers["prod"].TokenUserID)
		})
	}
}

// TestLoginSignInUnderTheVariableConnectsWithTheVariablesCA asserts a
// localProvider sign-in under the variable reaches the server with the
// variable's CA, where the entry's saved CA would fail.
func TestLoginSignInUnderTheVariableConnectsWithTheVariablesCA(t *testing.T) {
	for _, tc := range loginCases {
		t.Run(tc.name, func(t *testing.T) {
			url, ca := flowServer(t)
			t.Setenv(config.EnvCACert, ca)
			dir, store, in := tc.setup(t, url)
			in = withLocalProvider(in)

			p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
			res, err := productionLoginCmd(dir, store).Execute(t.Context(), in, p)
			require.NoError(t, err)
			assert.True(t, hasNotice(res, tc.notice()), "warnings: %v", res.Warnings)
			tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
			require.NoError(t, terr)
			assert.NotEmpty(t, tok)
		})
	}
}

// TestAFailedSignInUnderTheVariableStillCarriesTheNotice asserts the notice
// reaches the decoded failure when the sign-in itself fails (a wrong
// password), on the delegated return and the plain one, for a new entry and
// for a known entry whose saved CA differs.
func TestAFailedSignInUnderTheVariableStillCarriesTheNotice(t *testing.T) {
	for _, tc := range loginCases {
		t.Run(tc.name, func(t *testing.T) {
			url, ca := flowServer(t)
			t.Setenv(config.EnvCACert, ca)
			dir, store, in := tc.setup(t, url)
			in = withLocalProvider(in)

			p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "wrong-password"), false, true)
			_, err := productionLoginCmd(dir, store).Execute(t.Context(), in, p)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeAuth, ce.Code, "the sign-in itself is refused, not the connection: %v", err)
			assert.True(t, slices.Contains(ce.Warnings(), tc.notice()), "warnings: %v", ce.Warnings())
			_, terr := store.GetToken(config.ServerRef{Name: "prod"})
			assert.Error(t, terr, "nothing is stored")
		})
	}
}

// TestPlainLoginOfAnHTTPEntryGivesNoVariableNotice asserts the plain login
// of an http:// entry with the variable set connects with no CA: no notice
// and no record that the variable supplied the connection.
func TestPlainLoginOfAnHTTPEntryGivesNoVariableNotice(t *testing.T) {
	t.Setenv(config.EnvCACert, otherCA(t))
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "dev", "http://127.0.0.1:1", "", false)

	res, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("dev"), cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Empty(t, noticesOfTheVariable(res))
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.False(t, cfg.Servers["dev"].CAFromEnvironment)
}
