package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

// lockedRoleServer serves role templates in every state the filter reads:
// unlocked, locked, locked spelled false, locked as a string, hidden and
// unlocked, and a locked template with no context; and global roles
// including one marked locked.
func lockedRoleServer(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates": `{"items":[
			{"metadata":{"name":"project-open"},"context":"project","displayName":"Project Open"},
			{"metadata":{"name":"project-locked"},"context":"project","locked":true,"displayName":"Project Locked"},
			{"metadata":{"name":"project-false"},"context":"project","locked":false,"displayName":"Project False"},
			{"metadata":{"name":"project-string"},"context":"project","locked":"true","displayName":"Project String"},
			{"metadata":{"name":"project-hidden"},"context":"project","hidden":true,"displayName":"Project Hidden"},
			{"metadata":{"name":"cluster-open"},"context":"cluster","displayName":"Cluster Open"},
			{"metadata":{"name":"cluster-locked"},"context":"cluster","locked":true,"displayName":"Cluster Locked"},
			{"metadata":{"name":"cluster-false"},"context":"cluster","locked":false,"displayName":"Cluster False"},
			{"metadata":{"name":"orphan-locked"},"locked":true,"displayName":"Orphan Locked"},
			{"metadata":{"name":"orphan-open"},"displayName":"Orphan Open"}
		]}`,
		"/k8s/clusters/local/apis/management.cattle.io/v3/globalroles": `{"items":[
			{"metadata":{"name":"global-open"},"displayName":"Global Open"},
			{"metadata":{"name":"global-locked"},"locked":true,"displayName":"Global Locked"}
		]}`,
	})
}

// TestGrantRevokeAndRoleSourcesAnswerLockedRolesByScope asserts, at each
// scope, one listing and the names and display names each source answers:
// grant's source leaves out a role template whose locked field is the
// JSON boolean true at project and cluster scope and keeps one with
// locked false, a string value, or no locked field and a hidden one;
// revoke's source and RoleSource answer every role template of the
// scope's context; a global role is kept by all three; a role template
// with no context is dropped by all three.
func TestGrantRevokeAndRoleSourcesAnswerLockedRolesByScope(t *testing.T) {
	t.Parallel()
	projectTail := []string{"jane", "--project", "Default", "--role"}
	clusterTail := []string{"jane", "--cluster", "c-x", "--role"}
	globalTail := []string{"jane", "--global", "--role"}
	projectOffered := map[string]string{
		"project-false": "Project False", "project-hidden": "Project Hidden",
		"project-open": "Project Open", "project-string": "Project String",
	}
	projectAll := map[string]string{
		"project-false": "Project False", "project-hidden": "Project Hidden",
		"project-locked": "Project Locked", "project-open": "Project Open",
		"project-string": "Project String",
	}
	clusterOffered := map[string]string{"cluster-false": "Cluster False", "cluster-open": "Cluster Open"}
	clusterAll := map[string]string{"cluster-false": "Cluster False", "cluster-locked": "Cluster Locked", "cluster-open": "Cluster Open"}
	globalAll := map[string]string{"global-locked": "Global Locked", "global-open": "Global Open"}
	sources := map[string]completion.Source{"grant": GrantSource(), "revoke": RevokeSource(), "role": RoleSource()}
	tests := []struct {
		name string
		tail []string
		want map[string]map[string]string
	}{
		{name: "project", tail: projectTail, want: map[string]map[string]string{"grant": projectOffered, "revoke": projectAll, "role": projectAll}},
		{name: "cluster", tail: clusterTail, want: map[string]map[string]string{"grant": clusterOffered, "revoke": clusterAll, "role": clusterAll}},
		{name: "global", tail: globalTail, want: map[string]map[string]string{"grant": globalAll, "revoke": globalAll, "role": globalAll}},
	}
	for _, tt := range tests {
		for source, src := range sources {
			t.Run(tt.name+" "+source, func(t *testing.T) {
				t.Parallel()
				s := lockedRoleServer(t)
				conn := s.Conn(1)
				got := src.Complete(t.Context(), conn, roleRequest(append([]string{source}, tt.tail...)...))
				want := tt.want[source]
				names := make([]string, 0, len(want))
				for name := range want {
					names = append(names, name)
				}
				assert.ElementsMatch(t, names, got.Names)
				assert.Equal(t, want, got.Descriptions)
				assert.Len(t, s.Requested(), 1)
				assert.False(t, conn.Exceeded())
			})
		}
	}
}

// TestRoleSourceKeepsLockedRoleTemplates asserts RoleSource answers every
// role template of the scope's context, locked ones included, and the
// locked global role.
func TestRoleSourceKeepsLockedRoleTemplates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail []string
		want []string
	}{
		{name: "project", tail: []string{"grant", "jane", "--project", "Default", "--role"}, want: []string{"project-false", "project-hidden", "project-locked", "project-open", "project-string"}},
		{name: "cluster", tail: []string{"grant", "jane", "--cluster", "c-x", "--role"}, want: []string{"cluster-false", "cluster-locked", "cluster-open"}},
		{name: "global", tail: []string{"grant", "jane", "--global", "--role"}, want: []string{"global-locked", "global-open"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RoleSource().Complete(t.Context(), lockedRoleServer(t).Conn(1), roleRequest(tt.tail...))
			assert.Equal(t, tt.want, got.Names)
		})
	}
}

// TestGrantAndRevokeSourcesAnswerTheSubjectPositionalAlike asserts the
// subject positional answers the same names and descriptions from both
// sources, and one listing each.
func TestGrantAndRevokeSourcesAnswerTheSubjectPositionalAlike(t *testing.T) {
	t.Parallel()
	grantServer := completiontest.NewFixed(t, completiontest.SubjectsBody)
	revokeServer := completiontest.NewFixed(t, completiontest.SubjectsBody)
	req := completion.Request{Index: 0}
	grant := GrantSource().Complete(t.Context(), grantServer.Conn(1), req)
	revoke := RevokeSource().Complete(t.Context(), revokeServer.Conn(1), req)
	assert.NotEmpty(t, grant.Names)
	assert.Equal(t, grant.Names, revoke.Names)
	assert.Equal(t, grant.Descriptions, revoke.Descriptions)
	assert.Len(t, grantServer.Requested(), 1)
	assert.Len(t, revokeServer.Requested(), 1)
}
