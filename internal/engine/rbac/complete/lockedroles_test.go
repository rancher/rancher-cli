package complete

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

// lockedRoleServer serves role templates in every state the filter reads:
// unlocked, locked, locked spelled false, locked as a string, hidden and
// unlocked, and a locked template with no context; and global roles that
// name no inherited role template, an unlocked, a locked and a missing one.
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
			{"metadata":{"name":"global-inherits-open"},"inheritedClusterRoles":["cluster-open"],"displayName":"Global Inherits Open"},
			{"metadata":{"name":"global-inherits-locked"},"inheritedClusterRoles":["cluster-locked"],"displayName":"Global Inherits Locked"},
			{"metadata":{"name":"global-inherits-missing"},"inheritedClusterRoles":["no-such-template"],"displayName":"Global Inherits Missing"}
		]}`,
	})
}

// TestGrantRevokeAndRoleSourcesAnswerLockedRolesByScope asserts, at each
// scope, one listing and the names and display names each source answers:
// grant's source leaves out a role template whose locked field is the
// JSON boolean true at project and cluster scope and keeps one with
// locked false, a string value, or no locked field and a hidden one;
// revoke's source and RoleSource answer every role template of the
// scope's context; a role template with no context is dropped by all three;
// at global scope grant's source leaves out the global roles that inherit a
// locked role template or one that does not exist and lists the role
// templates for it, a second request, while revoke's source and RoleSource
// answer every global role from one.
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
	globalOffered := map[string]string{"global-inherits-open": "Global Inherits Open", "global-open": "Global Open"}
	globalAll := map[string]string{
		"global-inherits-locked": "Global Inherits Locked", "global-inherits-missing": "Global Inherits Missing",
		"global-inherits-open": "Global Inherits Open", "global-open": "Global Open",
	}
	sources := map[string]completion.Source{"grant": GrantSource(), "revoke": RevokeSource(), "role": RoleSource()}
	tests := []struct {
		name string
		tail []string
		want map[string]map[string]string
	}{
		{name: "project", tail: projectTail, want: map[string]map[string]string{"grant": projectOffered, "revoke": projectAll, "role": projectAll}},
		{name: "cluster", tail: clusterTail, want: map[string]map[string]string{"grant": clusterOffered, "revoke": clusterAll, "role": clusterAll}},
		{name: "global", tail: globalTail, want: map[string]map[string]string{"grant": globalOffered, "revoke": globalAll, "role": globalAll}},
	}
	for _, tt := range tests {
		for source, src := range sources {
			t.Run(tt.name+" "+source, func(t *testing.T) {
				t.Parallel()
				s := lockedRoleServer(t)
				wantRequests := 1
				if tt.name == "global" && source == "grant" {
					wantRequests = 2
				}
				conn := s.Conn(wantRequests)
				got := src.Complete(t.Context(), conn, roleRequest(append([]string{source}, tt.tail...)...))
				want := tt.want[source]
				names := make([]string, 0, len(want))
				for name := range want {
					names = append(names, name)
				}
				assert.ElementsMatch(t, names, got.Names)
				assert.Equal(t, want, got.Descriptions)
				assert.Len(t, s.Requested(), wantRequests)
				assert.False(t, conn.Exceeded())
			})
		}
	}
}

// TestRoleSourceKeepsLockedRoleTemplates asserts RoleSource answers every
// role template of the scope's context, locked ones included, and every
// global role from one request.
func TestRoleSourceKeepsLockedRoleTemplates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail []string
		want []string
	}{
		{name: "project", tail: []string{"grant", "jane", "--project", "Default", "--role"}, want: []string{"project-false", "project-hidden", "project-locked", "project-open", "project-string"}},
		{name: "cluster", tail: []string{"grant", "jane", "--cluster", "c-x", "--role"}, want: []string{"cluster-false", "cluster-locked", "cluster-open"}},
		{name: "global", tail: []string{"grant", "jane", "--global", "--role"}, want: []string{"global-inherits-locked", "global-inherits-missing", "global-inherits-open", "global-open"}},
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

// TestGrantSourceGlobalRoleListsRoleTemplatesOnDemand asserts grant's global
// --role answer in the cases the role template listing decides: no request
// for role templates when no global role names an inherited one, one when
// some does, only the global roles that name none when that listing is
// refused (a 403) or when the allowance has no second request, and no
// answer without a request when no scope is typed.
func TestGrantSourceGlobalRoleListsRoleTemplatesOnDemand(t *testing.T) {
	t.Parallel()
	const (
		roleTemplates = "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
		globalRoles   = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
	)
	namesNone := `{"items":[{"metadata":{"name":"open"},"displayName":"Open"},{"metadata":{"name":"empty"},"inheritedClusterRoles":[],"displayName":"Empty"}]}`
	namesSome := `{"items":[
		{"metadata":{"name":"open"},"displayName":"Open"},
		{"metadata":{"name":"fine"},"inheritedClusterRoles":["cluster-open"],"displayName":"Fine"},
		{"metadata":{"name":"locked"},"inheritedClusterRoles":["cluster-locked"],"displayName":"Locked"}
	]}`
	templates := `{"items":[{"metadata":{"name":"cluster-open"},"context":"cluster"},{"metadata":{"name":"cluster-locked"},"context":"cluster","locked":true}]}`
	globalTail := []string{"grant", "jane", "--global", "--role"}
	tests := []struct {
		name         string
		server       func(t *testing.T) *completiontest.Server
		limit        int
		tail         []string
		want         []string
		wantRequests []string
	}{
		{
			name: "no global role names a role template",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesNone, roleTemplates: templates})
			},
			limit: 1, tail: globalTail, want: []string{"empty", "open"}, wantRequests: []string{globalRoles},
		},
		{
			name: "some global role names a role template",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesSome, roleTemplates: templates})
			},
			limit: 2, tail: globalTail, want: []string{"fine", "open"}, wantRequests: []string{globalRoles, roleTemplates},
		},
		{
			name: "the role template listing is refused",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.NewByAccept(t, func(path, _ string) (string, int) {
					if path == roleTemplates {
						return `{"message":"forbidden"}`, http.StatusForbidden
					}
					return namesSome, http.StatusOK
				})
			},
			limit: 2, tail: globalTail, want: []string{"open"}, wantRequests: []string{globalRoles, roleTemplates},
		},
		{
			name: "the allowance has no second request",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesSome, roleTemplates: templates})
			},
			limit: 1, tail: globalTail, want: []string{"open"}, wantRequests: []string{globalRoles},
		},
		{
			name: "no scope",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesSome, roleTemplates: templates})
			},
			limit: 2, tail: []string{"grant", "jane", "--role"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := tt.server(t)
			got := GrantSource().Complete(t.Context(), s.Conn(tt.limit), roleRequest(tt.tail...))
			assert.Equal(t, tt.want, got.Names)
			requested := s.Requested()
			var paths []string
			for _, uri := range requested {
				path, _, _ := strings.Cut(uri, "?")
				paths = append(paths, path)
			}
			assert.Equal(t, tt.wantRequests, paths)
		})
	}
}

// TestGrantSourceGlobalRoleTemplateListingPastTheDeadlineAnswersNothing
// asserts a deadline that ends during the role template listing, after the
// global roles were read, answers no candidates at all: the global roles
// that name an inherited role template are not silently missing from a
// partial list. A refused listing (a 403) still answers the global roles
// that name none.
func TestGrantSourceGlobalRoleTemplateListingPastTheDeadlineAnswersNothing(t *testing.T) {
	t.Parallel()
	const (
		roleTemplates = "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
		globalRoles   = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
	)
	globals := `{"items":[
		{"metadata":{"name":"open"},"displayName":"Open"},
		{"metadata":{"name":"fine"},"inheritedClusterRoles":["cluster-open"],"displayName":"Fine"}
	]}`
	s := completiontest.NewByAccept(t, func(path, _ string) (string, int) {
		if path == roleTemplates {
			time.Sleep(400 * time.Millisecond)
			return `{"items":[{"metadata":{"name":"cluster-open"},"context":"cluster"}]}`, http.StatusOK
		}
		return globals, http.StatusOK
	})
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	got := GrantSource().Complete(ctx, s.Conn(2), roleRequest("grant", "jane", "--global", "--role"))
	assert.Empty(t, got.Names)
	assert.Empty(t, got.Descriptions)
	var paths []string
	for _, uri := range s.Requested() {
		path, _, _ := strings.Cut(uri, "?")
		paths = append(paths, path)
	}
	assert.Equal(t, []string{globalRoles, roleTemplates}, paths, "the deadline ended the second listing, not the first")
}
