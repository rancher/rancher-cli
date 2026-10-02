package rbac

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// roleGet returns a role object as Rancher stores one, for seeding
// fakeClient.gets at key "/<name>" (empty namespace: roletemplates and
// globalroles are cluster-scoped). A roletemplate carries the context of
// the scope its name starts with ("project-member" is a project role),
// the field a binding is refused without; a globalrole ("admin") carries
// none.
func roleGet(name string) *unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": name}}
	if rtContext, _, _ := strings.Cut(name, "-"); rtContext == string(ScopeProject) || rtContext == string(ScopeCluster) {
		obj["context"] = rtContext
	}
	return &unstructured.Unstructured{Object: obj}
}

func prtbObj(name, userName, userPrincipal, groupPrincipal, role string) map[string]any {
	obj := map[string]any{
		"apiVersion":       "management.cattle.io/v3",
		"kind":             "ProjectRoleTemplateBinding",
		"metadata":         map[string]any{"name": name, "namespace": "p-ns"},
		"roleTemplateName": role,
	}
	if userName != "" {
		obj["userName"] = userName
	}
	if userPrincipal != "" {
		obj["userPrincipalName"] = userPrincipal
	}
	if groupPrincipal != "" {
		obj["groupPrincipalName"] = groupPrincipal
	}
	return obj
}

func diffPaths(entries []safety.DiffEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

// projectGetObj is the project object returned by BackingNamespace (no
// backingNamespace field → falls back to the project id as the namespace).
func projectGetObj(id string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
	}}
}

func planGrant(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "grant must be MutatingCommand")
	return mc.Plan(typedProjectContext(t, in), in)
}

func TestGrantProjectUserByID(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "projectroletemplatebindings/project-member-*", eff.Target)
	assert.True(t, eff.Destructive, "grant must be Destructive (requires --yes gate)")
	assert.Empty(t, eff.Warnings)

	require.NotNil(t, client.created)
	un, _, _ := unstructured.NestedString(client.created.Object, "userName")
	assert.Equal(t, "u-abc", un)
	upn, _, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.Equal(t, "local://u-abc", upn)
	rtn, _, _ := unstructured.NestedString(client.created.Object, "roleTemplateName")
	assert.Equal(t, "project-member", rtn)
	pn, _, _ := unstructured.NestedString(client.created.Object, "projectName")
	assert.Equal(t, "c-x:p-x", pn)
	gn, _, _ := unstructured.NestedString(client.created.Object, "metadata", "generateName")
	assert.Equal(t, "project-member-", gn)

	assert.Equal(t, []string{"/projectName", "/roleTemplateName", "/userName", "/userPrincipalName"}, diffPaths(eff.Diff))
	for _, e := range eff.Diff {
		assert.Nil(t, e.From)
	}
	diffByPath := map[string]any{}
	for _, e := range eff.Diff {
		diffByPath[e.Path] = e.To
	}
	assert.Equal(t, "u-abc", diffByPath["/userName"])
	assert.Equal(t, "local://u-abc", diffByPath["/userPrincipalName"])
	assert.Equal(t, "project-member", diffByPath["/roleTemplateName"])
	assert.Equal(t, "c-x:p-x", diffByPath["/projectName"])
}

func TestGrantClusterScope(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets:  map[string]*unstructured.Unstructured{"/cluster-owner": roleGet("cluster-owner")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "cluster-owner"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "clusterroletemplatebindings/cluster-owner-*", eff.Target)
	assert.Empty(t, eff.Warnings)

	require.NotNil(t, client.created)
	k, _, _ := unstructured.NestedString(client.created.Object, "kind")
	assert.Equal(t, "ClusterRoleTemplateBinding", k)
	cn, _, _ := unstructured.NestedString(client.created.Object, "clusterName")
	assert.Equal(t, "c-x", cn)
	ns, _, _ := unstructured.NestedString(client.created.Object, "metadata", "namespace")
	assert.Equal(t, "c-x", ns)

	assert.Equal(t, []string{"/clusterName", "/roleTemplateName", "/userName", "/userPrincipalName"}, diffPaths(eff.Diff))
	for _, e := range eff.Diff {
		assert.Nil(t, e.From)
	}
}

func TestGrantGlobalScope(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets:  map[string]*unstructured.Unstructured{"/admin": roleGet("admin")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"globalrolebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		bools:   map[string]bool{"global": true},
		strings: map[string]string{"role": "admin"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "globalrolebindings/grb-*", eff.Target)
	assert.Empty(t, eff.Warnings)

	require.NotNil(t, client.created)
	k, _, _ := unstructured.NestedString(client.created.Object, "kind")
	assert.Equal(t, "GlobalRoleBinding", k)
	grn, _, _ := unstructured.NestedString(client.created.Object, "globalRoleName")
	assert.Equal(t, "admin", grn)
	gn, _, _ := unstructured.NestedString(client.created.Object, "metadata", "generateName")
	assert.Equal(t, "grb-", gn)
	_, nsFound, _ := unstructured.NestedString(client.created.Object, "metadata", "namespace")
	assert.False(t, nsFound, "global scope: namespace must be absent from the binding object")

	assert.Equal(t, []string{"/globalRoleName", "/userName", "/userPrincipalName"}, diffPaths(eff.Diff))
	for _, e := range eff.Diff {
		assert.Nil(t, e.From)
	}
}

// generateNameFakeClient simulates server-side generateName resolution on
// top of fakeClient: the dry-run create returns the submitted object
// unchanged (a real server's dry-run response also carries a throwaway
// generated suffix, which is exactly why grant discards it for the preview
// identity - see eff.DisplayName below); the REAL (non-dry-run) create
// returns an object carrying committedName, standing in for the server's
// actual generateName assignment.
type generateNameFakeClient struct {
	*fakeClient
	committedName string
}

func (g *generateNameFakeClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if dryRun {
		return g.fakeClient.Create(ctx, gvr, ns, obj, dryRun)
	}
	committed := obj.DeepCopy()
	committed.SetName(g.committedName)
	return g.fakeClient.Create(ctx, gvr, ns, committed, dryRun)
}

// TestGrantCommittedIdentityNeverCarriesPreviewWildcard asserts the
// generateName rule end to end: the dry-run preview identity carries the
// "grb-*" wildcard (never a real name), but the COMMITTED success text
// derives from the real create's returned object and never carries "*".
func TestGrantCommittedIdentityNeverCarriesPreviewWildcard(t *testing.T) {
	t.Parallel()
	inner := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets:  map[string]*unstructured.Unstructured{"/admin": roleGet("admin")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"globalrolebindings": {},
		},
		allowCreate: true,
	}
	client := &generateNameFakeClient{fakeClient: inner, committedName: "grb-x7k"}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		bools:   map[string]bool{"global": true},
		strings: map[string]string{"role": "admin"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "globalrolebinding", eff.DisplayKind)
	assert.Equal(t, "grb-*", eff.DisplayName, "preview identity carries the wildcard")

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "globalrolebinding/grb-x7k created\n", res.Text)
	assert.NotContains(t, res.Text, "*", "committed identity must never carry the preview wildcard")
}

func TestGrantProvisioningWarnsAndPredictsID(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		// No users: the principal will not resolve to an existing user
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"okta_user://ghost"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	require.NotNil(t, client.created)
	_, unFound, _ := unstructured.NestedString(client.created.Object, "userName")
	assert.False(t, unFound, "provisioning path: userName must be absent from the created object")
	upn, _, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.Equal(t, "okta_user://ghost", upn)

	assert.Equal(t, []string{"/projectName", "/roleTemplateName", "/userPrincipalName"}, diffPaths(eff.Diff))
	for _, e := range eff.Diff {
		assert.Nil(t, e.From)
	}

	require.Len(t, eff.Warnings, 1)
	assert.Contains(t, eff.Warnings[0], ProvisionedUserID("okta_user://ghost"))
}

func TestGrantGroupPassthrough(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"okta_group://y"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	require.NotNil(t, client.created)
	gp, _, _ := unstructured.NestedString(client.created.Object, "groupPrincipalName")
	assert.Equal(t, "okta_group://y", gp)
	_, unFound, _ := unstructured.NestedString(client.created.Object, "userName")
	assert.False(t, unFound, "group path: userName must be absent")
	_, upnFound, _ := unstructured.NestedString(client.created.Object, "userPrincipalName")
	assert.False(t, upnFound, "group path: userPrincipalName must be absent")

	assert.Equal(t, []string{"/groupPrincipalName", "/projectName", "/roleTemplateName"}, diffPaths(eff.Diff))
	for _, e := range eff.Diff {
		assert.Nil(t, e.From)
	}
}

func TestGrantDuplicateConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        stubInput
		listsByGVR   map[string]*unstructured.UnstructuredList
		gets         map[string]*unstructured.Unstructured
		conflictName string
	}{
		{
			name: "project scope",
			input: stubInput{
				strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
				args:    []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"projectroletemplatebindings": {Items: []unstructured.Unstructured{
					{Object: prtbObj("pm-1", "u-abc", "", "", "project-member")},
				}},
			},
			gets: map[string]*unstructured.Unstructured{
				"c-x/p-x":         projectGetObj("p-x"),
				"/project-member": roleGet("project-member"),
			},
			conflictName: "pm-1",
		},
		{
			name: "cluster scope",
			input: stubInput{
				strings: map[string]string{"cluster": "c-x", "role": "cluster-owner"},
				args:    []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"clusterroletemplatebindings": {Items: []unstructured.Unstructured{
					{Object: map[string]any{
						"metadata":         map[string]any{"name": "co-1", "namespace": "c-x"},
						"userName":         "u-abc",
						"roleTemplateName": "cluster-owner",
					}},
				}},
			},
			gets:         map[string]*unstructured.Unstructured{"/cluster-owner": roleGet("cluster-owner")},
			conflictName: "co-1",
		},
		{
			name: "global scope",
			input: stubInput{
				bools:   map[string]bool{"global": true},
				strings: map[string]string{"role": "admin"},
				args:    []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"globalrolebindings": {Items: []unstructured.Unstructured{
					{Object: map[string]any{
						"metadata":       map[string]any{"name": "grb-1"},
						"userName":       "u-abc",
						"globalRoleName": "admin",
					}},
				}},
			},
			gets:         map[string]*unstructured.Unstructured{"/admin": roleGet("admin")},
			conflictName: "grb-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &fakeClient{
				users:      []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				listsByGVR: tt.listsByGVR,
				gets:       tt.gets,
			}
			cmd := NewGrant(fakeFactory{c: client})

			_, _, err := planGrant(t, cmd, tt.input)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.Contains(t, ce.Message, tt.conflictName)
		})
	}
}

func TestGrantDuplicateAliasConflict(t *testing.T) {
	t.Parallel()
	// User has BOTH principals. Bare-id resolution picks local://u-abc as
	// rs.Principal; the existing binding is principal-only under the OTHER
	// alias (okta_user://alice, userName absent). Grant by bare id u-abc must
	// still conflict because bindingMatchesSubject consults rs.PrincipalIDs.
	client := &fakeClient{
		users: []map[string]any{
			userObj("u-abc", "Alice", "alice", "local://u-abc", "okta_user://alice"),
		},
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {Items: []unstructured.Unstructured{
				{Object: prtbObj("pm-1", "", "okta_user://alice", "", "project-member")},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "pm-1")
	// Alias-only match: hint must name the exact principal stored in the
	// binding so the caller knows the correct string to pass to revoke.
	assert.Contains(t, ce.Hint, "okta_user://alice")
}

func TestGrantDuplicateForbiddenPassthrough(t *testing.T) {
	t.Parallel()
	forbidden := clierr.New(clierr.CodeRBAC, "projectroletemplatebindings is forbidden")
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{},
		listErrByGVR: map[string]error{
			"projectroletemplatebindings": forbidden,
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Nil(t, client.created, "forbidden list must not proceed to create")
}

func TestGrantStaleHashIDOccupiedConflict(t *testing.T) {
	t.Parallel()
	// A user was provisioned from okta_user://alice. Its principal has since
	// been replaced with okta_user://renamed. No other user owns
	// okta_user://alice. EnsureUser would derive the same hash id and get
	// AlreadyExists. Grant must return CodeConflict, not enter the provisioning
	// path and preview a binding whose reconciliation will always fail.
	requestedPrincipal := "okta_user://alice"
	id := ProvisionedUserID(requestedPrincipal)
	client := &fakeClient{
		users: []map[string]any{
			userObj(id, "Alice", "alice", "okta_user://renamed"),
		},
		gets: map[string]*unstructured.Unstructured{
			"c-x/p-x":         projectGetObj("p-x"),
			"/project-member": roleGet("project-member"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{requestedPrincipal},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, id, "conflict message must name the occupied user id")
	assert.Nil(t, client.created, "must not proceed to create on occupied-id conflict")
}

func TestGrantProjectRoleAtClusterScopeRejected(t *testing.T) {
	t.Parallel()
	// A project-context roletemplate IS found by Get but must be refused when
	// the requested scope is cluster. The error names both contexts and carries
	// the valid at-scope pool (the cluster-context roles alone: Rancher binds
	// no template whose context is another scope's or empty) so the caller
	// is guided to the right options.
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		gets: map[string]*unstructured.Unstructured{
			"/project-member": {Object: map[string]any{
				"metadata": map[string]any{"name": "project-member"},
				"context":  "project",
			}},
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"roletemplates": {Items: []unstructured.Unstructured{
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-owner"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-member"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "project-member"}, "context": "project"}},
				// An empty-context template is bound by no scope and is no candidate.
				{Object: map[string]any{"metadata": map[string]any{"name": "base-viewer"}, "context": ""}},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "project-member"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "project-member", "message must name the role")
	assert.Contains(t, ce.Message, "project", "message must name the role's context")
	assert.Contains(t, ce.Message, "cluster", "message must name the requested scope")
	assert.Contains(t, ce.Hint, "get roletemplates")
	// Candidates must be non-empty: scope-mismatch shows the valid at-scope pool.
	require.NotEmpty(t, ce.Candidates)
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	// Cluster-context roles appear.
	assert.Contains(t, ids, "cluster-owner")
	assert.Contains(t, ids, "cluster-member")
	// A template with no context is refused by Rancher at every scope.
	assert.NotContains(t, ids, "base-viewer")
	// Wrong-context (project) role must not appear.
	assert.NotContains(t, ids, "project-member")
}

// TestGrantEmptyContextRoleRejected asserts a roletemplate without a
// context is refused before the dry-run at both project and cluster
// scope, with a message naming the missing context and the scope, the
// usable at-scope pool as candidates, and the discovery hint - the
// refusal Rancher's webhook would give ("Unsupported value: \"\""),
// stated the CLI's way.
func TestGrantEmptyContextRoleRejected(t *testing.T) {
	t.Parallel()
	// One fake client per subtest: the fake is not safe for concurrent use.
	newClient := func() *fakeClient {
		return &fakeClient{
			users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
			gets: map[string]*unstructured.Unstructured{
				"c-x/p-x":      projectGetObj("p-x"),
				"/base-viewer": {Object: map[string]any{"metadata": map[string]any{"name": "base-viewer"}}},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"roletemplates": {Items: []unstructured.Unstructured{
					{Object: map[string]any{"metadata": map[string]any{"name": "cluster-owner"}, "context": "cluster"}},
					{Object: map[string]any{"metadata": map[string]any{"name": "project-member"}, "context": "project"}},
					{Object: map[string]any{"metadata": map[string]any{"name": "base-viewer"}}},
				}},
			},
		}
	}
	for _, tc := range []struct {
		scope string
		flags map[string]string
		want  string
	}{
		{"cluster", map[string]string{"cluster": "c-x", "role": "base-viewer"}, "cluster-owner"},
		{"project", map[string]string{"cluster": "c-x", "project": "p-x", "role": "base-viewer"}, "project-member"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			t.Parallel()
			cmd := NewGrant(fakeFactory{c: newClient()})
			_, _, err := planGrant(t, cmd, stubInput{strings: tc.flags, args: []string{"u-abc"}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Contains(t, ce.Message, "base-viewer has no context")
			assert.Contains(t, ce.Message, "whose context is "+tc.scope)
			assert.Contains(t, ce.Hint, "get roletemplates")
			var ids []string
			for _, c := range ce.Candidates {
				ids = append(ids, c.ID)
			}
			assert.Equal(t, []string{tc.want}, ids, "the pool holds the scope's context alone")
		})
	}
}

// TestGrantCheckRolePreflightOrder verifies that checkRole runs before the
// duplicate-binding list: if the role Get returns NOT_FOUND while the binding
// list contains a stale duplicate, NOT_FOUND wins - a grant of a nonexistent
// role cannot proceed regardless of the duplicate.
func TestGrantCheckRolePreflightOrder(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		// Role does not exist.
		getErrs: map[string]error{
			"cluster-owner": clierr.New(clierr.CodeNotFound, "not found: cluster-owner"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			// Stale duplicate binding exists in the list.
			"clusterroletemplatebindings": {Items: []unstructured.Unstructured{
				{Object: map[string]any{
					"metadata":         map[string]any{"name": "co-1", "namespace": "c-x"},
					"userName":         "u-abc",
					"roleTemplateName": "cluster-owner",
				}},
			}},
			"roletemplates": {},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "cluster-owner"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code, "role not found must win over stale duplicate binding")
	assert.Contains(t, ce.Message, "cluster-owner")
}

func TestGrantMissingRole(t *testing.T) {
	t.Parallel()
	client := &fakeClient{}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--role")
}

func TestGrantBadRoleAtClusterScope(t *testing.T) {
	t.Parallel()
	// "cluster-owne" is a realistic typo of "cluster-owner": the seeded
	// cluster-context roletemplate's name contains "cluster-owne" as a
	// substring so the candidate list is non-empty.
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"roletemplates": {Items: []unstructured.Unstructured{
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-owner"}, "context": "cluster", "displayName": "Cluster Owner"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-member"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "project-member"}, "context": "project"}},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "cluster-owne"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "cluster-owne")
	assert.Contains(t, ce.Hint, "get roletemplates")
	// candidates: cluster-context roletemplates whose names contain "cluster-owne"
	require.NotEmpty(t, ce.Candidates)
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Contains(t, ids, "cluster-owner")
	assert.NotContains(t, ids, "project-member", "project-context roles must be excluded from cluster-scope candidates")
}

func TestGrantBadRoleAtGlobalScope(t *testing.T) {
	t.Parallel()
	// "admi" is a realistic typo of "admin".
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"globalroles": {Items: []unstructured.Unstructured{
				{Object: map[string]any{"metadata": map[string]any{"name": "admin"}, "displayName": "Admin"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "restricted-admin"}}},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		bools:   map[string]bool{"global": true},
		strings: map[string]string{"role": "admi"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "admi")
	assert.Contains(t, ce.Hint, "get globalroles")
	require.NotEmpty(t, ce.Candidates)
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Contains(t, ids, "admin")
}

func TestGrantNoNearMatchRoleFallsBackToPool(t *testing.T) {
	t.Parallel()
	// "lx-no-such-role" shares no substring with any seeded role name, so
	// nearFilter returns zero candidates. The fallback must return the capped
	// at-scope pool so both role-error shapes always name valid options.
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"roletemplates": {Items: []unstructured.Unstructured{
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-owner"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-member"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "project-member"}, "context": "project"}},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "lx-no-such-role"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "lx-no-such-role")
	// No substring overlap with any role → fallback to the full at-scope pool.
	require.NotEmpty(t, ce.Candidates, "zero near-matches must fall back to the at-scope pool")
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	// Both cluster-context roles appear.
	assert.Contains(t, ids, "cluster-owner")
	assert.Contains(t, ids, "cluster-member")
	// project-context role excluded at cluster scope.
	assert.NotContains(t, ids, "project-member")
}

func TestGrantBadRoleTypoKeepsNarrowedCandidates(t *testing.T) {
	t.Parallel()
	// "cluster-owne" IS a substring of "cluster-owner" → nearFilter finds it;
	// the fallback must NOT replace the narrowed list with the full pool.
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"roletemplates": {Items: []unstructured.Unstructured{
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-owner"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "cluster-member"}, "context": "cluster"}},
				{Object: map[string]any{"metadata": map[string]any{"name": "project-member"}, "context": "project"}},
			}},
		},
	}
	cmd := NewGrant(fakeFactory{c: client})

	_, _, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "cluster-owne"},
		args:    []string{"u-abc"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	require.NotEmpty(t, ce.Candidates)
	var ids []string
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	// Typo-match narrows to the role whose name contains "cluster-owne".
	assert.Contains(t, ids, "cluster-owner")
	// "cluster-member" does not contain "cluster-owne".
	assert.NotContains(t, ids, "cluster-member")
}

func TestGrantRoleReadDegraded(t *testing.T) {
	t.Parallel()
	// A non-NotFound, non-cancellation error on the role Get must not block
	// the grant: an RBAC-restricted granter must not be blocked by an
	// unreadable role object. Plan must succeed (degraded validation).
	rbacErr := clierr.New(clierr.CodeRBAC, "roletemplates is forbidden")
	client := &fakeClient{
		users:   []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		getErrs: map[string]error{"cluster-owner": rbacErr},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings": {},
		},
		allowCreate: true,
	}
	cmd := NewGrant(fakeFactory{c: client})

	eff, exec, err := planGrant(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x", "role": "cluster-owner"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err, "non-NotFound role read error must degrade and allow the grant to proceed")
	require.NotNil(t, exec)
	assert.Equal(t, "clusterroletemplatebindings/cluster-owner-*", eff.Target)
}
