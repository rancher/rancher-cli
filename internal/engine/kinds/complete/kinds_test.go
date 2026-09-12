package complete

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listServer answers every path with a two-item list and records the paths
// requested.
type listServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newListServer(t *testing.T) *listServer {
	t.Helper()
	ls := &listServer{}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.paths = append(ls.paths, r.URL.RequestURI())
		ls.mu.Unlock()
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"b"}},{"metadata":{"name":"a"}}]}`))
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

func (ls *listServer) requested() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]string(nil), ls.paths...)
}

func (ls *listServer) conn(limit int) *completion.Connection {
	return completion.NewConnection(ls.srv.Client(), "tok", ls.srv.URL, false, limit)
}

func mustKind(t *testing.T, key string) kinds.Kind {
	t.Helper()
	k, ok := kinds.ByKey(key)
	require.True(t, ok, key)
	return k
}

// TestRequirements asserts the declaration per kind class: downstream kinds
// need the typed cluster resolved and accept any credential; management
// kinds need a management token, and only a cluster-namespaced kind needs
// the cluster resolved; a kind that does not complete declares nothing.
func TestRequirements(t *testing.T) {
	t.Parallel()
	downstream := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
	management := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}
	inCluster := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}
	tests := []struct {
		key  string
		want completion.Requirements
		ok   bool
	}{
		{key: "namespace", want: downstream, ok: true},
		{key: "node", want: downstream, ok: true},
		{key: "pod", want: downstream, ok: true},
		{key: "deployment", want: downstream, ok: true},
		{key: "nodedriver", want: management, ok: true},
		{key: "oidcclient", want: management, ok: true},
		{key: "user", want: management, ok: true},
		{key: "managementcluster", want: management, ok: true},
		{key: "provisioningcluster", want: management, ok: true},
		{key: "project", want: inCluster, ok: true},
		{key: "secret"},
		{key: "capicluster"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			got, ok := requirements(mustKind(t, tt.key))
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestKindSourceClaim asserts the source claims through the class rule and
// declines an unknown key and a kind that does not complete.
func TestKindSourceClaim(t *testing.T) {
	t.Parallel()
	reqs, ok := KindSource("pod").Claim(completion.Request{})
	require.True(t, ok)
	assert.Equal(t, completion.ClusterResolved, reqs.Cluster)
	for _, key := range []string{"secret", "capicluster", "no-such-kind", ""} {
		_, ok := KindSource(key).Claim(completion.Request{})
		assert.False(t, ok, key)
	}
	_, ok = NamespaceSource().Claim(completion.Request{Flag: "namespace"})
	assert.True(t, ok)
	_, ok = NodeSource().Claim(completion.Request{Flag: "node"})
	assert.True(t, ok)
}

// TestKindSourceCompletePaths asserts the path each kind class lists and
// the names answered: cluster-scoped downstream kinds through the typed
// cluster's proxy, namespaced kinds in the typed --namespace, management
// kinds through the management cluster, a workspace kind in its default
// namespace unless --namespace is typed, a cluster-namespaced kind in the
// resolved cluster's namespace.
func TestKindSourceCompletePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		req  completion.Request
		path string
	}{
		{name: "namespace flag", key: "namespace", req: completion.Request{Flag: "namespace", ClusterID: "c-x"}, path: "/k8s/clusters/c-x/api/v1/namespaces"},
		{name: "node flag", key: "node", req: completion.Request{Flag: "node", ClusterID: "c-x"}, path: "/k8s/clusters/c-x/api/v1/nodes"},
		{name: "namespace positional", key: "namespace", req: completion.Request{ClusterID: "c-x"}, path: "/k8s/clusters/c-x/api/v1/namespaces"},
		{name: "pod in a typed namespace", key: "pod", req: completion.Request{ClusterID: "c-x", Tail: []string{"get", "pod", "--cluster", "c-x", "--namespace", "default"}}, path: "/k8s/clusters/c-x/api/v1/namespaces/default/pods"},
		{name: "deployment in an equals-form namespace", key: "deployment", req: completion.Request{ClusterID: "c-x", Tail: []string{"--namespace=web"}}, path: "/k8s/clusters/c-x/apis/apps/v1/namespaces/web/deployments"},
		{name: "nodedriver", key: "nodedriver", req: completion.Request{}, path: "/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers"},
		{name: "oidcclient", key: "oidcclient", req: completion.Request{}, path: "/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients"},
		{name: "provisioning cluster in the default namespace", key: "provisioningcluster", req: completion.Request{}, path: "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"},
		{name: "provisioning cluster in a typed namespace", key: "provisioningcluster", req: completion.Request{Tail: []string{"--namespace", "team-a"}}, path: "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/team-a/clusters"},
		{name: "project in the resolved cluster", key: "project", req: completion.Request{Cluster: "prod", ClusterID: "c-x"}, path: "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/projects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := newListServer(t)
			got := KindSource(tt.key).Complete(t.Context(), ls.conn(1), tt.req)
			assert.Equal(t, []string{"b", "a"}, got.Names, "names in list order; the surface sorts")
			assert.Empty(t, got.Prefix)
			assert.Equal(t, []string{tt.path}, ls.requested())
		})
	}
}

// TestKindSourceCompleteAnswersNothing asserts the sites that answer
// nothing without a request: a project-scopable downstream kind under a
// typed --project (the namespace flag included), a namespaced kind with
// no --namespace or one that cannot be a path segment, and a cluster id
// that cannot be a path segment.
func TestKindSourceCompleteAnswersNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		req  completion.Request
	}{
		{name: "namespace flag under a project", key: "namespace", req: completion.Request{Flag: "namespace", ClusterID: "c-x", Project: "p-1"}},
		{name: "pod positional under a project", key: "pod", req: completion.Request{ClusterID: "c-x", Project: "p-1", Tail: []string{"--namespace", "default"}}},
		{name: "pod without a namespace", key: "pod", req: completion.Request{ClusterID: "c-x"}},
		{name: "pod with a slash in the namespace", key: "pod", req: completion.Request{ClusterID: "c-x", Tail: []string{"--namespace", "a/b"}}},
		{name: "pod with an all-dots namespace", key: "pod", req: completion.Request{ClusterID: "c-x", Tail: []string{"--namespace", ".."}}},
		{name: "namespaces of an unusable cluster id", key: "namespace", req: completion.Request{ClusterID: "prod cluster"}},
		{name: "provisioning cluster with a slash in the namespace", key: "provisioningcluster", req: completion.Request{Tail: []string{"--namespace", "a/b"}}},
		{name: "unknown key", key: "no-such-kind", req: completion.Request{ClusterID: "c-x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := newListServer(t)
			got := KindSource(tt.key).Complete(t.Context(), ls.conn(1), tt.req)
			assert.Empty(t, got.Names)
			assert.Empty(t, ls.requested())
		})
	}
}

// TestKindSourceCompleteFailsEmpty asserts a refused listing and an
// exhausted allowance answer nothing.
func TestKindSourceCompleteFailsEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, 1)
	assert.Empty(t, KindSource("nodedriver").Complete(t.Context(), conn, completion.Request{}).Names)

	ls := newListServer(t)
	exhausted := ls.conn(0)
	assert.Empty(t, KindSource("nodedriver").Complete(t.Context(), exhausted, completion.Request{}).Names)
	assert.True(t, exhausted.Exceeded())
	assert.Empty(t, ls.requested())
}
