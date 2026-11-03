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
	"github.com/stretchr/testify/require"
)

// TestGlobalRoleSourceClaimsTheFlagValueAlone asserts the source claims the
// --global-role value with a management need, no cluster need and two
// listings whatever else is typed, and declines the positional, the other
// flags and a request that names no flag.
func TestGlobalRoleSourceClaimsTheFlagValueAlone(t *testing.T) {
	t.Parallel()
	for _, req := range []completion.Request{
		{Flag: "global-role"},
		{Flag: "global-role", Tail: []string{"alice", "--global"}},
		{Flag: "global-role", Tail: []string{"alice", "--cluster", "c-x"}},
	} {
		reqs, ok := GlobalRoleSource().Claim(req)
		require.True(t, ok, "%+v", req)
		assert.Equal(t, completion.ManagementTwoListings(), reqs, "%+v", req)
	}
	for _, req := range []completion.Request{
		{},
		{Index: 0},
		{Index: 0, Tail: []string{"--global-role", "x"}},
		{Flag: "description"},
		{Flag: "role"},
		{Flag: "display-name"},
	} {
		_, ok := GlobalRoleSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestGlobalRoleSourceCompletes asserts one listing of the GlobalRoles
// answers every id, each described by its display name, a repeated row once,
// and a refused listing answering nothing.
func TestGlobalRoleSourceCompletes(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"admin"},"displayName":"Administrator"},
		{"metadata":{"name":"user"},"displayName":"User","newUserDefault":true},
		{"metadata":{"name":"user"},"displayName":"User"},
		{"metadata":{"name":"user-base"}}
	]}`)
	got := GlobalRoleSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "global-role"})
	assert.Equal(t, []string{"admin", "user", "user-base"}, got.Names)
	assert.Equal(t, map[string]string{"admin": "Administrator", "user": "User"}, got.Descriptions)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"}, s.Requested())

	refused := completiontest.NewStatus(t, 403)
	assert.Empty(t, GlobalRoleSource().Complete(t.Context(), refused.Conn(1), completion.Request{Flag: "global-role"}).Names)
}

// TestGlobalRoleSourceListsRoleTemplatesOnDemand asserts the --global-role
// answer in the cases the role template listing decides: no request for role
// templates when no global role names an inherited one, one when some does,
// and the global roles that name an inherited role template left out when it
// is locked or does not exist. When the role template listing is refused (a
// 403) or the allowance has no second request, only the global roles that
// name none are offered.
func TestGlobalRoleSourceListsRoleTemplatesOnDemand(t *testing.T) {
	t.Parallel()
	const (
		roleTemplates = "/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates"
		globalRoles   = "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"
	)
	namesNone := `{"items":[{"metadata":{"name":"open"},"displayName":"Open"},{"metadata":{"name":"empty"},"inheritedClusterRoles":[],"displayName":"Empty"}]}`
	namesSome := `{"items":[
		{"metadata":{"name":"open"},"displayName":"Open"},
		{"metadata":{"name":"fine"},"inheritedClusterRoles":["cluster-open"],"displayName":"Fine"},
		{"metadata":{"name":"locked"},"inheritedClusterRoles":["cluster-locked"],"displayName":"Locked"},
		{"metadata":{"name":"missing"},"inheritedClusterRoles":["no-such-template"],"displayName":"Missing"}
	]}`
	templates := `{"items":[{"metadata":{"name":"cluster-open"},"context":"cluster"},{"metadata":{"name":"cluster-locked"},"context":"cluster","locked":true}]}`
	tests := []struct {
		name         string
		server       func(t *testing.T) *completiontest.Server
		limit        int
		want         []string
		wantDescribe map[string]string
		wantRequests []string
	}{
		{
			name: "no global role names a role template",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesNone, roleTemplates: templates})
			},
			limit: 1, want: []string{"empty", "open"}, wantDescribe: map[string]string{"empty": "Empty", "open": "Open"},
			wantRequests: []string{globalRoles},
		},
		{
			name: "some global role names a role template",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesSome, roleTemplates: templates})
			},
			limit: 2, want: []string{"fine", "open"}, wantDescribe: map[string]string{"fine": "Fine", "open": "Open"},
			wantRequests: []string{globalRoles, roleTemplates},
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
			limit: 2, want: []string{"open"}, wantDescribe: map[string]string{"open": "Open"},
			wantRequests: []string{globalRoles, roleTemplates},
		},
		{
			name: "the allowance has no second request",
			server: func(t *testing.T) *completiontest.Server {
				t.Helper()
				return completiontest.New(t, map[string]string{globalRoles: namesSome, roleTemplates: templates})
			},
			limit: 1, want: []string{"open"}, wantDescribe: map[string]string{"open": "Open"},
			wantRequests: []string{globalRoles},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := tt.server(t)
			got := GlobalRoleSource().Complete(t.Context(), s.Conn(tt.limit), completion.Request{Flag: "global-role"})
			assert.Equal(t, tt.want, got.Names)
			assert.Equal(t, tt.wantDescribe, got.Descriptions)
			var paths []string
			for _, uri := range s.Requested() {
				path, _, _ := strings.Cut(uri, "?")
				paths = append(paths, path)
			}
			assert.Equal(t, tt.wantRequests, paths)
		})
	}
}

// TestGlobalRoleSourceTemplateListingPastTheDeadlineAnswersNothing asserts a
// deadline that ends during the role template listing, after the global roles
// were read, answers no candidates at all: the global roles that name an
// inherited role template are not silently missing from a partial list.
func TestGlobalRoleSourceTemplateListingPastTheDeadlineAnswersNothing(t *testing.T) {
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
	got := GlobalRoleSource().Complete(ctx, s.Conn(2), completion.Request{Flag: "global-role"})
	assert.Empty(t, got.Names)
	assert.Empty(t, got.Descriptions)
	var paths []string
	for _, uri := range s.Requested() {
		path, _, _ := strings.Cut(uri, "?")
		paths = append(paths, path)
	}
	assert.Equal(t, []string{globalRoles, roleTemplates}, paths, "the deadline ended the second listing, not the first")
}
