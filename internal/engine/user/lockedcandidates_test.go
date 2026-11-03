package user

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

// roleTemplate is a RoleTemplate with the given id, context ("" leaves the
// field out) and extra fields.
func roleTemplate(id, roleContext string, extra map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":  "management.cattle.io/v3",
		"kind":        "RoleTemplate",
		"metadata":    map[string]any{"name": id},
		"displayName": id,
	}}
	if roleContext != "" {
		obj.Object["context"] = roleContext
	}
	maps.Copy(obj.Object, extra)
	return obj
}

// inheritingRole is a GlobalRole with the given id and display name whose
// inheritedClusterRoles is inherited (nil leaves the field out).
func inheritingRole(id, display string, inherited []any) *unstructured.Unstructured {
	fields := map[string]any{"displayName": display}
	if inherited != nil {
		fields["inheritedClusterRoles"] = inherited
	}
	return globalRole(id, fields)
}

// lockedCandidateTemplates are the role templates the candidate lists read:
// unlocked, locked, hidden, locked spelled false, a locked one with no
// context, and an unlocked one that inherits a locked one through
// roleTemplateNames.
func lockedCandidateTemplates() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		roleTemplate("project-open", "project", nil),
		roleTemplate("project-locked", "project", map[string]any{"locked": true}),
		roleTemplate("project-hidden", "project", map[string]any{"hidden": true}),
		roleTemplate("project-false", "project", map[string]any{"locked": false}),
		roleTemplate("cluster-open", "cluster", nil),
		roleTemplate("cluster-locked", "cluster", map[string]any{"locked": true}),
		roleTemplate("cluster-hidden", "cluster", map[string]any{"hidden": true}),
		roleTemplate("cluster-false", "cluster", map[string]any{"locked": false}),
		roleTemplate("cluster-parent-of-locked", "cluster", map[string]any{"roleTemplateNames": []any{"cluster-locked"}}),
		roleTemplate("orphan-locked", "", map[string]any{"locked": true}),
	}
}

// lockedCandidateGlobalRoles are the global roles the candidate lists read.
// Rancher's webhook refuses a binding to a global role whose own
// inheritedClusterRoles names a locked role template or one that does not
// exist, whatever the context of the role template and whatever the role
// template itself inherits. The display name of each is its id in title case.
func lockedCandidateGlobalRoles() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		inheritingRole("global-open", "Global Open", nil),
		inheritingRole("global-empty-list", "Global Empty List", []any{}),
		inheritingRole("global-inherits-open", "Global Inherits Open", []any{"cluster-open"}),
		inheritingRole("global-inherits-hidden", "Global Inherits Hidden", []any{"cluster-hidden"}),
		inheritingRole("global-inherits-false", "Global Inherits False", []any{"cluster-false"}),
		inheritingRole("global-inherits-parent-of-locked", "Global Inherits Parent Of Locked", []any{"cluster-parent-of-locked"}),
		inheritingRole("global-inherits-locked", "Global Inherits Locked", []any{"cluster-locked"}),
		inheritingRole("global-inherits-project-locked", "Global Inherits Project Locked", []any{"project-locked"}),
		inheritingRole("global-inherits-orphan-locked", "Global Inherits Orphan Locked", []any{"orphan-locked"}),
		inheritingRole("global-inherits-mixed", "Global Inherits Mixed", []any{"cluster-open", "cluster-locked"}),
		inheritingRole("global-inherits-missing", "Global Inherits Missing", []any{"no-such-template"}),
	}
}

func lockedCandidateIDs(ce *clierr.Error) []string {
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	return ids
}

// planNotFound plans create user with the typed global role against client
// and returns the NOT_FOUND error.
func planNotFound(t *testing.T, client *fakeUserClient, typed string) *clierr.Error {
	t.Helper()
	_, _, err := plan(t, client, fakeSrc{body: []byte(testPassword)}, typedInput(typed))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a *clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	require.ErrorContains(t, err, "role not found: "+typed)
	return ce
}

// TestCreateUserGlobalRoleErrorCandidatesLeaveOutLockedRoles asserts the
// candidates of a NOT_FOUND error of --global-role are the global roles
// Rancher accepts in a new binding, in list order: no global role that
// inherits a locked role template or one that does not exist. A needle that
// matches only roles left out lists the whole pool and never brings one back.
// The expected ids are written out by name, not computed from the fixture.
func TestCreateUserGlobalRoleErrorCandidatesLeaveOutLockedRoles(t *testing.T) {
	t.Parallel()
	pool := []string{
		"global-open", "global-empty-list", "global-inherits-open", "global-inherits-hidden",
		"global-inherits-false", "global-inherits-parent-of-locked",
	}
	leftOut := []string{
		"global-inherits-locked", "global-inherits-project-locked", "global-inherits-orphan-locked",
		"global-inherits-mixed", "global-inherits-missing",
	}
	tests := []struct {
		name   string
		needle string
		want   []string
	}{
		{name: "unknown role", needle: "zzz-none", want: pool},
		{name: "needle matches an offered and a left out role", needle: "locked", want: []string{"global-inherits-parent-of-locked"}},
		{name: "needle matches only a role that inherits a locked role template", needle: "inherits-locked", want: pool},
		{name: "needle matches only a role that inherits a locked role template with no context", needle: "orphan", want: pool},
		{name: "needle matches offered and left out roles", needle: "inherits", want: []string{
			"global-inherits-open", "global-inherits-hidden", "global-inherits-false", "global-inherits-parent-of-locked",
		}},
		{name: "needle matches only the role that names a missing role template", needle: "missing", want: pool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeUserClient{roles: lockedCandidateGlobalRoles(), roleTemplates: lockedCandidateTemplates()}
			ce := planNotFound(t, client, tt.needle)
			assert.Equal(t, tt.want, lockedCandidateIDs(ce))
			assert.Zero(t, ce.CandidatesTotal, "a list of at most candidateLimit roles records no total")
			assert.Equal(t, clierr.Rancher+" get globalroles", ce.Hint)
			for _, id := range lockedCandidateIDs(ce) {
				assert.NotContains(t, leftOut, id)
			}
			assert.Empty(t, client.bindings)
			assert.Empty(t, client.writes)
		})
	}
}

// TestCreateUserGlobalRoleErrorCandidatesListTheRoleTemplatesOnlyWhenNeeded
// asserts the lists a NOT_FOUND error of --global-role reads: the global roles
// alone when none names an inherited role template, the role templates as
// well when one does; and the degraded answers: with the role template list
// failing the candidates are the global roles that name no inherited role
// template, and with the global role list failing there are no candidates.
func TestCreateUserGlobalRoleErrorCandidatesListTheRoleTemplatesOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		client        *fakeUserClient
		wantRoleLists int
		wantTemplates int
		wantIDs       []string
		wantNoCand    bool
	}{
		{
			name: "no global role names a role template",
			client: &fakeUserClient{
				roles:         []*unstructured.Unstructured{inheritingRole("global-open", "Global Open", nil), inheritingRole("global-empty-list", "Global Empty List", []any{})},
				roleTemplates: lockedCandidateTemplates(),
			},
			wantRoleLists: 1, wantTemplates: 0, wantIDs: []string{"global-open", "global-empty-list"},
		},
		{
			name:          "some global role names a role template",
			client:        &fakeUserClient{roles: lockedCandidateGlobalRoles(), roleTemplates: lockedCandidateTemplates()},
			wantRoleLists: 1, wantTemplates: 1,
			wantIDs: []string{
				"global-open", "global-empty-list", "global-inherits-open", "global-inherits-hidden",
				"global-inherits-false", "global-inherits-parent-of-locked",
			},
		},
		{
			name: "the role template list fails",
			client: &fakeUserClient{
				roles: lockedCandidateGlobalRoles(), roleTemplates: lockedCandidateTemplates(),
				roleTemplateListErr: clierr.New(clierr.CodeRBAC, "roletemplates is forbidden"),
			},
			wantRoleLists: 1, wantTemplates: 1, wantIDs: []string{"global-open", "global-empty-list"},
		},
		{
			name: "the global role list fails",
			client: &fakeUserClient{
				roles: lockedCandidateGlobalRoles(), roleTemplates: lockedCandidateTemplates(),
				roleListErr: clierr.New(clierr.CodeRBAC, "globalroles is forbidden"),
			},
			wantRoleLists: 1, wantTemplates: 0, wantNoCand: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ce := planNotFound(t, tt.client, "zzz-none")
			assert.Equal(t, tt.wantRoleLists, tt.client.roleListCalls)
			assert.Equal(t, tt.wantTemplates, tt.client.roleTemplateListCalls)
			assert.Equal(t, clierr.Rancher+" get globalroles", ce.Hint)
			if tt.wantNoCand {
				assert.Empty(t, ce.Candidates)
				return
			}
			assert.Equal(t, tt.wantIDs, lockedCandidateIDs(ce))
		})
	}
}

// TestCreateUserGlobalRoleErrorCandidatesTotalCountsWhatRemains asserts that
// when the global roles Rancher accepts outnumber candidateLimit, the error
// lists the first candidateLimit of them in list order and its total counts
// the accepted roles alone.
func TestCreateUserGlobalRoleErrorCandidatesTotalCountsWhatRemains(t *testing.T) {
	t.Parallel()
	var templates, globals []*unstructured.Unstructured
	var want []string
	for i := 1; i <= 12; i++ {
		open := fmt.Sprintf("cluster-role-%02d", i)
		locked := fmt.Sprintf("cluster-locked-%02d", i)
		templates = append(templates,
			roleTemplate(open, "cluster", nil),
			roleTemplate(locked, "cluster", map[string]any{"locked": true}))
		fine := fmt.Sprintf("global-role-%02d", i)
		globals = append(globals,
			inheritingRole(fine, fine, []any{open}),
			inheritingRole(fmt.Sprintf("global-locked-%02d", i), "Locked", []any{locked}))
		want = append(want, fine)
	}
	client := &fakeUserClient{roles: globals, roleTemplates: templates}
	ce := planNotFound(t, client, "zzz-none")
	assert.Equal(t, want[:candidateLimit], lockedCandidateIDs(ce))
	assert.Equal(t, 12, ce.CandidatesTotal)
}

// cancelOnTemplatesClient ends the caller's context while the role
// templates are listed, as an interrupt or a deadline does during the second
// list of the NOT_FOUND error.
type cancelOnTemplatesClient struct {
	*fakeUserClient
	cancel context.CancelFunc
}

func (c cancelOnTemplatesClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	if gvr.Resource == "roletemplates" {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.fakeUserClient.List(ctx, gvr, ns, selector)
}

// TestCreateUserGlobalRoleErrorEndsWithTheContextErrorDuringTheSecondList
// asserts that when the context ends while the role templates are listed for
// a NOT_FOUND error, create user returns the context error and not a
// NOT_FOUND with the degraded candidates.
func TestCreateUserGlobalRoleErrorEndsWithTheContextErrorDuringTheSecondList(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := cancelOnTemplatesClient{
		fakeUserClient: &fakeUserClient{roles: lockedCandidateGlobalRoles(), roleTemplates: lockedCandidateTemplates()},
		cancel:         cancel,
	}
	cmd, ok := NewCreate(stubUserFactory{c: client}, fakeSrc{body: []byte(testPassword)}).(runtime.MutatingCommand)
	require.True(t, ok)
	_, _, err := cmd.Plan(ctx, typedInput("zzz-none"))
	require.ErrorIs(t, err, context.Canceled)
	_, isClierr := errors.AsType[*clierr.Error](err)
	assert.False(t, isClierr, "want the context error, got %v", err)
}
