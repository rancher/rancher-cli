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

// repairRequest is one request the stub saw: its method, path and
// Authorization header.
type repairRequest struct{ Method, Path, Auth string }

// repairFixture is a config directory and a stub server that records every
// request it receives, and every DELETE apart.
type repairFixture struct {
	srvURL   string
	dir      string
	fill     func(string) string
	mu       *sync.Mutex
	deletes  *[]string
	requests *[]repairRequest
}

// mark is the number of requests the stub has seen; requestsSince returns the
// ones after it.
func (f repairFixture) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(*f.requests)
}

func (f repairFixture) requestsSince(mark int) []repairRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]repairRequest(nil), (*f.requests)[mark:]...)
}

func newRepairFixture(t *testing.T) repairFixture {
	t.Helper()
	restoreRawArgv(t)
	var mu sync.Mutex
	var deletes []string
	var requests []repairRequest
	srv := secretServer(t, func(_ http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		requests = append(requests, repairRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		if r.Method == http.MethodDelete {
			deletes = append(deletes, r.URL.Path+" "+r.Header.Get("Authorization"))
		}
		mu.Unlock()
		return false
	}, secretBearer)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	return repairFixture{srvURL: srv.URL, dir: os.Getenv(config.EnvConfigDir), fill: secretFiller(t, srv), mu: &mu, deletes: &deletes, requests: &requests}
}

// run runs the command with the stub's placeholders filled in and requires
// it to succeed.
func (f repairFixture) run(t *testing.T, stdin string, argv ...string) string {
	t.Helper()
	stdout, stderr, code := runWithStdin(t, fillAll(f.fill, argv), stdin)
	require.Zero(t, code, "%v\nstdout: %s\nstderr: %s", argv, stdout, stderr)
	return stdout
}

// TestAfterTheRepairTheTokenOfAnEntryIsListedWithTheRouteThatDeletesIt asserts
// that after config update-server of an entry whose URL cannot be used, the
// token recorded on it is still listed by the repair, and by the login that
// follows, with the route that deletes it where it lives: the record names a
// server that cannot be used, so no login retires it.
func TestAfterTheRepairTheTokenOfAnEntryIsListedWithTheRouteThatDeletesIt(t *testing.T) {
	f := newRepairFixture(t)
	writeSecretStored(t, f.dir, secretStored{name: "x", url: "{url}/pq@x", stubCA: true, token: true}, f.fill)
	const route = "rancher delete token 'token-abc12' --apply --yes"

	updated := f.run(t, "", "-o", "json", "config", "update-server", "x", "--url", "{url}", "--cacert", "{pem}")
	assert.Contains(t, withoutRunDirectory(strings.Join(envelopeWarnings(t, updated), "\n")), route, "the repair says how to delete the token")

	stdout, stderr, code := runWithStdin(t, fillAll(f.fill, []string{"auth", "login", "x", "--token-stdin", "--no-verify"}), "id:y")
	require.Zero(t, code, "stdout: %s stderr: %s", stdout, stderr)
	login := withoutRunDirectory(stdout + stderr)
	assert.Contains(t, login, "the superseded token ext/token-abc12 was not retired: the server recorded for token 'token-abc12' cannot be used: ")
	assert.Contains(t, login, route, "the login says how to delete the token")
	assert.NotContains(t, login, "auth login 'x'", "no login retires it")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, *f.deletes, "nothing is sent")
}

// TestTheWarningOfATokenWithNoServerDoesNotSayItStaysOnTheServer asserts that
// a token of a cache record that names no server is listed with the route
// that deletes it, and not with the text for a token of an entry whose URL
// cannot be used: that text is for the entries a login refuses.
func TestTheWarningOfATokenWithNoServerDoesNotSayItStaysOnTheServer(t *testing.T) {
	f := newRepairFixture(t)
	writeSecretStored(t, f.dir, secretStored{name: "x", url: "{url}", stubCA: true, token: true}, f.fill)
	seedCacheRecords(t, f.dir, nil, []cacheRecordSeed{{name: "x", server: "", ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-nos01"}}})
	stdout := f.run(t, "", "-o", "json", "auth", "logout", "--all")
	var about []string
	for _, w := range envelopeWarnings(t, stdout) {
		if strings.Contains(w, "token-nos01") {
			about = append(about, w)
		}
	}
	require.NotEmpty(t, about, "the token is in a warning: %s", stdout)
	for _, w := range about {
		assert.Contains(t, w, "no server is recorded for token 'token-nos01'")
		assert.Contains(t, w, "delete token 'token-nos01' --apply --yes")
		assert.NotContains(t, w, "the token stays on the server that issued it", w)
	}
}
