package token_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

const gatedSeed = "seed:secret"

// gatedCLIFacade refuses every bearer the local cluster's management API
// outside Rancher's allow list, as Rancher refuses a user with no access
// to cluster local, and serves c-abc (displayed as prod) and c-def (whose
// display name is c-abc's id) on steve to the bearers it knows. paths
// records every request path.
type gatedCLIFacade struct {
	f     *testrancher.Facade
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newGatedCLIFacade(t *testing.T) *gatedCLIFacade {
	t.Helper()
	g := &gatedCLIFacade{}
	g.f = testrancher.New(testrancher.Options{
		SeedToken:           gatedSeed,
		LocalManagementGate: func(string) bool { return false },
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return []testrancher.ClusterFixture{{ID: "c-abc", DisplayName: "prod"}, {ID: "c-def", DisplayName: "c-abc"}}, b == gatedSeed || g.f.ValidBearer(b)
		},
		RequestHook: func(r *http.Request) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.paths = append(g.paths, r.URL.Path)
		},
	})
	g.srv = httptest.NewServer(g.f)
	t.Cleanup(g.srv.Close)
	return g
}

// localManagementReads are the requests that reached the local cluster's
// management clusters API.
func (g *gatedCLIFacade) localManagementReads() []string {
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

func (g *gatedCLIFacade) clusterCreates() []string {
	var out []string
	for _, c := range g.f.CreateRequests() {
		if c.ClusterName != "" {
			out = append(out, c.ClusterName)
		}
	}
	return out
}

// storedLoginCLIDir writes a directory with one server entry, prod, at
// url, holding the seed login in the file store.
func storedLoginCLIDir(t *testing.T, url string) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: url}, gatedSeed))
	return dir
}

// TestStoredLoginMissCreatesTheTokenForTheResolvedClusterUnderTheGate
// asserts a stored login's cache miss resolves --cluster through steve when
// the local cluster's management API refuses the user: a display name
// creates the token for its id, an id that is also another cluster's
// display name creates it for the id with the warning on stderr, and
// stdout holds only the ExecCredential.
func TestStoredLoginMissCreatesTheTokenForTheResolvedClusterUnderTheGate(t *testing.T) {
	for _, tc := range []struct{ value, warning string }{
		{"prod", ""},
		{"c-abc", "--cluster 'c-abc' is the id of cluster 'c-abc' and the display name of cluster 'c-def'; the command uses the one with that id. To use the other one, pass --cluster 'c-def'"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			g := newGatedCLIFacade(t)
			dir := storedLoginCLIDir(t, g.srv.URL)
			stdout, stderr, err := runCLI(t, "", "--config", dir, "auth", "get-token", "--server", g.srv.URL, "--cluster", tc.value, "--name", "kube")
			require.NoError(t, err, stderr)
			var cred map[string]any
			require.NoError(t, json.Unmarshal([]byte(stdout), &cred), "stdout is one JSON document: %q", stdout)
			assert.Equal(t, "ExecCredential", cred["kind"])
			assert.Equal(t, []string{"c-abc"}, g.clusterCreates())
			assert.Empty(t, g.localManagementReads())
			if tc.warning == "" {
				assert.NotContains(t, stderr, "is the id of")
			} else {
				assert.Contains(t, stderr, tc.warning)
				assert.NotContains(t, stdout, tc.warning)
			}
		})
	}
}

// storedLoginNoMatchError runs the exec plugin with a stored login and a
// cluster value, and returns the error it renders, decoded.
func storedLoginNoMatchError(t *testing.T, g *gatedCLIFacade, value string) (code, message, hint string, candidates []clierr.Candidate) {
	t.Helper()
	dir := storedLoginCLIDir(t, g.srv.URL)
	_, stderr, err := runCLI(t, "", "--config", dir, "auth", "get-token", "--server", g.srv.URL, "--cluster", value, "-o", "json")
	require.Error(t, err)
	var env struct {
		Data struct {
			Error struct {
				Code, Message, Hint string
				Candidates          []clierr.Candidate
			} `json:"error"`
		} `json:"data"`
	}
	i := strings.Index(stderr, "{")
	require.GreaterOrEqual(t, i, 0, stderr)
	require.NoError(t, json.Unmarshal([]byte(stderr[i:]), &env), stderr)
	e := env.Data.Error
	return e.Code, e.Message, e.Hint, e.Candidates
}

// TestStoredLoginNoMatchRefusedByIDIsRBACListingTheReadableClusters asserts
// that for a cluster the user cannot read, which the test server (gatedCLIFacade,
// the caller-scope gate in front of it) refuses (HTTP 403) when the exec plugin
// gets it by its id, the exec plugin returns an RBAC error. The error names the clusters the
// user can read and no command, carries them as candidates and creates nothing.
func TestStoredLoginNoMatchRefusedByIDIsRBACListingTheReadableClusters(t *testing.T) {
	g := newGatedCLIFacade(t)
	code, message, hint, candidates := storedLoginNoMatchError(t, g, "web")
	assert.Equal(t, clierr.CodeRBAC, code)
	assert.Equal(t, "you cannot read cluster 'web' (GET /v1/management.cattle.io.clusters/web answered 403); the clusters you can read: 'c-abc' (prod), 'c-def' (c-abc)", message)
	assert.Empty(t, hint)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "c-abc"}}, candidates)
	assert.Empty(t, g.clusterCreates())
}

// TestStoredLoginNoMatchNotAnIDIsNotFoundListingTheReadableClusters asserts
// that for a value that cannot be a cluster id the exec plugin returns a
// NOT_FOUND error that names the clusters the user can read and no command,
// and never gets the value by its id.
func TestStoredLoginNoMatchNotAnIDIsNotFoundListingTheReadableClusters(t *testing.T) {
	g := newGatedCLIFacade(t)
	code, message, hint, candidates := storedLoginNoMatchError(t, g, "my web")
	assert.Equal(t, clierr.CodeNotFound, code)
	assert.Equal(t, "cluster not found: 'my web'; it is not among the clusters you can read; pass a cluster id or display name; the clusters you can read: 'c-abc' (prod), 'c-def' (c-abc)", message)
	assert.Empty(t, hint)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "c-abc"}}, candidates)
	assert.Empty(t, g.clusterCreates())
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.paths {
		assert.False(t, strings.HasPrefix(p, testrancher.ClustersCollectionPath+"/"), "a value that is not an id is not sent as a get by id: %s", p)
	}
}
