package complete

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roleServer serves a management API with mixed-context roletemplates and
// globalroles.
func roleServer(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{
		"/k8s/clusters/local/apis/management.cattle.io/v3/roletemplates": `{"items":[
			{"metadata":{"name":"project-owner"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"project-member"},"context":"project"},
			{"metadata":{"name":"cluster-owner"},"context":"cluster"},
			{"metadata":{"name":"bad name!"},"context":"project"},
			{"metadata":{"name":"no-context-role"}}
		]}`,
		"/k8s/clusters/local/apis/management.cattle.io/v3/globalroles": `{"items":[
			{"metadata":{"name":"admin"}},
			{"metadata":{"name":"user"}}
		]}`,
	})
}

func roleRequest(tail ...string) completion.Request {
	return completion.Request{Flag: "role", Tail: tail}
}

// TestRoleKind asserts the shared vocabulary rule: GlobalRoles without a
// context at global scope, RoleTemplates carrying the scope's context
// otherwise.
func TestRoleKind(t *testing.T) {
	t.Parallel()
	kind, roleContext := RoleKind(ScopeGlobal)
	assert.Equal(t, "globalrole", kind)
	assert.Empty(t, roleContext)
	kind, roleContext = RoleKind(ScopeProject)
	assert.Equal(t, "roletemplate", kind)
	assert.Equal(t, "project", roleContext)
	kind, roleContext = RoleKind(ScopeCluster)
	assert.Equal(t, "roletemplate", kind)
	assert.Equal(t, "cluster", roleContext)
}

// TestScopeOfTail asserts the presence heuristic: which scope each flag
// combination selects, the rejected combination, the boolean spellings
// of --global, and that a --global typed twice takes its last spelling.
func TestScopeOfTail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		tail  []string
		want  Scope
		found bool
	}{
		{name: "no scope flag", tail: []string{"grant", "jane", "--role"}},
		{name: "global", tail: []string{"grant", "jane", "--global", "--role"}, want: ScopeGlobal, found: true},
		{name: "project", tail: []string{"grant", "jane", "--project", "Default", "--role"}, want: ScopeProject, found: true},
		{name: "project with cluster qualifier", tail: []string{"grant", "jane", "--project", "Default", "--cluster", "c-x", "--role"}, want: ScopeProject, found: true},
		{name: "cluster alone", tail: []string{"grant", "jane", "--cluster", "c-x", "--role"}, want: ScopeCluster, found: true},
		{name: "conflict global+project", tail: []string{"grant", "jane", "--global", "--project", "Default", "--role"}},
		{name: "conflict global+cluster", tail: []string{"grant", "jane", "--global", "--cluster", "c-x", "--role"}},
		{name: "global=false counts as absent", tail: []string{"grant", "jane", "--global=false", "--role"}},
		{name: "global=0 counts as absent", tail: []string{"grant", "jane", "--global=0", "--role"}},
		{name: "global=False counts as absent", tail: []string{"grant", "jane", "--global=False", "--role"}},
		{name: "global=f counts as absent", tail: []string{"grant", "jane", "--global=f", "--role"}},
		{name: "global=FALSE counts as absent", tail: []string{"grant", "jane", "--global=FALSE", "--role"}},
		{name: "global=true counts as present", tail: []string{"grant", "jane", "--global=true", "--role"}, want: ScopeGlobal, found: true},
		{name: "global=1 counts as present", tail: []string{"grant", "jane", "--global=1", "--role"}, want: ScopeGlobal, found: true},
		{name: "global=T counts as present", tail: []string{"grant", "jane", "--global=T", "--role"}, want: ScopeGlobal, found: true},
		{name: "global=garbage counts as present (mid-edit)", tail: []string{"grant", "jane", "--global=garbage", "--role"}, want: ScopeGlobal, found: true},
		{name: "global=false with project selects project", tail: []string{"grant", "jane", "--global=false", "--project", "Default", "--role"}, want: ScopeProject, found: true},
		{name: "empty project value", tail: []string{"grant", "jane", "--project=", "--role"}, want: ScopeProject, found: true},
		{name: "empty cluster value", tail: []string{"grant", "jane", "--cluster=", "--role"}, want: ScopeCluster, found: true},
		{name: "equals-form project", tail: []string{"grant", "jane", "--project=Default", "--role"}, want: ScopeProject, found: true},
		{name: "prefix is not presence", tail: []string{"grant", "jane", "--projected", "--role"}},
		{name: "global then global=false with a cluster selects cluster", tail: []string{"grant", "jane", "--global", "--global=false", "--cluster", "c-x", "--role"}, want: ScopeCluster, found: true},
		{name: "global=false then global with a cluster conflicts", tail: []string{"grant", "jane", "--global=false", "--global", "--cluster", "c-x", "--role"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, found := ScopeOfTail(tt.tail)
			assert.Equal(t, tt.found, found)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestRoleSourceClaim asserts the source claims the --role value site with
// no cluster need, a management credential and one listing, and declines
// every other site of grant and revoke.
func TestRoleSourceClaim(t *testing.T) {
	t.Parallel()
	src := RoleSource()
	reqs, ok := src.Claim(roleRequest("grant", "jane", "--global", "--role"))
	require.True(t, ok)
	assert.Equal(t, completion.Requirements{
		Cluster:    completion.ClusterNone,
		Credential: completion.CredentialManagement,
		Allowance:  completion.AllowanceOne,
	}, reqs)

	_, ok = src.Claim(roleRequest("grant", "jane", "--role"))
	assert.True(t, ok, "a --role site with no scope is still owned; Complete answers nothing")

	declined := []completion.Request{
		{Index: 0, Tail: []string{"grant"}},
		{Flag: "cluster", Tail: []string{"grant", "jane", "--cluster"}},
		{Flag: "project", Cluster: "c-x", Tail: []string{"grant", "jane", "--cluster", "c-x", "--project"}},
		{Flag: "type", Tail: []string{"revoke", "jane", "--type"}},
	}
	for _, req := range declined {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestRoleSourceCompleteByScope asserts each scope lists its collection
// once and answers the names of the scope's context: project- or
// cluster-context roletemplates, deduped, context-less rows excluded;
// every globalrole at global scope. Names are answered unsanitized; the
// surface drops "bad name!".
func TestRoleSourceCompleteByScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail []string
		want []string
	}{
		{name: "global", tail: []string{"grant", "jane", "--global", "--role"}, want: []string{"admin", "user"}},
		{name: "project", tail: []string{"grant", "jane", "--project", "Default", "--role"}, want: []string{"bad name!", "project-member", "project-owner"}},
		{name: "project with cluster qualifier", tail: []string{"revoke", "jane", "--project=Default", "--cluster", "c-x", "--role"}, want: []string{"bad name!", "project-member", "project-owner"}},
		{name: "cluster", tail: []string{"grant", "jane", "--cluster", "c-x", "--role"}, want: []string{"cluster-owner"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := roleServer(t)
			conn := s.Conn(1)
			got := RoleSource().Complete(t.Context(), conn, roleRequest(tt.tail...))
			assert.Equal(t, tt.want, got.Names)
			assert.Empty(t, got.Prefix)
			assert.Len(t, s.Requested(), 1)
			assert.False(t, conn.Exceeded())
		})
	}
}

// TestRoleSourceCompleteNoScopeIsEmptyWithoutARequest asserts no scope and
// the rejected combination answer nothing and send nothing.
func TestRoleSourceCompleteNoScopeIsEmptyWithoutARequest(t *testing.T) {
	t.Parallel()
	for _, tail := range [][]string{
		{"grant", "jane", "--role"},
		{"grant", "jane", "--global", "--project", "Default", "--role"},
	} {
		s := roleServer(t)
		conn := s.Conn(1)
		got := RoleSource().Complete(t.Context(), conn, roleRequest(tail...))
		assert.Empty(t, got.Names, "%v", tail)
		assert.Empty(t, s.Requested(), "%v", tail)
	}
}

// TestRoleSourceCompleteFailsEmpty asserts a refused listing, an
// undecodable body, an exhausted allowance and a deadline each answer
// nothing.
func TestRoleSourceCompleteFailsEmpty(t *testing.T) {
	t.Parallel()
	req := roleRequest("grant", "jane", "--global", "--role")

	t.Run("401", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, RoleSource().Complete(t.Context(), completiontest.NewStatus(t, 401).Conn(1), req).Names)
	})

	t.Run("malformed body", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, RoleSource().Complete(t.Context(), completiontest.NewFixed(t, `{not json`).Conn(1), req).Names)
	})

	t.Run("allowance exhausted", func(t *testing.T) {
		t.Parallel()
		s := roleServer(t)
		conn := s.Conn(0)
		assert.Empty(t, RoleSource().Complete(t.Context(), conn, req).Names)
		assert.True(t, conn.Exceeded())
		assert.Empty(t, s.Requested())
	})

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			block := make(chan struct{})
			srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-block
			}))
			t.Cleanup(func() { close(block) })
			client := srv.Client() // sets srv.URL: an in-memory server's URL is set by its first Client call
			conn := completion.NewConnection(client, "tok", srv.URL, false, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			assert.Empty(t, RoleSource().Complete(ctx, conn, req).Names)
		})
	})
}
