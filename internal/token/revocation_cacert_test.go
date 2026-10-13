package token

import (
	"encoding/pem"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// newTLSRevFixture is newRevFixture over a TLS server, so a revoke reaches
// it only with a CA that verifies its certificate. The fixture's own token
// creation uses the server's client; the revokes under test do not.
func newTLSRevFixture(t *testing.T) (*revFixture, string) {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	f := testrancher.New(testrancher.Options{Users: revocationUsers})
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	x := &revFixture{t: t, f: f, srv: srv, url: srv.URL, dir: t.TempDir()}
	x.store = config.NewFileStore(x.dir)
	cfg := &config.Config{Servers: map[string]config.Server{}, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(x.dir))
	require.NoError(t, cfg.Write())
	return x, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// withCA sets the CA fields of entry name.
func (x *revFixture) withCA(name, ca string, fromEnvironment bool) {
	x.t.Helper()
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers[name]
		s.CA, s.CAFromEnvironment = ca, fromEnvironment
		cfg.Servers[name] = s
	})
}

// TestRevokesConnectWithTheCAOfTheEntry asserts which CA a revoke's request
// is verified against: the entry's saved CA whatever the variable says; the
// variable only for an entry that saves no CA because an auth login
// connected with it; the system roots for any other entry, which fail here
// because the server's certificate is private. Failures stay in the report,
// and the token stays live.
func TestRevokesConnectWithTheCAOfTheEntry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		saved           string
		marked          bool
		variable        string
		rancherURL      string
		wantRevoked     bool
		wantErrContains string
	}{
		{name: "a marked entry uses the variable", marked: true, variable: "right", wantRevoked: true},
		{name: "an unmarked entry with no CA uses system roots", variable: "right", wantErrContains: "certificate"},
		{name: "a saved CA that fits, a variable that does not", saved: "right", variable: "wrong", wantRevoked: true},
		{name: "a saved CA that does not fit ignores a variable that would", saved: "wrong", variable: "right", wantErrContains: "certificate"},
		{name: "a marked entry, the variable for another server", marked: true, variable: "right", rancherURL: "https://other.example", wantErrContains: "certificate"},
		{name: "a marked entry, RANCHER_CLI_URL naming it", marked: true, variable: "right", rancherURL: "SERVER", wantRevoked: true},
		{name: "a marked entry, no variable", marked: true, wantErrContains: "certificate"},
		{name: "a marked entry, an unreadable variable", marked: true, variable: "missing", wantErrContains: "reading RANCHER_CLI_CACERT: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, right := newTLSRevFixture(t)
			wrong := selfSignedCAPEM(t)
			value := func(v string) string {
				switch v {
				case "right":
					return right
				case "wrong":
					return wrong
				case "missing":
					return filepath.Join(t.TempDir(), "missing.pem")
				}
				return ""
			}
			login := x.login("prod", "alice")
			x.withCA("prod", value(tc.saved), tc.marked)
			t.Setenv(config.EnvCACert, value(tc.variable))
			rancherURL := tc.rancherURL
			if rancherURL == "SERVER" {
				rancherURL = x.url
			}
			t.Setenv(config.EnvURL, rancherURL)

			_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)
			if tc.wantRevoked {
				assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
				assert.False(t, x.alive(login))
				return
			}
			assert.Empty(t, revokedNames(rep))
			assert.True(t, x.alive(login), "the token was not reached")
			n := notRevoked(t, rep, login.Ref.Name)
			assert.Equal(t, "failed", n.Reason)
			assert.Contains(t, n.Error, tc.wantErrContains)
		})
	}
}

// TestLogoutAllWithTheVariableRevokesEveryEntryItRevokesWithout asserts
// auth logout --all with the variable set to the server's CA reaches every
// entry it reaches today: one that saves the CA, and one a
// variable-connected login saved without it.
func TestLogoutAllWithTheVariableRevokesEveryEntryItRevokesWithout(t *testing.T) {
	x, right := newTLSRevFixture(t)
	saved := x.login("saved", "alice")
	marked := x.login("marked", "bob")
	x.withCA("saved", right, false)
	x.withCA("marked", "", true)
	t.Setenv(config.EnvCACert, right)

	_, rep, err := x.run(x.req(DeleteForLogoutAll, "saved", "marked"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{saved.Ref.Name, marked.Ref.Name}, revokedNames(rep))
	assert.Empty(t, rep.NotRevoked)
}

// TestTrustForRanksAnEntryThatConnectsWithTheVariableAfterOneWithACA asserts
// among entries that name a server and none of which is the record's parent,
// the revoke goes through the one with a fingerprint, then one with a CA,
// then a marked one, then the first by name.
func TestTrustForRanksAnEntryThatConnectsWithTheVariableAfterOneWithACA(t *testing.T) {
	const url = "https://r.ex"
	build := func(entries map[string]config.Server) *config.Config {
		cfg := &config.Config{Servers: entries}
		return cfg
	}
	pick := func(entries map[string]config.Server) string {
		name, ok := trustFor(build(entries), "none", url)
		require.True(t, ok)
		return name
	}
	assert.Equal(t, "b-marked", pick(map[string]config.Server{
		"a-plain":  {URL: url},
		"b-marked": {URL: url, CAFromEnvironment: true},
	}), "a marked entry beats the first by name")
	assert.Equal(t, "z-ca", pick(map[string]config.Server{
		"a-marked": {URL: url, CAFromEnvironment: true},
		"z-ca":     {URL: url, CA: "ca"},
	}), "an entry with a CA beats a marked one")
	assert.Equal(t, "z-fingerprint", pick(map[string]config.Server{
		"a-ca":          {URL: url, CA: "ca"},
		"b-marked":      {URL: url, CAFromEnvironment: true},
		"z-fingerprint": {URL: url, CA: "ca", CAFingerprint: "f"},
	}), "a fingerprint beats a CA")
	assert.Equal(t, "a-plain", pick(map[string]config.Server{
		"a-plain": {URL: url},
		"b-plain": {URL: url},
	}), "otherwise the first by name")
	name, ok := trustFor(build(map[string]config.Server{"p": {URL: url}}), "p", url)
	require.True(t, ok)
	assert.Equal(t, "p", name, "the record's parent, when it names the server")
}

// TestAManualRecoveryForANoEntryTokenAsksForThePrivateCA asserts the warning
// line of a token no entry names carries the private-CA sentence beside the
// recovery sequence, and no recovery list in the report does; a manual
// recovery of another reason carries none.
func TestAManualRecoveryForANoEntryTokenAsksForThePrivateCA(t *testing.T) {
	const sentence = "for a server whose certificate a private CA signs, add --cacert <file with the server's CA certificate> to the login, or set RANCHER_CLI_CACERT to that file"

	t.Run("noEntry", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("k", ct, x.serverWide(), true)
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.URL = "https://elsewhere.ex"
			cfg.Servers["prod"] = s
		})
		req := x.req(DeleteForDeleteToken)
		req.Select = func(config.CachedRecord) bool { return true }
		d, rep, err := x.run(req)
		require.NoError(t, err)
		n := notRevoked(t, rep, ct.Ref.Name)
		require.Equal(t, "noEntry", n.Reason)
		require.NotEmpty(t, n.Recovery)
		for _, r := range n.Recovery {
			assert.NotContains(t, r, "private CA", "a recovery list is commands alone")
			assert.NotContains(t, r, config.EnvCACert)
		}
		var line string
		for _, w := range d.Warnings() {
			if strings.Contains(w, ct.Ref.Name) {
				line = w
			}
		}
		require.NotEmpty(t, line)
		assert.Contains(t, clierr.ExpandRemedies(line, ""), "; "+sentence)
		assert.True(t, strings.HasSuffix(line, sentence), "the sentence ends the line")
	})

	t.Run("another reason", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("k", ct, x.own("prod"), true)
		d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
		require.NoError(t, err)
		d.RevokeTokens(t.Context())
		rep := d.Report()
		n := notRevoked(t, rep, ct.Ref.Name)
		require.Equal(t, "otherServer", n.Reason)
		var line string
		for _, w := range d.Warnings() {
			if strings.Contains(w, ct.Ref.Name) {
				line = w
			}
		}
		require.NotEmpty(t, line)
		assert.NotContains(t, line, "private CA")
		assert.NotContains(t, line, config.EnvCACert)
	})
}
