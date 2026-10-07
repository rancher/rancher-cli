package token

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// gatedClusters is what every bearer the facade knows reads on the steve
// clusters collection in these tests: c-abc displayed as prod, and c-def
// whose display name is c-abc's id.
var gatedClusters = []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}, {ID: "c-def", DisplayName: "c-abc"}}

const seedLogin = "seed:secret"

// gatedFacade is a facade that refuses every bearer the local cluster's
// management API outside Rancher's allow list, as Rancher refuses a user
// with no access to cluster local, and serves gatedClusters on steve to
// the bearers it knows. paths records every request path.
type gatedFacade struct {
	f     *testrancher.Facade
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newGatedFacade(t *testing.T, wrap func(http.Handler) http.Handler) *gatedFacade {
	t.Helper()
	g := &gatedFacade{}
	g.f = testrancher.New(testrancher.Options{
		SeedToken:           seedLogin,
		LocalManagementGate: func(string) bool { return false },
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return gatedClusters, b == seedLogin || g.f.ValidBearer(b)
		},
		RequestHook: func(r *http.Request) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.paths = append(g.paths, r.URL.Path)
		},
	})
	var h http.Handler = g.f
	if wrap != nil {
		h = wrap(g.f)
	}
	g.srv = httptest.NewServer(h)
	t.Cleanup(g.srv.Close)
	return g
}

// localManagementReads are the requests that reached the local cluster's
// management clusters API.
func (g *gatedFacade) localManagementReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, p := range g.paths {
		if strings.HasPrefix(p, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters") {
			out = append(out, p)
		}
	}
	return out
}

func (g *gatedFacade) clusterCreates() []string {
	var out []string
	for _, c := range g.f.CreateRequests() {
		if c.ClusterName != "" {
			out = append(out, c.ClusterName)
		}
	}
	return out
}

// storedLoginDir writes a directory with one server entry, prod, at url,
// holding the seed login in the file store.
func storedLoginDir(t *testing.T, url string) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: url}, seedLogin))
	return dir
}

// TestStoredLoginSteveRejections asserts a 401 from the steve read takes
// the stored login's sign-in rule (the rejection notice, then the
// interactive fallback, which a call without a terminal cannot take), and
// a 403 is the collection refusal's RBAC error, not "cannot create
// tokens".
func TestStoredLoginSteveRejections(t *testing.T) {
	keyring.MockInit()
	t.Run("401", func(t *testing.T) {
		g := newGatedFacade(t, nil)
		dir := storedLoginDir(t, g.srv.URL)
		require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: g.srv.URL}, "unknown:secret"))
		var notices bytes.Buffer
		cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": g.srv.URL, "cluster": "prod"}}, ui.New(&notices, strings.NewReader(""), true, false))
		require.Error(t, err)
		assert.Contains(t, notices.String(), `the stored login credential for "prod" was rejected`)
		assert.Contains(t, notices.String(), "falling back to interactive login")
		assert.Empty(t, g.clusterCreates())
		assert.Empty(t, g.localManagementReads())
	})
	t.Run("403", func(t *testing.T) {
		g := newGatedFacade(t, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == testrancher.ClustersCollectionPath {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		dir := storedLoginDir(t, g.srv.URL)
		cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": g.srv.URL, "cluster": "prod"}}, ui.Discard())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.True(t, strings.HasPrefix(ce.Message, "the server refused GET /v1/management.cattle.io.clusters (403)"), ce.Message)
		assert.NotContains(t, ce.Message, "cannot create tokens")
		assert.Empty(t, g.clusterCreates())
		assert.Empty(t, g.localManagementReads())
	})
}

// TestPluginCreatedLoginCreatesTheTokenForTheResolvedClusterUnderTheGate
// asserts the interactive path: the plugin-created login resolves
// --cluster through steve with the credential it just created, creates the
// cluster token for the id, and prints the warning on stderr.
func TestPluginCreatedLoginCreatesTheTokenForTheResolvedClusterUnderTheGate(t *testing.T) {
	var g *gatedFacade
	x := newPluginFixture(t, true, testrancher.Options{
		LocalManagementGate: func(string) bool { return false },
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return gatedClusters, g != nil && g.f.ValidBearer(b)
		},
	})
	g = &gatedFacade{f: x.f}
	var notices bytes.Buffer
	res, err := x.getToken(t, "c-abc", "n", ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	assert.Equal(t, "ExecCredential", res.Kind)
	assert.Equal(t, []string{"c-abc"}, g.clusterCreates())
	assert.Contains(t, notices.String(), "--cluster 'c-abc' is the id of cluster 'c-abc'; cluster 'c-def' also has the display name 'c-abc'")
	assert.Empty(t, res.Warnings)
}
