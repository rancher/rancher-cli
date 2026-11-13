package cli

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pluginRow is one call of the exec plugin against an entry that cannot be
// used, and what it must produce.
type pluginRow struct {
	name string
	argv []string
	// url is the stored URL of the entry x, with {url} for the test server's.
	url string
	// reuse turns token reuse on and makes the raw-at entry the plugin login
	// entry exec-login-o instead of x.
	reuse bool
	// usable registers a usable entry y, with no stored login, at the server.
	usable bool
	// signsIn is set for a call the refusal does not apply to: a usable entry
	// is registered at the server, so the call goes on to its sign-in at its
	// own URL, and the raw-at entry's login is neither read nor sent.
	signsIn bool
	// opening is the start of the refusal's text.
	opening string
	// vouched is set when a usable entry is registered at the server: the
	// refusal shows the server as typed and adds the route that uses it.
	vouched bool
	// cached seeds the cache record the call hits.
	cached bool
	// reason is the part of the entry's refusal the error holds.
	reason string
	// misread is set for a call that names a host no test server answers, read
	// from a URL with an @ after the host; its refusal must hold no part of it.
	misread bool
}

// TestThePluginSendsNothingForAnEntryThatCannotBeUsed asserts that for an
// entry whose URL cannot be used (an @ after the host, whose host is the one
// the call's --server names, the form an earlier build wrote into a
// kubeconfig; and a user name and password), every lookup of the exec plugin
// refuses the call with the entry's refusal and repair, and no request at all
// reaches the server: the entry the call's --name identifies, with a cache
// miss and with a cache hit, the entries that record the call's --user-id, the
// ranked entries of the URL, and the plugin login entry that follows an
// identified entry with no login. The refusal shows the call's server as
// unusable when it is the host read from the URL. Not parallel: it sets the
// process environment.
func TestThePluginSendsNothingForAnEntryThatCannotBeUsed(t *testing.T) {
	restoreRawArgv(t)
	const rawAt = "it holds an @ after the host, which usually means a password with a /, ? or # in it"
	get := func(extra ...string) []string {
		return append([]string{"auth", "get-token", "--server", "{url}", "--cluster", "c-2"}, extra...)
	}
	misreadGet := func(extra ...string) []string {
		return append([]string{"auth", "get-token", "--server", "https://admin:1234", "--cluster", "c-2"}, extra...)
	}
	for _, tc := range []pluginRow{
		{name: "the identified entry", argv: get("--name", "x"), url: "{url}/pq@x", reason: rawAt, opening: "the call names server entry 'x' and runs for <unusable URL>;"},
		{name: "the identified entry, a usable entry at the server", argv: get("--name", "x"), url: "{url}/pq@x", usable: true, vouched: true, reason: rawAt,
			opening: "the call names server entry 'x' and runs for {url};"},
		{name: "the entries that record the user", argv: get("--user-id", "u-abc12"), url: "{url}/pq@x", reason: rawAt, opening: "the call runs for <unusable URL>, where server entry 'x' is registered;"},
		{name: "the ranked entries of the URL", argv: get(), url: "{url}/pq@x", reason: rawAt, opening: "the call runs for <unusable URL>, where server entry 'x' is registered;"},
		{name: "the plugin login entry after an identified entry with no login", argv: get("--name", "y"), url: "{url}/pq@x", reuse: true, usable: true, signsIn: true},
		{name: "the same with a user", argv: get("--name", "y", "--user-id", "u-abc12"), url: "{url}/pq@x", reuse: true, usable: true, signsIn: true},
		{name: "an identified entry with no login, no token reuse", argv: get("--name", "y"), url: "{url}/pq@x", usable: true, signsIn: true},
		{name: "the ranked entries when a usable entry has no login", argv: get(), url: "{url}/pq@x", usable: true, signsIn: true},
		{name: "the identified entry, a cache hit", argv: get("--cluster", "c-1", "--name", "x"), url: "{url}/pq@x", cached: true, reason: rawAt},
		{name: "a user name and password in the URL", argv: get("--name", "x"), url: "https://u:pwzq7@{host}", reason: "it holds a user name or password"},
		{name: "the identified entry, the host read from the URL", argv: misreadGet("--name", "x"), url: canaryURLMisread, reason: rawAt, misread: true},
		{name: "the entries that record the user, the host read from the URL", argv: misreadGet("--user-id", "u-abc12"), url: canaryURLMisread, reason: rawAt, misread: true},
		{name: "the ranked entries, the host read from the URL", argv: misreadGet(), url: canaryURLMisread, reason: rawAt, misread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bearers []string
			srv := secretServer(t, func(_ http.ResponseWriter, r *http.Request) bool {
				mu.Lock()
				defer mu.Unlock()
				bearers = append(bearers, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
				return false
			}, secretBearer)
			fill := secretFiller(t, srv)
			t.Setenv(config.EnvURL, "")
			t.Setenv(config.EnvToken, "")
			dir := os.Getenv(config.EnvConfigDir)
			name, prov := "x", clierr.Provenance("")
			if tc.reuse {
				name, prov = "exec-login-o", clierr.ProvenanceExecLogin
			}
			writeSecretStored(t, dir, secretStored{name: name, url: tc.url, stubCA: true, token: true, provenance: prov}, fill)
			if tc.reuse || tc.usable {
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				cfg.TokenReuse = tc.reuse
				if tc.usable {
					cfg.Servers["y"] = config.Server{URL: srv.URL, TokenUserID: "u-abc12"}
				}
				cfg.SetPath(config.FilePath(dir))
				require.NoError(t, cfg.Write())
			}
			if tc.cached {
				seedCacheRecords(t, dir, nil, []cacheRecordSeed{{name: "x", server: srv.URL}})
			}
			if tc.cached {
				before, _, _ := runWithStdin(t, []string{"-o", "json", "auth", "list-tokens"}, "")
				rows, _ := dataOf(t, before).([]any)
				require.Len(t, rows, 1, "the cache record is there before the call: %s", before)
			}
			stdout, stderr, code := runWithStdin(t, fillAll(fill, tc.argv), "")
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			if tc.signsIn {
				mu.Lock()
				for _, b := range bearers {
					assert.NotContains(t, b, "ext/token-abc12:secret", "the login of the raw-at entry is not sent")
				}
				assert.NotEmpty(t, bearers, "the call goes on to the sign-in of the usable entry")
				mu.Unlock()
				assert.NotContains(t, stderr, "cannot be used", "stderr: %s", stderr)
				assert.NotContains(t, stderr, "pq@x")
				return
			}
			mu.Lock()
			assert.Empty(t, bearers, "no request reaches the server")
			mu.Unlock()
			assert.NotContains(t, stdout, "own-credential", "no cached credential is served")
			ce := renderedError(t, "", stderr[strings.Index(stderr, "error ("):])
			assert.Equal(t, string(clierr.CodeConflict), ce.Code, "stderr: %s", stderr)
			assert.Contains(t, ce.Message, "cannot be used: "+tc.reason)
			assert.Contains(t, ce.Message, "the server URL in server entry '"+map[bool]string{false: "x", true: "exec-login-o"}[tc.reuse]+"'")
			if tc.opening != "" {
				assert.True(t, strings.HasPrefix(ce.Message, strings.ReplaceAll(tc.opening, "{url}", srv.URL)), "message: %s", ce.Message)
			}
			if tc.vouched {
				assert.Contains(t, ce.Message, "to use "+srv.URL+", regenerate it from an entry registered there")
			} else {
				assert.NotContains(t, ce.Message, "to use ")
			}
			assert.NotContains(t, ce.Message, "pwzq7")
			assert.NotContains(t, ce.Message, "pq@x")
			if tc.misread {
				assert.NotRegexp(t, misreadPattern, withoutRunDirectory(stdout+stderr))
				assert.Contains(t, ce.Message, "runs for <unusable URL>")
			}
			if tc.cached {
				stdout, _, code = runWithStdin(t, []string{"-o", "json", "auth", "list-tokens"}, "")
				require.Zero(t, code)
				rows, _ := dataOf(t, stdout).([]any)
				assert.Len(t, rows, 1, "a record the call refuses to serve is kept")
			}
		})
	}
}
