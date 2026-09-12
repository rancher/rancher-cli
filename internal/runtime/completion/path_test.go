package completion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidSegment asserts the shell-safe charset, the no-leading-hyphen
// rule, the no-slash rule, and the all-dots rejection.
func TestValidSegment(t *testing.T) {
	t.Parallel()
	valid := []string{"c-m-abc123", "local", "fleet-default", "default", "a.b", "v1", "management.cattle.io", "_x", "x_", "0"}
	for _, s := range valid {
		assert.True(t, ValidSegment(s), s)
	}
	invalid := []string{"", "-x", "a/b", "..", ".", "...", "a b", "a;b", "a$b", "a?b", "a#b", "%2e%2e", "\u00fcnicode", "a\nb"}
	for _, s := range invalid {
		assert.False(t, ValidSegment(s), "%q", s)
	}
}

// TestPathBuilders asserts every builder's shape, the core-group form, the
// optional namespace on a named resource, and the query encoding.
func TestPathBuilders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    Path
		err  error
		want string
	}{
		{name: "management grouped", want: "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles",
			p: must(ManagementPath("management.cattle.io", "v3", "globalroles"))},
		{name: "management core", want: "/k8s/clusters/local/api/v1/namespaces",
			p: must(ManagementPath("", "v1", "namespaces"))},
		{name: "cluster grouped", want: "/k8s/clusters/c-x/apis/apps/v1/deployments",
			p: must(ClusterPath("c-x", "apps", "v1", "deployments"))},
		{name: "cluster core", want: "/k8s/clusters/c-x/api/v1/nodes",
			p: must(ClusterPath("c-x", "", "v1", "nodes"))},
		{name: "namespaced grouped", want: "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters",
			p: must(NamespacedPath("local", "provisioning.cattle.io", "v1", "fleet-default", "clusters"))},
		{name: "management namespaced", want: "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-x/projects",
			p: must(ManagementNamespacedPath("management.cattle.io", "v3", "c-x", "projects"))},
		{name: "namespaced core", want: "/k8s/clusters/c-x/api/v1/namespaces/default/pods",
			p: must(NamespacedPath("c-x", "", "v1", "default", "pods"))},
		{name: "named namespaced", want: "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod",
			p: must(NamedResourcePath("local", "provisioning.cattle.io", "v1", "fleet-default", "clusters", "prod"))},
		{name: "named cluster-scoped", want: "/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients/my-client",
			p: must(NamedResourcePath("local", "management.cattle.io", "v3", "", "oidcclients", "my-client"))},
		{name: "query", want: "/k8s/clusters/c-x/api/v1/namespaces/default/pods?labelSelector=app%3Dx%2Ctier%3Dweb",
			p: must(NamespacedPath("c-x", "", "v1", "default", "pods")).WithQuery("labelSelector", "app=x,tier=web")},
		{name: "two queries", want: "/k8s/clusters/c-x/api/v1/nodes?a=1&b=2%2F3",
			p: must(ClusterPath("c-x", "", "v1", "nodes")).WithQuery("a", "1").WithQuery("b", "2/3")},
		{name: "steve collection", want: "/v1/management.cattle.io.clusters",
			p: must(SteveCollectionPath("management.cattle.io.clusters"))},
		{name: "steve collection with a continue token", want: "/v1/management.cattle.io.clusters?continue=x%26y",
			p: must(SteveCollectionPath("management.cattle.io.clusters")).WithQuery("continue", "x&y")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.p.String())
		})
	}
}

// TestPathBuildersRejectInvalidSegments asserts every builder rejects a
// slash, an all-dots segment, an empty segment, a leading hyphen and a
// space in every position - group, version, resource, cluster id,
// namespace and name - returning a zero Path.
func TestPathBuildersRejectInvalidSegments(t *testing.T) {
	t.Parallel()
	bad := []string{"a/b", "..", ".", "", "-x", "a b"}
	for _, s := range bad {
		t.Run("["+s+"]", func(t *testing.T) {
			t.Parallel()
			results := []struct {
				p   Path
				err error
			}{}
			add := func(p Path, err error) {
				results = append(results, struct {
					p   Path
					err error
				}{p, err})
			}
			add(ManagementPath("management.cattle.io", "v3", s))
			add(SteveCollectionPath(s))
			if s != "" { // an empty group is the core-group form, not a segment
				add(ManagementPath(s, "v3", "users"))
			}
			add(ManagementPath("management.cattle.io", s, "users"))
			add(ClusterPath(s, "", "v1", "nodes"))
			add(ClusterPath("c-x", "", "v1", s))
			add(NamespacedPath("c-x", "", "v1", s, "pods"))
			add(NamespacedPath(s, "", "v1", "default", "pods"))
			add(NamespacedPath("c-x", "", "v1", "default", s))
			add(ManagementNamespacedPath("management.cattle.io", "v3", s, "projects"))
			add(NamedResourcePath("local", "management.cattle.io", "v3", "", "oidcclients", s))
			add(NamedResourcePath("local", "management.cattle.io", "v3", "", s, "c"))
			if s != "" { // an empty namespace is the cluster-scoped form, not a segment
				add(NamedResourcePath("local", "management.cattle.io", "v3", s, "oidcclients", "c"))
			}
			for i, r := range results {
				require.ErrorIs(t, r.err, ErrInvalidSegment, "builder %d", i)
				assert.Empty(t, r.p.String(), "builder %d", i)
			}
		})
	}
}

// TestWithQueryOnZeroPathStaysZero asserts a query cannot make an empty
// Path requestable.
func TestWithQueryOnZeroPathStaysZero(t *testing.T) {
	t.Parallel()
	assert.Empty(t, Path{}.WithQuery("a", "b").String())
}

func must(p Path, err error) Path {
	if err != nil {
		panic(err)
	}
	return p
}
