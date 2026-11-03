package rbac

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func roleTemplateObj(name, context string, extra map[string]any) unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": name}, "displayName": name}
	if context != "" {
		obj["context"] = context
	}
	maps.Copy(obj, extra)
	return unstructured.Unstructured{Object: obj}
}

func globalRoleObj(name string, inherited any) unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": name}, "displayName": name}
	if inherited != nil {
		obj["inheritedClusterRoles"] = inherited
	}
	return unstructured.Unstructured{Object: obj}
}

// lockedCandidateTemplates are the role templates the candidate lists read:
// unlocked, locked, hidden, locked spelled false, a locked one with no
// context, and an unlocked one that inherits a locked one through
// roleTemplateNames.
func lockedCandidateTemplates() *unstructured.UnstructuredList {
	return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		roleTemplateObj("project-open", "project", nil),
		roleTemplateObj("project-locked", "project", map[string]any{"locked": true}),
		roleTemplateObj("project-hidden", "project", map[string]any{"hidden": true}),
		roleTemplateObj("project-false", "project", map[string]any{"locked": false}),
		roleTemplateObj("cluster-open", "cluster", nil),
		roleTemplateObj("cluster-locked", "cluster", map[string]any{"locked": true}),
		roleTemplateObj("cluster-hidden", "cluster", map[string]any{"hidden": true}),
		roleTemplateObj("cluster-false", "cluster", map[string]any{"locked": false}),
		roleTemplateObj("cluster-parent-of-locked", "cluster", map[string]any{"roleTemplateNames": []any{"cluster-locked"}}),
		roleTemplateObj("orphan-locked", "", map[string]any{"locked": true}),
	}}
}

// lockedCandidateGlobalRoles are the global roles the candidate lists read.
// Rancher's webhook refuses a binding to a global role whose own
// inheritedClusterRoles names a locked role template or one that does not
// exist, whatever the context of the role template and whatever the role
// template itself inherits.
func lockedCandidateGlobalRoles() *unstructured.UnstructuredList {
	return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		globalRoleObj("global-open", nil),
		globalRoleObj("global-empty-list", []any{}),
		globalRoleObj("global-inherits-open", []any{"cluster-open"}),
		globalRoleObj("global-inherits-hidden", []any{"cluster-hidden"}),
		globalRoleObj("global-inherits-false", []any{"cluster-false"}),
		globalRoleObj("global-inherits-parent-of-locked", []any{"cluster-parent-of-locked"}),
		globalRoleObj("global-inherits-locked", []any{"cluster-locked"}),
		globalRoleObj("global-inherits-project-locked", []any{"project-locked"}),
		globalRoleObj("global-inherits-orphan-locked", []any{"orphan-locked"}),
		globalRoleObj("global-inherits-mixed", []any{"cluster-open", "cluster-locked"}),
		globalRoleObj("global-inherits-missing", []any{"no-such-template"}),
	}}
}

// TestGrantRoleErrorCandidatesLeaveOutLockedRoles asserts, for every grant
// path that lists roles to bind in a NOT_FOUND error (an unknown role at
// project, cluster and global scope, and a role of another context or none
// at cluster scope), that the candidates are the roles Rancher accepts in a
// new binding, in list order: no locked role template at project and
// cluster scope, and at global scope no global role that inherits a locked
// role template or one that does not exist. A needle that matches only roles
// left out lists the whole pool and never brings one back. The expected ids
// are written out by name, not computed from the fixture.
func TestGrantRoleErrorCandidatesLeaveOutLockedRoles(t *testing.T) {
	t.Parallel()
	projectPool := []string{"project-open", "project-hidden", "project-false"}
	clusterPool := []string{"cluster-open", "cluster-hidden", "cluster-false", "cluster-parent-of-locked"}
	globalPool := []string{"global-open", "global-empty-list", "global-inherits-open", "global-inherits-hidden", "global-inherits-false", "global-inherits-parent-of-locked"}
	leftOut := []string{
		"project-locked", "cluster-locked", "orphan-locked", "global-inherits-locked", "global-inherits-project-locked",
		"global-inherits-orphan-locked", "global-inherits-mixed", "global-inherits-missing",
	}
	tests := []struct {
		name    string
		scope   map[string]string
		global  bool
		role    string
		gets    map[string]*unstructured.Unstructured
		want    []string
		wantMsg string
	}{
		{name: "project unknown role", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "zzz-none", want: projectPool},
		{name: "project needle matches only a locked role", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, role: "project-locked-x", want: projectPool},
		{name: "cluster unknown role", scope: map[string]string{"cluster": "c-x"}, role: "zzz-none", want: clusterPool},
		{name: "cluster needle matches a locked and an unlocked role", scope: map[string]string{"cluster": "c-x"}, role: "locked", want: []string{"cluster-parent-of-locked"}},
		{name: "cluster needle matches only a locked role", scope: map[string]string{"cluster": "c-x"}, role: "cluster-locked-x", want: clusterPool},
		{
			name: "cluster role of another context", scope: map[string]string{"cluster": "c-x"}, role: "project-open",
			gets: map[string]*unstructured.Unstructured{"/project-open": {Object: map[string]any{"metadata": map[string]any{"name": "project-open"}, "context": "project"}}},
			want: clusterPool, wantMsg: "is a project role",
		},
		{
			name: "cluster role with no context", scope: map[string]string{"cluster": "c-x"}, role: "orphan-locked",
			gets: map[string]*unstructured.Unstructured{"/orphan-locked": {Object: map[string]any{"metadata": map[string]any{"name": "orphan-locked"}, "locked": true}}},
			want: clusterPool, wantMsg: "has no context",
		},
		{name: "global unknown role", global: true, role: "zzz-none", want: globalPool},
		{name: "global needle matches an offered and a left out role", global: true, role: "locked", want: []string{"global-inherits-parent-of-locked"}},
		{name: "global needle matches only left out roles", global: true, role: "inherits-locked", want: globalPool},
		{name: "global needle matches only the role naming a missing template", global: true, role: "missing", want: globalPool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gets := map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")}
			maps.Copy(gets, tt.gets)
			client := &fakeClient{
				users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				gets:  gets,
				listsByGVR: map[string]*unstructured.UnstructuredList{
					"roletemplates": lockedCandidateTemplates(),
					"globalroles":   lockedCandidateGlobalRoles(),
				},
			}
			in := stubInput{strings: map[string]string{"role": tt.role}, args: []string{"u-abc"}}
			maps.Copy(in.strings, tt.scope)
			if tt.global {
				in.bools = map[string]bool{"global": true}
			}
			_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a *clierr.Error, got %v", err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Contains(t, ce.Message, tt.wantMsg)
			assert.Equal(t, tt.want, candidateIDs(ce))
			assert.Zero(t, ce.CandidatesTotal, "a list of at most CandidateLimit roles records no total")
			for _, id := range candidateIDs(ce) {
				assert.NotContains(t, leftOut, id)
			}
		})
	}
}

// TestGrantRoleErrorCandidatesListTheRoleTemplatesOnlyWhenNeeded asserts the
// lists a global-scope NOT_FOUND error reads: the global roles alone when
// none names an inherited role template, the role templates as well when
// one does, and one list at project and cluster scope; and the degraded
// answers: with the role template list failing the candidates are the global
// roles that name no inherited role template, and with the global role list
// failing there are no candidates.
func TestGrantRoleErrorCandidatesListTheRoleTemplatesOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		global     bool
		scope      map[string]string
		globals    *unstructured.UnstructuredList
		listErrs   map[string]error
		wantLists  int
		wantIDs    []string
		wantNoCand bool
	}{
		{
			name: "no global role names a role template", global: true,
			globals:   &unstructured.UnstructuredList{Items: []unstructured.Unstructured{globalRoleObj("global-open", nil), globalRoleObj("global-empty-list", []any{})}},
			wantLists: 1, wantIDs: []string{"global-open", "global-empty-list"},
		},
		{
			name: "some global role names a role template", global: true, globals: lockedCandidateGlobalRoles(),
			wantLists: 2, wantIDs: []string{"global-open", "global-empty-list", "global-inherits-open", "global-inherits-hidden", "global-inherits-false", "global-inherits-parent-of-locked"},
		},
		{
			name: "the role template list fails", global: true, globals: lockedCandidateGlobalRoles(),
			listErrs:  map[string]error{"roletemplates": clierr.New(clierr.CodeRBAC, "roletemplates is forbidden")},
			wantLists: 2, wantIDs: []string{"global-open", "global-empty-list"},
		},
		{
			name: "the global role list fails", global: true, globals: lockedCandidateGlobalRoles(),
			listErrs:  map[string]error{"globalroles": clierr.New(clierr.CodeRBAC, "globalroles is forbidden")},
			wantLists: 1, wantNoCand: true,
		},
		{name: "cluster scope", scope: map[string]string{"cluster": "c-x"}, wantLists: 1, wantIDs: []string{"cluster-open", "cluster-hidden", "cluster-false", "cluster-parent-of-locked"}},
		{name: "project scope", scope: map[string]string{"project": "p-x", "cluster": "c-x"}, wantLists: 1, wantIDs: []string{"project-open", "project-hidden", "project-false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			globals := tt.globals
			if globals == nil {
				globals = lockedCandidateGlobalRoles()
			}
			client := &fakeClient{
				users:        []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				gets:         map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
				listsByGVR:   map[string]*unstructured.UnstructuredList{"roletemplates": lockedCandidateTemplates(), "globalroles": globals},
				listErrByGVR: tt.listErrs,
			}
			in := stubInput{strings: map[string]string{"role": "zzz-none"}, args: []string{"u-abc"}}
			maps.Copy(in.strings, tt.scope)
			if tt.global {
				in.bools = map[string]bool{"global": true}
			}
			_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), in)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a *clierr.Error, got %v", err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Contains(t, ce.Message, "role not found: zzz-none")
			assert.Equal(t, tt.wantLists, client.lists)
			if tt.wantNoCand {
				assert.Empty(t, ce.Candidates)
				assert.NotEmpty(t, ce.Hint)
				return
			}
			assert.Equal(t, tt.wantIDs, candidateIDs(ce))
		})
	}
}

// TestGrantRoleErrorCandidatesTotalCountsWhatRemains asserts that when the
// roles Rancher accepts outnumber CandidateLimit, the error lists the first
// CandidateLimit of them in list order and its total counts the accepted
// roles alone, at cluster scope with locked role templates among the
// listed ones and at global scope with global roles that inherit a locked
// role template.
func TestGrantRoleErrorCandidatesTotalCountsWhatRemains(t *testing.T) {
	t.Parallel()
	var templates, globals []unstructured.Unstructured
	var clusterWant, globalWant []string
	for i := 1; i <= 12; i++ {
		open := fmt.Sprintf("cluster-role-%02d", i)
		templates = append(templates,
			roleTemplateObj(open, "cluster", nil),
			roleTemplateObj(fmt.Sprintf("cluster-locked-%02d", i), "cluster", map[string]any{"locked": true}))
		clusterWant = append(clusterWant, open)
		fine := fmt.Sprintf("global-role-%02d", i)
		globals = append(globals,
			globalRoleObj(fine, []any{open}),
			globalRoleObj(fmt.Sprintf("global-locked-%02d", i), []any{fmt.Sprintf("cluster-locked-%02d", i)}))
		globalWant = append(globalWant, fine)
	}
	tests := []struct {
		name  string
		in    stubInput
		want  []string
		total int
	}{
		{name: "cluster", in: stubInput{strings: map[string]string{"cluster": "c-x", "role": "zzz-none"}, args: []string{"u-abc"}}, want: clusterWant[:CandidateLimit], total: 12},
		{name: "global", in: stubInput{bools: map[string]bool{"global": true}, strings: map[string]string{"role": "zzz-none"}, args: []string{"u-abc"}}, want: globalWant[:CandidateLimit], total: 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{
				users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				listsByGVR: map[string]*unstructured.UnstructuredList{
					"roletemplates": {Items: templates},
					"globalroles":   {Items: globals},
				},
			}
			_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), tt.in)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a *clierr.Error, got %v", err)
			assert.Equal(t, tt.want, candidateIDs(ce))
			assert.Equal(t, tt.total, ce.CandidatesTotal)
		})
	}
}

// contextEndingClient is a fakeClient that ends the caller's context while
// it lists the roletemplates, as an interrupt or a deadline does during the
// second list of a global-scope error.
type contextEndingClient struct {
	*fakeClient
	cancel context.CancelFunc
}

func (c contextEndingClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	if gvr.Resource == "roletemplates" {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.fakeClient.List(ctx, gvr, ns, selector)
}

// TestGrantRoleErrorEndsWithTheContextErrorDuringTheSecondList asserts that
// when the context ends while the roletemplates are listed for a global-scope
// NOT_FOUND error, grant returns the context error and not a NOT_FOUND with
// the degraded candidates.
func TestGrantRoleErrorEndsWithTheContextErrorDuringTheSecondList(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := contextEndingClient{
		fakeClient: &fakeClient{
			users:      []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
			listsByGVR: map[string]*unstructured.UnstructuredList{"globalroles": lockedCandidateGlobalRoles(), "roletemplates": lockedCandidateTemplates()},
		},
		cancel: cancel,
	}
	in := stubInput{bools: map[string]bool{"global": true}, strings: map[string]string{"role": "zzz-none"}, args: []string{"u-abc"}}
	mc, ok := NewGrant(fakeFactory{c: client}).(runtime.MutatingCommand)
	require.True(t, ok)
	_, _, err := mc.Plan(ctx, in)
	require.ErrorIs(t, err, context.Canceled)
	_, isClierr := errors.AsType[*clierr.Error](err)
	assert.False(t, isClierr, "want the context error, got %v", err)
}
