package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namespaceListServer responds to the Kubernetes namespace list of cluster
// c-guard with the status kubernetes, to its Steve namespace collection with
// the status steve, and to the Steve provisioning clusters of fleet-default
// with 200, and records the paths of the requests. The Kubernetes body has the names default and kube-system, and the
// Steve bodies team-a, listed in full, and team-b, listed with a few fields.
type namespaceListServer struct {
	srv               *httptest.Server
	kubernetes, steve int
	mu                sync.Mutex
	paths             []string
}

const (
	kubernetesNamespacesPath = "/k8s/clusters/c-guard/api/v1/namespaces"
	steveClusterNamespaces   = "/k8s/clusters/c-guard/v1/namespaces"
	steveProvisioningPath    = "/v1/provisioning.cattle.io.clusters/fleet-default"
)

func newNamespaceListServer(t *testing.T, kubernetes, steve int) *namespaceListServer {
	t.Helper()
	s := &namespaceListServer{kubernetes: kubernetes, steve: steve}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		status, body := http.StatusOK, map[string]any{}
		switch r.URL.Path {
		case kubernetesNamespacesPath:
			status = s.kubernetes
			body = map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "default"}}, map[string]any{"metadata": map[string]any{"name": "kube-system"}}}}
		case steveClusterNamespaces:
			status = s.steve
			body = map[string]any{"revision": "1", "data": []any{
				map[string]any{"id": "team-a", "metadata": map[string]any{"name": "team-a", "labels": map[string]any{"kubernetes.io/metadata.name": "team-a"}}},
				map[string]any{"id": "team-b", "metadata": map[string]any{"name": "team-b", "labels": map[string]any{"field.cattle.io/projectId": "p-1"}}},
			}}
		case steveProvisioningPath:
			body = map[string]any{"revision": "1", "data": []any{map[string]any{"id": "fleet-default/prod", "metadata": map[string]any{"name": "prod", "namespace": "fleet-default"}}}}
		}
		w.WriteHeader(status)
		if status/100 == 2 {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *namespaceListServer) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

// TestNamespaceCompletionReadsSteveOnlyAfterTheKubernetesListIsRefused asserts,
// for the namespace name of `get namespace` and for the value of --namespace,
// that the Kubernetes list, with a response of 200, 401, 403, 404 or 500, gives: an
// administrator (200) one request and the names of that list; a 401, 404 or 500
// one request and no name; a 403 the Steve collection of the same cluster as
// the second request and its names (the name of `get namespace` leaves out
// team-b, which Steve lists with a few fields and `get namespace` refuses),
// and no name when that responds with 403 too; and
// that a management kind that sets the flag reads its Steve collection first and
// only, whatever the Kubernetes API responds with. No site makes a third request.
// Not parallel: it sets the process environment and the stored config.
func TestNamespaceCompletionReadsSteveOnlyAfterTheKubernetesListIsRefused(t *testing.T) {
	sites := map[string]steveReadSite{}
	for _, s := range steveReadSites(t) {
		switch {
		case s.d.Name == "get namespace" && s.site.Kind == SitePositional && s.site.Index == 0 && slices.Contains(s.tail, "--cluster"):
			sites["the name of get namespace"] = s
		case s.d.Name == "get pod" && s.site.FlagName == "namespace" && slices.Contains(s.tail, "--cluster") && !slices.Contains(s.tail, "--namespace"):
			sites["the --namespace of get pod"] = s
		case s.d.Name == "get provisioning-cluster" && s.site.Kind == SitePositional && s.site.Index == 0 && len(s.tail) == 0:
			sites["the name of get provisioning-cluster"] = s
		}
	}
	require.Len(t, sites, 3, "the sites of the table are claimed by a completion source")

	tests := []struct {
		name       string
		kubernetes int
		steve      int
		names      []string
		positional []string
		requested  []string
	}{
		{name: "200", kubernetes: 200, steve: 200, names: []string{"default", "kube-system"}, requested: []string{kubernetesNamespacesPath}},
		{name: "401", kubernetes: 401, steve: 200, requested: []string{kubernetesNamespacesPath}},
		{name: "403", kubernetes: 403, steve: 200, names: []string{"team-a", "team-b"}, positional: []string{"team-a"}, requested: []string{kubernetesNamespacesPath, steveClusterNamespaces}},
		{name: "403 and Steve 403", kubernetes: 403, steve: 403, requested: []string{kubernetesNamespacesPath, steveClusterNamespaces}},
		{name: "404", kubernetes: 404, steve: 200, requested: []string{kubernetesNamespacesPath}},
		{name: "500", kubernetes: 500, steve: 200, requested: []string{kubernetesNamespacesPath}},
	}
	for _, tt := range tests {
		for _, name := range []string{"the name of get namespace", "the --namespace of get pod"} {
			t.Run(name+", Kubernetes "+tt.name, func(t *testing.T) {
				s := sites[name]
				server := newNamespaceListServer(t, tt.kubernetes, tt.steve)
				liveFixture(t, server.srv)
				got, owned := liveSourceCandidates(t.Context(), s.site, s.d, s.lc, s.tail, flagSources, kindSources, positionalKinds)
				require.True(t, owned)
				assert.Equal(t, tt.requested, server.requested())
				assert.LessOrEqual(t, len(server.requested()), 2)
				want := tt.names
				if tt.positional != nil && name == "the name of get namespace" {
					want = tt.positional
				}
				assert.ElementsMatch(t, want, got)
			})
		}
	}

	for _, status := range []int{200, 401, 403, 404, 500} {
		t.Run("the name of get provisioning-cluster, Kubernetes "+strconv.Itoa(status), func(t *testing.T) {
			s := sites["the name of get provisioning-cluster"]
			server := newNamespaceListServer(t, status, status)
			liveFixture(t, server.srv)
			got, owned := liveSourceCandidates(t.Context(), s.site, s.d, s.lc, s.tail, flagSources, kindSources, positionalKinds)
			require.True(t, owned)
			assert.Equal(t, []string{steveProvisioningPath}, server.requested())
			assert.Equal(t, []string{"prod"}, got)
		})
	}
}
