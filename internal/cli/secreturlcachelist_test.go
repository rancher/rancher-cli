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

// cacheListOwner is the user the test server signs a stored login in as.
const cacheListOwner = "e2e-admin"

// cacheListSeed writes the entries of the cache list rows into dir, which
// holds nothing yet: a usable entry y at the test server with a stored login,
// and an entry x whose URL holds an @ after the host and reads as the same
// server. With reuse the login is the plugin's own: a usable plugin login
// entry holds it, y holds none, and x is a plugin login entry of the raw-at
// URL.
func cacheListSeed(t *testing.T, dir string, srv string, fill func(string) string, reuse bool) {
	t.Helper()
	if reuse {
		writeSecretStored(t, dir, secretStored{name: "exec-login-o", url: "{url}", stubCA: true, token: true, provenance: clierr.ProvenanceExecLogin}, fill)
	} else {
		writeSecretStored(t, dir, secretStored{name: "y", url: "{url}", stubCA: true, token: true}, fill)
	}
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: map[bool]string{false: "y", true: "exec-login-o"}[reuse], URL: srv}, secretBearer))
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.TokenReuse = reuse
	for n, e := range cfg.Servers {
		e.TokenUserID = cacheListOwner
		cfg.Servers[n] = e
	}
	if reuse {
		cfg.Servers["y"] = config.Server{URL: srv, TokenUserID: cacheListOwner}
		cfg.Servers["exec-login-x"] = config.Server{URL: srv + "/pq@x", Provenance: clierr.ProvenanceExecLogin, TokenUserID: cacheListOwner}
	} else {
		cfg.Servers["x"] = config.Server{URL: srv + "/pq@x", TokenUserID: cacheListOwner}
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// TestAnEntryWithAnAtSignAfterTheHostDoesNotStopTheCacheOfAUsableEntry asserts
// that when an entry whose URL holds an @ after the host reads as the same
// server as a usable entry, the exec plugin's cache still works for the
// usable entry: two calls in a row create one token, and the second is a
// cache hit that sends no request. Records are bound to the entries the
// lookup reads, which leave the unusable entry out. It runs the three kinds
// of record that hold a list: a call with --user-id, a call whose --name
// identifies no entry, and a call whose identified entry falls back to the
// plugin login entries (token reuse on). Not parallel: it sets the process
// environment.
func TestAnEntryWithAnAtSignAfterTheHostDoesNotStopTheCacheOfAUsableEntry(t *testing.T) {
	restoreRawArgv(t)
	for _, tc := range []struct {
		name  string
		argv  []string
		reuse bool
	}{
		{"a call with a user", []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--user-id", cacheListOwner}, false},
		{"a call whose name identifies no entry", []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--name", "prod-cluster"}, false},
		{"a call whose identified entry falls back to the plugin login entries", []string{"auth", "get-token", "--server", "{url}", "--cluster", "c-x", "--name", "y"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			srv := secretServer(t, func(_ http.ResponseWriter, r *http.Request) bool {
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				return false
			}, secretBearer)
			fill := secretFiller(t, srv)
			t.Setenv(config.EnvURL, "")
			t.Setenv(config.EnvToken, "")
			dir := os.Getenv(config.EnvConfigDir)
			cacheListSeed(t, dir, srv.URL, fill, tc.reuse)

			stdout, stderr, code := runWithStdin(t, fillAll(fill, tc.argv), "")
			require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			mu.Lock()
			created := 0
			for _, r := range requests {
				if strings.HasPrefix(r, "POST ") && strings.Contains(r, "/tokens") {
					created++
				}
			}
			requests = nil
			mu.Unlock()
			assert.Equal(t, 1, created, "the first call creates one token")

			stdout, stderr, code = runWithStdin(t, fillAll(fill, tc.argv), "")
			require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			mu.Lock()
			assert.Empty(t, requests, "the second call is a cache hit")
			mu.Unlock()
		})
	}
}

// TestATokenRecordedOnAnEntryThatCannotBeUsedIsKeptWhenNoUsableEntryIsAtTheServer
// asserts that when the entry a cached token is recorded on cannot be used and
// no usable entry is registered at its server, logout --all sends nothing for
// it and lists it as not revoked with the repair of the entry's URL and the
// route that deletes the token where it lives, and never a login of the entry.
// Not parallel: it sets the process environment.
func TestATokenRecordedOnAnEntryThatCannotBeUsedIsKeptWhenNoUsableEntryIsAtTheServer(t *testing.T) {
	restoreRawArgv(t)
	var mu sync.Mutex
	var deletes []string
	srv := secretServer(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes = append(deletes, r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
		}
		return false
	}, secretBearer)
	fill := secretFiller(t, srv)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	dir := os.Getenv(config.EnvConfigDir)
	writeSecretStored(t, dir, secretStored{name: "x", url: "{url}/pq@x", stubCA: true, token: true}, fill)
	seedCacheRecords(t, dir, nil, []cacheRecordSeed{{name: "x", server: srv.URL, ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch02"}}})

	stdout, stderr, code := runWithStdin(t, []string{"-o", "json", "auth", "logout", "--all"}, "")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	mu.Lock()
	for _, d := range deletes {
		assert.NotContains(t, d, "token-cch02", "nothing is sent for the token")
	}
	mu.Unlock()
	var found bool
	for _, n := range notRevokedOf(t, stdout) {
		if ref, _ := n["ref"].(map[string]any); ref["name"] == "token-cch02" {
			found = true
			assert.Empty(t, n["recovery"], "no login is built")
		}
	}
	assert.True(t, found, "the token is listed as not revoked: %s", stdout)
	warnings := withoutRunDirectory(strings.Join(envelopeWarnings(t, stdout), "\n"))
	assert.Contains(t, warnings, "token-cch02")
	assert.Contains(t, warnings, "change the entry's URL with rancher config update-server 'x' --url <url>")
	assert.Contains(t, warnings, "delete token 'token-cch02' --apply --yes")
	assert.NotContains(t, warnings, "auth login 'x'", "the login of an entry that cannot be used is refused")
	assert.NotContains(t, warnings, "revoked by the next login")

	// The record stays on the entry; a second logout --all says the same, and
	// says once that the token stays on the server that issued it.
	for _, run := range []string{stdout, secondLogout(t)} {
		var about []string
		for _, w := range envelopeWarnings(t, run) {
			if strings.Contains(w, "token-cch02") {
				about = append(about, w)
			}
		}
		require.NotEmpty(t, about, "the token is in a warning: %s", run)
		for _, w := range about {
			assert.Equal(t, 1, strings.Count(w, "the token stays on the server that issued it"), "said once: %s", w)
			assert.Equal(t, 1, strings.Count(w, "delete token 'token-cch02' --apply --yes"), "the route is named once: %s", w)
		}
	}
}

// secondLogout runs auth logout --all once more in the process environment of
// the test and returns its JSON result.
func secondLogout(t *testing.T) string {
	t.Helper()
	stdout, stderr, code := runWithStdin(t, []string{"-o", "json", "auth", "logout", "--all"}, "")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	return stdout
}
