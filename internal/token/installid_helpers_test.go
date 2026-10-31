package token

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

const (
	installIDRoute = "/v1/management.cattle.io.settings/install-uuid"
	testInstallID  = "5e2f6c0a-8a3b-4c1d-9e47-0b6f1d2a3c4e"
)

// installIDServer answers the install id setting with testInstallID and
// keeps the bearer of each request. When it knows the facade it also keeps
// the kind of token the bearer was at the time of the request.
type installIDServer struct {
	mu       sync.Mutex
	f        *testrancher.Facade
	requests []installIDRequest
}

type installIDRequest struct {
	bearer string
	kind   string
}

func (s *installIDServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != installIDRoute {
		http.NotFound(w, r)
		return
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	kind := "unknown"
	if s.f != nil {
		switch {
		case slices.Contains(s.f.BearersOfKind("v3"), bearer):
			kind = "v3"
		case slices.Contains(s.f.BearersOfKind("ext"), bearer):
			kind = "ext"
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, installIDRequest{bearer: bearer, kind: kind})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"value":"` + testInstallID + `"}`))
}

func (s *installIDServer) seen() []installIDRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func (s *installIDServer) bearers() []string {
	var out []string
	for _, r := range s.seen() {
		out = append(out, r.bearer)
	}
	return out
}

// clearRecords empties the process-wide records now and when the test ends.
// A test that reads or sets a record never calls t.Parallel().
func clearRecords(t *testing.T) {
	t.Helper()
	httpx.ClearRecords()
	t.Cleanup(httpx.ClearRecords)
}

// registeredFetcher is the install id fetcher the call registered; the test
// fails when there is none.
func registeredFetcher(t *testing.T) httpx.InstallIDFetcher {
	t.Helper()
	f, ok := httpx.RegisteredInstallIDFetcher()
	require.True(t, ok, "the call registered an install id fetcher")
	return f
}

// fetchRegistered sends the registered request and asserts it got the
// install id.
func fetchRegistered(t *testing.T, f httpx.InstallIDFetcher) {
	t.Helper()
	value, status, err := f.Fetch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, testInstallID, value)
}

// collectedNotices is a prompter that keeps every notice it is given.
type collectedNotices struct {
	runtime.Prompter
	mu   *sync.Mutex
	list *[]string
}

func newCollectedNotices(p runtime.Prompter) collectedNotices {
	return collectedNotices{Prompter: p, mu: &sync.Mutex{}, list: &[]string{}}
}

func (c collectedNotices) Notice(msg string) {
	c.mu.Lock()
	*c.list = append(*c.list, msg)
	c.mu.Unlock()
	c.Prompter.Notice(msg)
}

func (c collectedNotices) notices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(*c.list)
}

// seedStoredLogin stores a login credential under an entry called name, as
// auth login does, and returns the token.
func seedStoredLogin(t *testing.T, x *pluginFixture, name string) string {
	t.Helper()
	parent, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
	require.NoError(t, err)
	seedEntry(t, x.dir, x.store, name, config.Server{URL: x.url, TokenRef: parent.Ref, TokenUserID: parent.UserID}, parent.Token, false)
	return parent.Token
}

// signInProbe is a sign-in request function that records what the server
// had answered when it was called and what its install id request got.
type signInProbe struct {
	mu    sync.Mutex
	calls []signInProbeCall
}

type signInProbeCall struct {
	origin   string
	sequence []string
	value    string
	status   int
	err      error
}

// registerSignInProbe registers a function that sends the install id request
// it is given at once, and keeps sequence() as it was at the call. The
// records are cleared first and when the test ends.
func registerSignInProbe(t *testing.T, sequence func() []string) *signInProbe {
	t.Helper()
	clearRecords(t)
	p := &signInProbe{}
	httpx.RegisterSignInRequest(func(ctx context.Context, origin string, fetch httpx.InstallIDFunc) {
		call := signInProbeCall{origin: origin, sequence: sequence()}
		call.value, call.status, call.err = fetch(ctx)
		p.mu.Lock()
		p.calls = append(p.calls, call)
		p.mu.Unlock()
	})
	return p
}

func (p *signInProbe) got() []signInProbeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// reset forgets the requests logged so far.
func (l *requestLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = nil
}
