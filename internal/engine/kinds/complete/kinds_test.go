package complete

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listBody is the two-item list every path of the fixture answers.
const listBody = `{"items":[{"metadata":{"name":"b"}},{"metadata":{"name":"a"}}]}`

func mustKind(t *testing.T, key string) kinds.Kind {
	t.Helper()
	k, ok := kinds.ByKey(key)
	require.True(t, ok, key)
	return k
}

// TestRequirements asserts the declaration per kind class: downstream kinds
// need the typed cluster resolved and accept any credential; management
// kinds need a management token; a kind that does not complete declares
// nothing, and neither does a cluster-namespaced kind (project), which its
// own source serves.
func TestRequirements(t *testing.T) {
	t.Parallel()
	downstream := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}
	management := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}
	tests := []struct {
		key  string
		want completion.Requirements
		ok   bool
	}{
		{key: "namespace", want: downstream, ok: true},
		{key: "node", want: downstream, ok: true},
		{key: "pod", want: downstream, ok: true},
		{key: "deployment", want: downstream, ok: true},
		{key: "clusterrepo", want: downstream, ok: true},
		{key: "app", want: downstream, ok: true},
		{key: "operation", want: downstream, ok: true},
		{key: "nodedriver", want: management, ok: true},
		{key: "roletemplate", want: management, ok: true},
		{key: "globalrole", want: management, ok: true},
		{key: "oidcclient", want: management, ok: true},
		{key: "user", want: management, ok: true},
		{key: "managementcluster", want: management, ok: true},
		{key: "provisioningcluster", want: management, ok: true},
		{key: "etcdsnapshot", want: management, ok: true},
		{key: "project"}, // its own source lists the projects the caller can read
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.NewFixed(t, listBody)
			got := KindSource(tt.key).Complete(t.Context(), ls.Conn(1), tt.req)
			assert.Equal(t, []string{"b", "a"}, got.Names, "names in list order; the surface sorts")
			assert.Empty(t, got.Prefix)
			assert.Equal(t, []string{tt.path}, ls.Requested())
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
		{name: "project, which its own source serves", key: "project", req: completion.Request{Cluster: "prod", ClusterID: "c-x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.NewFixed(t, listBody)
			got := KindSource(tt.key).Complete(t.Context(), ls.Conn(1), tt.req)
			assert.Empty(t, got.Names)
			assert.Empty(t, ls.Requested())
		})
	}
}

// TestKindSourceCompleteFailsEmpty asserts a refused listing and an
// exhausted allowance answer nothing.
func TestKindSourceCompleteFailsEmpty(t *testing.T) {
	t.Parallel()
	refused := completiontest.NewStatus(t, 403)
	assert.Empty(t, KindSource("nodedriver").Complete(t.Context(), refused.Conn(1), completion.Request{}).Names)

	ls := completiontest.NewFixed(t, listBody)
	exhausted := ls.Conn(0)
	assert.Empty(t, KindSource("nodedriver").Complete(t.Context(), exhausted, completion.Request{}).Names)
	assert.True(t, exhausted.Exceeded())
	assert.Empty(t, ls.Requested())
}

// TestKindSourceDescribesTheKindsThatHaveAHandle asserts the generic source
// describes a name with the fields of the row's Handle.Lookup: the display
// name of a node driver (spec.displayName), a role template and a global
// role (displayName), never spec.description or description. A hidden or
// locked role is listed and described like any other, and a name without a
// display name has no description.
func TestKindSourceDescribesTheKindsThatHaveAHandle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key  string
		body string
		want map[string]string
	}{
		{
			key: "nodedriver",
			body: `{"items":[
				{"metadata":{"name":"amazonec2"},"spec":{"displayName":"Amazon EC2","description":"machines on AWS"}},
				{"metadata":{"name":"custom"},"spec":{"description":"no display name"}},
				{"metadata":{"name":"evil"},"spec":{"displayName":"a\nb\u001b[31m"}}
			]}`,
			want: map[string]string{"amazonec2": "Amazon EC2", "evil": "a\nb\u001b[31m"},
		},
		{
			key: "roletemplate",
			body: `{"items":[
				{"metadata":{"name":"project-owner"},"displayName":"Project Owner","description":"full control"},
				{"metadata":{"name":"edit"},"displayName":"Kubernetes edit","hidden":true},
				{"metadata":{"name":"retired"},"displayName":"Retired","locked":true},
				{"metadata":{"name":"plain"}}
			]}`,
			want: map[string]string{"project-owner": "Project Owner", "edit": "Kubernetes edit", "retired": "Retired"},
		},
		{
			key: "globalrole",
			body: `{"items":[
				{"metadata":{"name":"admin"},"displayName":"Administrator","description":"all rights"},
				{"metadata":{"name":"hidden"},"displayName":"Hidden","hidden":true,"locked":true}
			]}`,
			want: map[string]string{"admin": "Administrator", "hidden": "Hidden"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.NewFixed(t, tt.body)
			got := KindSource(tt.key).Complete(t.Context(), ls.Conn(1), completion.Request{})
			assert.Equal(t, tt.want, got.Descriptions)
			assert.Len(t, got.Names, strings.Count(tt.body, `"metadata"`), "every listed name is offered, hidden and locked included")
		})
	}
}

// TestKindSourceDescribesNothingWithoutAHandle asserts a Completable kind
// whose row has no Handle gets no description, whatever display or
// description fields its objects carry.
func TestKindSourceDescribesNothingWithoutAHandle(t *testing.T) {
	t.Parallel()
	body := `{"items":[{"metadata":{"name":"a"},"displayName":"A","description":"d","spec":{"displayName":"A","description":"d"}}]}`
	tests := []struct {
		key string
		req completion.Request
	}{
		{key: "pod", req: completion.Request{ClusterID: "c-x", Tail: []string{"--namespace", "default"}}},
		{key: "deployment", req: completion.Request{ClusterID: "c-x", Tail: []string{"--namespace", "default"}}},
		{key: "namespace", req: completion.Request{ClusterID: "c-x"}},
		{key: "node", req: completion.Request{ClusterID: "c-x"}},
		{key: "oidcclient", req: completion.Request{}},
		{key: "provisioningcluster", req: completion.Request{}},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			t.Parallel()
			k := mustKind(t, tt.key)
			require.True(t, k.Completable, tt.key)
			require.Nil(t, k.Handle, "the kind has a Handle; move it to the table with a Handle")
			ls := completiontest.NewFixed(t, body)
			got := KindSource(tt.key).Complete(t.Context(), ls.Conn(1), tt.req)
			assert.Equal(t, []string{"a"}, got.Names)
			assert.Empty(t, got.Descriptions)
		})
	}
}

// TestKindSourceDescribesWithTheLookupFieldsOnly asserts a node driver, a
// role template and a global role are described by the fields of their
// Handle.Lookup and by no other field the object carries.
func TestKindSourceDescribesWithTheLookupFieldsOnly(t *testing.T) {
	t.Parallel()
	body := `{"items":[
		{"metadata":{"name":"a"},"displayName":"Top Display","description":"decoy description","spec":{"name":"decoy spec","displayName":"Spec Display","description":"decoy spec description"},"status":{"displayName":"decoy status"}}
	]}`
	for key, want := range map[string]string{"nodedriver": "Spec Display", "roletemplate": "Top Display", "globalrole": "Top Display"} {
		ls := completiontest.NewFixed(t, body)
		got := KindSource(key).Complete(t.Context(), ls.Conn(1), completion.Request{})
		assert.Equal(t, map[string]string{"a": want}, got.Descriptions, key)
	}
}

// TestClusterRepoSourceClaim asserts the source claims only the --repo flag
// of the charts and app install commands, as one list read of the typed
// cluster, and declines every other site, another command's --repo included.
func TestClusterRepoSourceClaim(t *testing.T) {
	t.Parallel()
	reqs, ok := ClusterRepoSource().Claim(completion.Request{Command: ChartsCommand, Flag: "repo"})
	require.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}, reqs)
	for _, flag := range []string{"", "cluster", "namespace", "repository"} {
		_, ok := ClusterRepoSource().Claim(completion.Request{Command: ChartsCommand, Flag: flag})
		assert.False(t, ok, "flag %q", flag)
	}
	installReqs, ok := ClusterRepoSourceFor(InstallCommand).Claim(completion.Request{Command: InstallCommand, Flag: "repo"})
	require.True(t, ok, "the --repo of app install")
	assert.Equal(t, reqs, installReqs)
	for _, flag := range []string{"", "cluster", "chart", "version"} {
		_, ok := ClusterRepoSourceFor(InstallCommand).Claim(completion.Request{Command: InstallCommand, Flag: flag})
		assert.False(t, ok, "app install flag %q", flag)
	}
	for _, command := range []string{"", "get clusterrepo", "install", "chart"} {
		_, ok := ClusterRepoSource().Claim(completion.Request{Command: command, Flag: "repo"})
		assert.False(t, ok, "command %q", command)
	}
}

// TestClusterRepoSourceOffersTheRepositoriesThatAreNotDisabled asserts a
// ClusterRepo with spec.enabled false is not offered, one with it true or
// with no spec.enabled is, and the one list read goes to the typed cluster.
func TestClusterRepoSourceOffersTheRepositoriesThatAreNotDisabled(t *testing.T) {
	t.Parallel()
	body := `{"items":[
	  {"metadata":{"name":"on"},"spec":{"enabled":true}},
	  {"metadata":{"name":"off"},"spec":{"enabled":false}},
	  {"metadata":{"name":"unset"},"spec":{"url":"https://charts.example.test"}},
	  {"metadata":{"name":"nospec"}}]}`
	ls := completiontest.NewFixed(t, body)
	got := ClusterRepoSource().Complete(t.Context(), ls.Conn(1), completion.Request{Command: ChartsCommand, Flag: "repo", ClusterID: "c-x"})
	assert.Equal(t, []string{"on", "unset", "nospec"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/c-x/apis/catalog.cattle.io/v1/clusterrepos"}, ls.Requested())
}

// TestClusterRepoSourceAnswersNothingWithoutACluster asserts a line with no
// resolved cluster sends no request, and neither does a cluster id that
// cannot be a path segment.
func TestClusterRepoSourceAnswersNothingWithoutACluster(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "a/b"} {
		ls := completiontest.NewFixed(t, listBody)
		got := ClusterRepoSource().Complete(t.Context(), ls.Conn(1), completion.Request{Command: ChartsCommand, Flag: "repo", ClusterID: id})
		assert.Empty(t, got.Names, "cluster %q", id)
		assert.Empty(t, ls.Requested(), "cluster %q", id)
	}
}
