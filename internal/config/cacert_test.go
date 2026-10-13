package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestEnvCACertForAppliesTheVariableToTheServerRancherCLIURLNames asserts
// the one reader of RANCHER_CLI_CACERT: the variable applies to any server
// while RANCHER_CLI_URL is unset, and to the server RANCHER_CLI_URL names
// when it is set, never to another one.
func TestEnvCACertForAppliesTheVariableToTheServerRancherCLIURLNames(t *testing.T) {
	for _, tc := range []struct {
		name, url, variable, target, want string
	}{
		{"unset variable", "", "", "https://a.example", ""},
		{"no URL variable applies everywhere", "", "CA", "https://a.example", "CA"},
		{"no URL variable applies with no target", "", "CA", "", "CA"},
		{"the same server", "https://a.example", "CA", "https://a.example", "CA"},
		{"the same server spelled differently", "https://a.example/", "CA", "HTTPS://A.example", "CA"},
		{"another server", "https://a.example", "CA", "https://b.example", ""},
		{"another port", "https://a.example", "CA", "https://a.example:8443", ""},
		{"a URL with no variable", "https://a.example", "", "https://a.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvURL, tc.url)
			t.Setenv(EnvCACert, tc.variable)
			assert.Equal(t, tc.want, EnvCACertFor(tc.target))
		})
	}
}

// TestEffectiveSaysWhereItsCAComesFrom asserts Effective.CASource: the
// variable, the current server entry with its name, or none, and none too
// when a RANCHER_CLI_URL that names another server cleared the entry's CA.
func TestEffectiveSaysWhereItsCAComesFrom(t *testing.T) {
	c := &Config{
		CurrentServer: "prod",
		Servers: map[string]Server{
			"prod":  {URL: "https://prod.example.com", CA: "PROD-CA"},
			"plain": {URL: "https://plain.example.com"},
		},
	}
	for _, tc := range []struct {
		name      string
		cfg       *Config
		url, ca   string
		wantCA    string
		wantFrom  CASource
		wantNamed map[string]bool
	}{
		{"the current entry's CA", c, "", "", "PROD-CA", CASource{Kind: CASourceEntry, Entry: "prod"}, map[string]bool{"prod": true}},
		{"the variable over the entry's CA", c, "", "ENV-CA", "ENV-CA", CASource{Kind: CASourceVariable}, map[string]bool{"prod": true}},
		{"a divergent URL clears the entry's CA", c, "https://staging.example.com", "", "", CASource{}, map[string]bool{"prod": true}},
		{"a divergent URL and the variable", c, "https://staging.example.com", "ENV-CA", "ENV-CA", CASource{Kind: CASourceVariable}, map[string]bool{"prod": true}},
		{"the same URL keeps the entry's CA", c, "https://prod.example.com/", "", "PROD-CA", CASource{Kind: CASourceEntry, Entry: "prod"}, map[string]bool{"prod": true}},
		{"an entry with no CA", &Config{CurrentServer: "plain", Servers: c.Servers}, "", "", "", CASource{}, map[string]bool{"prod": true}},
		{"no entries", &Config{}, "", "", "", CASource{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvURL, tc.url)
			t.Setenv(EnvCACert, tc.ca)
			eff := tc.cfg.EffectiveConnection()
			assert.Equal(t, tc.wantCA, eff.CA)
			assert.Equal(t, tc.wantFrom, eff.CASource)
			assert.Equal(t, tc.wantNamed, eff.ServersWithCA, "the entries that save a CA")
		})
	}
}

// TestInlineCAClauseNamesWhereTheInlineCAIsCarried asserts the clause that
// follows an auth login command whose CA is a PEM: the variable, a server
// entry in config.yaml, or the exec entry.
func TestInlineCAClauseNamesWhereTheInlineCAIsCarried(t *testing.T) {
	const lead = ", adding --cacert with the path of a file that holds the server's CA certificate, which "
	assert.Equal(t, lead+"RANCHER_CLI_CACERT carries inline", InlineCAClause(CASource{Kind: CASourceVariable}))
	assert.Equal(t, lead+"server entry 'x' carries inline in config.yaml", InlineCAClause(CASource{Kind: CASourceEntry, Entry: "x"}))
	assert.Equal(t, lead+"this exec entry carries inline", InlineCAClause(CASource{Kind: CASourceFlag}))
	assert.Equal(t, lead+"this exec entry carries inline", InlineCAClause(CASource{}))
}

// TestNotLoggedInForAPluginLoginEntryWithASavedCAAddsTheCA asserts the
// refusal when the one server entry is the exec plugin's own login entry:
// with a saved CA the login adds the CA, which that entry carries inline,
// and the error has no hint, since the login would not run as written;
// without one the hint stays.
func TestNotLoggedInForAPluginLoginEntryWithASavedCAAddsTheCA(t *testing.T) {
	t.Parallel()
	const entry = "exec-login-r.ex"
	servers := map[string]string{entry: "https://r.ex"}
	login := "rancher auth login 'r.ex' --url 'https://r.ex'"

	withCA := ErrNotLoggedInFor(servers, map[string]bool{entry: true})
	assert.Equal(t, clierr.CodeNotLoggedIn, withCA.Code)
	assert.Equal(t, "not logged in - no current server; the only server entry, '"+entry+"', is the exec plugin's own login entry, which auth login refuses; "+
		"log in under a server entry of your own with: "+login+
		", adding --cacert with the path of a file that holds the server's CA certificate, which server entry '"+entry+"' carries inline in config.yaml",
		clierr.ExpandRemedies(withCA.Message, ""))
	assert.Empty(t, withCA.Hint, "the login needs a file, so it is no hint")

	without := ErrNotLoggedInFor(servers, map[string]bool{entry: false})
	assert.NotContains(t, without.Message, "adding --cacert")
	assert.Equal(t, login, clierr.ExpandRemedies(without.Hint, ""))

	other := ErrNotLoggedInFor(servers, map[string]bool{"prod": true})
	assert.NotContains(t, other.Message, "adding --cacert", "a saved CA of another entry is not this entry's")
}

// TestNotLoggedInNamesTheSavedCAOfThePluginLoginEntry asserts ErrNotLoggedIn
// reads which entries save a CA from the config.
func TestNotLoggedInNamesTheSavedCAOfThePluginLoginEntry(t *testing.T) {
	t.Parallel()
	c := &Config{Servers: map[string]Server{"exec-login-r.ex": {URL: "https://r.ex", CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"}}}
	ce := c.ErrNotLoggedIn()
	require.Contains(t, ce.Message, "adding --cacert")
	assert.Empty(t, ce.Hint)
}
