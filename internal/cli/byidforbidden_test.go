package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/secret"
)

const (
	byIDK8sProjects = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-a/projects/"
	byIDClusters    = "/v1/management.cattle.io.clusters"
	byIDProjects    = "/v1/management.cattle.io.projects"
)

// byIDServer is a test server that behaves as Rancher does for a user who can
// read clusters c-a and c-b and projects p-1 and p-2 on c-a. It refuses (HTTP
// 403) a get of c-hidden or p-hidden by its id, which that user cannot read,
// and responds with 404 to a get of c-gone or p-gone. It records every request
// path.
type byIDServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []string
}

func newByIDServer(t *testing.T) *byIDServer {
	t.Helper()
	s := &byIDServer{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, r.URL.Path)
		s.mu.Unlock()
		switch p := r.URL.Path; p {
		case byIDClusters:
			grWriteJSON(w, steveCollectionOf(steveClusterObject("c-a", "Alpha"), steveClusterObject("c-b", "Beta")))
		case byIDClusters + "/c-hidden":
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "Method GET not supported")
		case byIDProjects + "/c-a":
			grWriteJSON(w, steveCollectionOf(steveProjectObject("c-a", "p-2", "Two"), steveProjectObject("c-a", "p-1", "One")))
		case byIDK8sProjects + "p-hidden":
			grWriteStatus(w, http.StatusForbidden, "Forbidden", "projects.management.cattle.io \"p-hidden\" is forbidden")
		default:
			grWriteStatus(w, http.StatusNotFound, "NotFound", "not found")
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *byIDServer) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *byIDServer) factory(t *testing.T) *clientFactory {
	t.Helper()
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := grConfigDir(t, s.srv)
	return newClientFactory(dir, config.NewFileStore(dir))
}

// flagInput is a runtime.Input that has one string flag.
type flagInput struct{ cluster string }

func (i flagInput) String(name string) string {
	if name == engine.FlagCluster {
		return i.cluster
	}
	return ""
}
func (flagInput) Bool(string) bool              { return false }
func (flagInput) Strings(string) []string       { return nil }
func (flagInput) IsSet(string) bool             { return false }
func (flagInput) Secrets(string) []secret.Value { return nil }
func (flagInput) Args() []string                { return nil }

func requireByIDRBAC(t *testing.T, err error, message, hint string, candidates []clierr.Candidate) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, 6, ce.ExitCode())
	assert.Equal(t, message, ce.Message)
	assert.Equal(t, hint, ce.Hint)
	assert.False(t, ce.Retryable)
	if len(candidates) == 0 {
		assert.Empty(t, ce.Candidates)
	} else {
		assert.Equal(t, candidates, ce.Candidates)
	}
	assert.Zero(t, ce.CandidatesTotal)
}

// TestResolveClusterRefusedByIDIsRBAC asserts that a cluster value missing from
// the caller's list, when Rancher refuses (HTTP 403) the get of the cluster by
// its id, gives an RBAC error with the access clusters hint and the clusters
// the caller can read, at ResolveCluster and ResolveClusterArgument alike,
// after one get by id.
func TestResolveClusterRefusedByIDIsRBAC(t *testing.T) {
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod"), steveClusterItem("c-def", "dev")},
		byID: map[string]int{"c-hidden": http.StatusForbidden}}
	candidates := []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "dev"}}
	message := "you cannot read cluster 'c-hidden' (GET /v1/management.cattle.io.clusters/c-hidden answered 403)"
	for name, resolve := range map[string]func(*clientFactory) (string, error){
		"ResolveCluster": func(f *clientFactory) (string, error) {
			return f.ResolveCluster(t.Context(), "c-hidden")
		},
		"ResolveClusterArgument": func(f *clientFactory) (string, error) {
			return f.ResolveClusterArgument(t.Context(), "c-hidden")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root.mu.Lock()
			root.paths = nil
			root.mu.Unlock()
			_, err := resolve(resolveFactory(t, root))
			requireByIDRBAC(t, err, message, clierr.Rancher+" access clusters", candidates)
			assert.Equal(t, []string{"/v1/management.cattle.io.clusters", "/v1/management.cattle.io.clusters/c-hidden"}, root.requests())
		})
	}
}

// TestResolveClusterRefusedByIDForAUserWhoCanReadNoCluster asserts that when
// Rancher refuses (HTTP 403) the get by its id of a cluster for a caller who
// can read no cluster, the RBAC error says so and who grants the roles, and has
// no hint and no candidates. The test above asserts the hint and the candidates
// for a caller who can read a cluster.
func TestResolveClusterRefusedByIDForAUserWhoCanReadNoCluster(t *testing.T) {
	root := &steveClustersRoot{byID: map[string]int{"c-hidden": http.StatusForbidden}}
	_, err := resolveFactory(t, root).ResolveCluster(t.Context(), "c-hidden")
	requireByIDRBAC(t, err,
		"you cannot read cluster 'c-hidden' (GET /v1/management.cattle.io.clusters/c-hidden answered 403); you can read no cluster on this server; a Rancher administrator grants cluster and project roles",
		"", nil)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters", "/v1/management.cattle.io.clusters/c-hidden"}, root.requests())
}

// TestResolveClusterNotAnIDIsNotFoundWithNoRequestByID asserts a value that
// cannot be a cluster id is NOT_FOUND and is never sent as a get by id.
func TestResolveClusterNotAnIDIsNotFoundWithNoRequestByID(t *testing.T) {
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod")},
		byID: map[string]int{"My Prod": http.StatusForbidden}}
	_, err := resolveFactory(t, root).ResolveCluster(t.Context(), "My Prod")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters"}, root.requests())
}

// TestResolveProjectRefusedByIDIsRBAC asserts that the --project lookup of a
// value missing from the caller's list, when Rancher refuses (HTTP 403) the
// get of the project by its id, gives an RBAC error with the hint that lists
// the projects on the cluster and the projects the caller can read. It also
// asserts that a 404 gives the NOT_FOUND error and that a value that cannot be
// a project id is never sent as a get by id.
func TestResolveProjectRefusedByIDIsRBAC(t *testing.T) {
	s := newByIDServer(t)
	f := s.factory(t)
	r := projectResolver{f}
	in := flagInput{cluster: "c-a"}
	candidates := []clierr.Candidate{{ID: "p-2", Name: "Two"}, {ID: "p-1", Name: "One"}}

	_, _, err := r.ResolveProject(t.Context(), in, "p-hidden")
	requireByIDRBAC(t, err, "you cannot read project 'p-hidden' on cluster 'c-a'",
		clierr.Rancher+" access projects --cluster 'c-a'", candidates)
	got := s.requested()
	assert.Equal(t, 1, countPath(got, byIDK8sProjects+"p-hidden"), "one request by id: %v", got)

	_, _, err = r.ResolveProject(t.Context(), in, "p-gone")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: 'p-gone'; it is not among the projects you can read on cluster 'c-a'; pass a project id or display name", ce.Message)
	assert.Equal(t, candidates, ce.Candidates)

	before := len(s.requested())
	_, _, err = r.ResolveProject(t.Context(), in, "My Web")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	for _, p := range s.requested()[before:] {
		assert.NotContains(t, p, byIDK8sProjects, "a value that is not an id is not sent as a get by id")
	}
}

func countPath(paths []string, want string) int {
	n := 0
	for _, p := range paths {
		if p == want {
			n++
		}
	}
	return n
}

// TestCommandsNameARefusedClusterOrProjectByIDAsRBAC asserts whole command
// lines that look up a cluster or project end with exit 6 and the RBAC error
// when Rancher refuses (HTTP 403) the get of the cluster or project by its id.
func TestCommandsNameARefusedClusterOrProjectByIDAsRBAC(t *testing.T) {
	s := newByIDServer(t)
	for _, tc := range []struct {
		args    []string
		message string
		hint    string
	}{
		{[]string{"inspect", "cluster", "c-hidden", "-o", "json"},
			"you cannot read cluster 'c-hidden' (GET /v1/management.cattle.io.clusters/c-hidden answered 403)", "rancher access clusters"},
		{[]string{"inspect", "project", "p-hidden", "--cluster", "c-a", "-o", "json"},
			"you cannot read project 'p-hidden' on cluster 'c-a'", "rancher access projects --cluster 'c-a'"},
	} {
		restoreRawArgv(t)
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
		dir := grConfigDir(t, s.srv)
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
		res := run(t.Context(), root, append([]string{"rancher", "--config", dir}, tc.args...))
		require.NotNil(t, res.err, "%v", tc.args)
		assert.Equal(t, 6, res.code, "%v", tc.args)
		ce := renderedError(t, "json", errOut.String())
		assert.Equal(t, clierr.CodeRBAC, ce.Code, "%v", tc.args)
		assert.Equal(t, tc.message, ce.Message, "%v", tc.args)
		assert.Equal(t, tc.hint, withoutConfigDir(ce.Hint, dir), "%v", tc.args)
	}
}
