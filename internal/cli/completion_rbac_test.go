package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	rbacBase                = "/k8s/clusters/c-aaa11/apis/rbac.authorization.k8s.io/v1"
	rbacClusterRolesPath    = rbacBase + "/clusterroles"
	rbacClusterBindingsPath = rbacBase + "/clusterrolebindings"
	rbacRolesPath           = rbacBase + "/namespaces/default/roles"
	rbacRoleBindingsPath    = rbacBase + "/namespaces/default/rolebindings"
)

// TestRBACKindNamesComplete asserts, through the real TAB callback, the
// candidates and the requests of the name argument of the four Kubernetes
// RBAC kinds: a cluster-wide list for clusterrole and clusterrolebinding, a
// list in the typed namespace for role and rolebinding, no request without a
// namespace or under a typed project, none on patch (it is refused), none with
// the network off, and no candidate that has a colon. NOT parallel: t.Setenv
// mutates env globals, setStoredConfig mutates storedConfig.
func TestRBACKindNamesComplete(t *testing.T) {
	s := newPathCountingServer(t, map[string]string{
		rbacClusterRolesPath:    items(plain("system:auth-delegator"), plain("view"), plain("admin")),
		rbacClusterBindingsPath: items(plain("system:node"), plain("cluster-admin")),
		rbacRolesPath:           items(plain("system:controller:leader"), plain("pod-reader")),
		rbacRoleBindingsPath:    items(plain("system:controller:leader"), plain("read-pods")),
	}, steveClustersPages)
	tests := []struct {
		name  string
		words []string
		want  []string
		hits  map[string]int
		noNet bool
	}{
		{name: "get clusterrole", words: []string{"get", "clusterrole", "--cluster", "c-aaa11"}, want: []string{"admin", "view"}, hits: map[string]int{rbacClusterRolesPath: 1}},
		{name: "get clusterroles through the alias", words: []string{"get", "clusterroles", "--cluster", "c-aaa11"}, want: []string{"admin", "view"}, hits: map[string]int{rbacClusterRolesPath: 1}},
		{name: "get clusterrolebinding", words: []string{"get", "clusterrolebinding", "--cluster", "c-aaa11"}, want: []string{"cluster-admin"}, hits: map[string]int{rbacClusterBindingsPath: 1}},
		{name: "get role in a namespace", words: []string{"get", "role", "--cluster", "c-aaa11", "--namespace", "default"}, want: []string{"pod-reader"}, hits: map[string]int{rbacRolesPath: 1}},
		{name: "get role in a namespace typed as -n", words: []string{"get", "role", "--cluster", "c-aaa11", "-n", "default"}, want: []string{"pod-reader"}, hits: map[string]int{rbacRolesPath: 1}},
		{name: "get rolebinding in a namespace", words: []string{"get", "rolebinding", "--cluster", "c-aaa11", "--namespace", "default"}, want: []string{"read-pods"}, hits: map[string]int{rbacRoleBindingsPath: 1}},
		{name: "get rolebinding in a namespace typed as -n", words: []string{"get", "rolebinding", "--cluster", "c-aaa11", "-n", "default"}, want: []string{"read-pods"}, hits: map[string]int{rbacRoleBindingsPath: 1}},
		{name: "get role without a namespace", words: []string{"get", "role", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "get rolebinding without a namespace", words: []string{"get", "rolebinding", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "get rolebinding under a typed project", words: []string{"get", "rolebinding", "--cluster", "c-aaa11", "--project", "p-1", "--namespace", "default"}, hits: map[string]int{}},
		{name: "patch role", words: []string{"patch", "role", "--cluster", "c-aaa11", "--namespace", "default"}, hits: map[string]int{}},
		{name: "patch clusterrole", words: []string{"patch", "clusterrole", "--cluster", "c-aaa11"}, hits: map[string]int{}},
		{name: "get clusterrole with the network off", words: []string{"get", "clusterrole", "--cluster", "c-aaa11"}, noNet: true, hits: map[string]int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completionFixture(t, s.srv.URL, !tt.noNet)
			s.reset()
			got := runCompletion(t, tt.words, "")
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.hits, s.reset())
		})
	}
}
