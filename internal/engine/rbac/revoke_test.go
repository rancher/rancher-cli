package rbac

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// revokeBinding builds a minimal binding object for revoke tests: name,
// optional namespace, resourceVersion, userName, userPrincipalName,
// groupPrincipalName, and role (stored under roleTemplateName or
// globalRoleName depending on the field name passed).
func revokeBinding(name, ns, rv, roleField, role, userName, userPrincipal, groupPrincipal string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"resourceVersion": rv,
		},
	}}
	if ns != "" {
		_ = unstructured.SetNestedField(obj.Object, ns, "metadata", "namespace")
	}
	if role != "" {
		_ = unstructured.SetNestedField(obj.Object, role, roleField)
	}
	if userName != "" {
		_ = unstructured.SetNestedField(obj.Object, userName, "userName")
	}
	if userPrincipal != "" {
		_ = unstructured.SetNestedField(obj.Object, userPrincipal, "userPrincipalName")
	}
	if groupPrincipal != "" {
		_ = unstructured.SetNestedField(obj.Object, groupPrincipal, "groupPrincipalName")
	}
	return obj
}

func planRevoke(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "revoke must be MutatingCommand")
	return mc.Plan(context.Background(), in)
}

func TestRevokeAllRolesAtScope(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("prtb-pm", "p-x", "rv-pm", "roleTemplateName", "project-member", "u-abc", "", ""),
				revokeBinding("prtb-po", "p-x", "rv-po", "roleTemplateName", "project-owner", "u-abc", "", ""),
				revokeBinding("prtb-other", "p-x", "rv-other", "roleTemplateName", "project-member", "u-xyz", "", ""),
			}},
			"projects": projectList("p-x"),
		},
		gets:        map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
		allowDelete: true,
	}
	cmd := NewRevoke(fakeFactory{c: client})

	eff, exec, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.True(t, eff.Destructive)
	assert.Contains(t, eff.Target, "(2)")
	assert.False(t, eff.ConflictDetection, "multi-binding revoke: no single rv precondition applies")
	require.Len(t, eff.Warnings, 1)
	assert.Equal(t, "2 bindings are deleted sequentially; on failure some may already be deleted", eff.Warnings[0])

	// Plan dry-runs every matched delete (prtb-pm and prtb-po, sorted).
	assert.Equal(t, []bool{true, true}, client.deleteDryRuns)

	res, err := exec(context.Background())
	require.NoError(t, err)
	// deletedNames: [dry-run pm, dry-run po] from Plan, then [real pm, real po] from exec.
	assert.Equal(t, []string{"prtb-pm", "prtb-po", "prtb-pm", "prtb-po"}, client.deletedNames)
	deleted, _ := res.Data.(map[string]any)["deleted"].([]string)
	assert.ElementsMatch(t, []string{"prtb-pm", "prtb-po"}, deleted)
}

func TestRevokeSingleRole(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("prtb-pm", "p-x", "rv-pm", "roleTemplateName", "project-member", "u-abc", "", ""),
				revokeBinding("prtb-po", "p-x", "rv-po", "roleTemplateName", "project-owner", "u-abc", "", ""),
				revokeBinding("prtb-other", "p-x", "rv-other", "roleTemplateName", "project-member", "u-xyz", "", ""),
			}},
			"projects": projectList("p-x"),
		},
		gets:        map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
		allowDelete: true,
	}
	cmd := NewRevoke(fakeFactory{c: client})

	eff, exec, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "projectroletemplatebindings/prtb-pm", eff.Target)
	assert.Equal(t, "rv-pm", eff.ResourceVersion)
	assert.True(t, eff.ConflictDetection, "single-binding revoke: rv precondition applies")
	assert.Empty(t, eff.Warnings, "no sequential-deletion warning for a single binding")

	_, err = exec(context.Background())
	require.NoError(t, err)
	// dry-run (Plan) + real delete (exec) both on prtb-pm only.
	assert.Equal(t, []string{"prtb-pm", "prtb-pm"}, client.deletedNames)
	assert.Equal(t, []string{"rv-pm", "rv-pm"}, client.deletedRVs)
}

func TestRevokeByPrincipalMatchesUserPrincipalName(t *testing.T) {
	t.Parallel()
	// Binding carries only userPrincipalName (no userName); no user object
	// exists. ResolveSubject provisions and bindingMatchesSubject matches on
	// the principal string.
	client := &fakeClient{
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("crtb-ghost", "c-x", "rv-ghost", "roleTemplateName", "cluster-member", "", "okta_user://ghost", ""),
			}},
		},
		allowDelete: true,
	}
	cmd := NewRevoke(fakeFactory{c: client})

	eff, exec, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x"},
		args:    []string{"okta_user://ghost"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "clusterroletemplatebindings/crtb-ghost", eff.Target)

	_, err = exec(context.Background())
	require.NoError(t, err)
	assert.Contains(t, client.deletedNames, "crtb-ghost")
}

func TestRevokeZeroMatchesListsHeldRoles(t *testing.T) {
	t.Parallel()
	// Subject holds project-member and project-owner but --role targets
	// a role not present. Error must list held roles in its hint.
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"projectroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("prtb-pm", "p-x", "rv-pm", "roleTemplateName", "project-member", "u-abc", "", ""),
				revokeBinding("prtb-po", "p-x", "rv-po", "roleTemplateName", "project-owner", "u-abc", "", ""),
			}},
			"projects": projectList("p-x"),
		},
		gets: map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
	}
	cmd := NewRevoke(fakeFactory{c: client})

	_, _, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "nonexistent-role"},
		args:    []string{"u-abc"},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "subject holds: project-member, project-owner", ce.Hint)
}

func TestRevokeZeroMatchesHintNamesRealCommand(t *testing.T) {
	t.Parallel()
	// No bindings exist at all. The hint must use the scoped form so the
	// operator inspects the same binding set revoke searched, and must use
	// bindKind.Name (singular), not bindKind.GVR.Resource (plural), because
	// only the singular is a valid rancher get argument.
	tests := []struct {
		name       string
		input      stubInput
		listsByGVR map[string]*unstructured.UnstructuredList
		gets       map[string]*unstructured.Unstructured
		wantHint   string
		wantNoPlur string
	}{
		{
			name: "cluster scope scoped hint",
			input: stubInput{
				strings: map[string]string{"cluster": "c-x"},
				args:    []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"clusterroletemplatebindings": {},
			},
			wantHint:   "run: rancher get clusterroletemplatebinding --cluster c-x -o json",
			wantNoPlur: "clusterroletemplatebindings",
		},
		{
			name: "project scope scoped hint",
			input: stubInput{
				strings: map[string]string{"project": "p-x", "cluster": "c-x"},
				args:    []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"projects":                    projectList("p-x"),
				"projectroletemplatebindings": {},
			},
			gets:       map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
			wantHint:   "run: rancher get projectroletemplatebinding --namespace p-x -o json",
			wantNoPlur: "projectroletemplatebindings",
		},
		{
			name: "global scope unqualified hint",
			input: stubInput{
				bools: map[string]bool{"global": true},
				args:  []string{"u-abc"},
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"globalrolebindings": {},
			},
			wantHint:   "run: rancher get globalrolebinding -o json",
			wantNoPlur: "globalrolebindings",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &fakeClient{
				users:      []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
				listsByGVR: tt.listsByGVR,
				gets:       tt.gets,
			}
			cmd := NewRevoke(fakeFactory{c: c})

			_, _, err := planRevoke(t, cmd, tt.input)
			require.Error(t, err)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, tt.wantHint, ce.Hint)
			assert.NotContains(t, ce.Hint, tt.wantNoPlur)
		})
	}
}

func TestRevokePartialFailure(t *testing.T) {
	t.Parallel()
	// Three matches; delete of the second (sorted: alpha < beta < gamma)
	// is forced to fail with a CodeConflict error. Plan must complete
	// (deleteErrs only fires on dryRun == false). Execute returns a
	// clierr with Deleted == [alpha], Failed.Name == beta, Unattempted == [gamma].
	conflictErr := clierr.New(clierr.CodeConflict, "rv conflict").WithRetryable(true)
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("crtb-alpha", "c-x", "rv-alpha", "roleTemplateName", "cluster-owner", "u-abc", "", ""),
				revokeBinding("crtb-beta", "c-x", "rv-beta", "roleTemplateName", "cluster-member", "u-abc", "", ""),
				revokeBinding("crtb-gamma", "c-x", "rv-gamma", "roleTemplateName", "cluster-reader", "u-abc", "", ""),
			}},
		},
		allowDelete: true,
		deleteErrs:  map[string]error{"crtb-beta": conflictErr},
	}
	cmd := NewRevoke(fakeFactory{c: client})

	_, exec, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x"},
		args:    []string{"u-abc"},
	})
	require.NoError(t, err, "Plan must succeed: dry-run deletes must not consult deleteErrs")
	require.NotNil(t, exec)

	// Plan dry-runs all three matches.
	assert.Equal(t, []bool{true, true, true}, client.deleteDryRuns)

	_, execErr := exec(context.Background())
	require.Error(t, execErr)

	var ce *clierr.Error
	require.ErrorAs(t, execErr, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.True(t, ce.Retryable, "partialFailure must hoist Retryable from the underlying error")
	assert.Contains(t, ce.Hint, "re-run")

	// Type assertion pins the Details shape.
	pf, ok := ce.Details.(RevokePartialFailure)
	require.True(t, ok, "Details must be RevokePartialFailure")
	assert.Equal(t, []string{"crtb-alpha"}, pf.Deleted)
	assert.Equal(t, "crtb-beta", pf.Failed.Name)
	assert.Equal(t, clierr.CodeConflict, pf.Failed.Code)
	assert.Equal(t, []string{"crtb-gamma"}, pf.Unattempted)

	// JSON decode pins the wire field names.
	data, marshalErr := json.Marshal(pf)
	require.NoError(t, marshalErr)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Contains(t, raw, "deleted")
	assert.Contains(t, raw, "failed")
	assert.Contains(t, raw, "unattempted")
}

func TestRevokeStaleHashIDOccupiedConflict(t *testing.T) {
	t.Parallel()
	// A user was provisioned as ProvisionedUserID("okta_user://alice").
	// Its principal has since been replaced with okta_user://renamed and no
	// other user owns okta_user://alice. Revoking okta_user://alice must
	// return CodeConflict (occupied-id, cannot provision) — not NOT_FOUND or
	// a silent match of the renamed user's bindings.
	requestedPrincipal := "okta_user://alice"
	id := ProvisionedUserID(requestedPrincipal)
	client := &fakeClient{
		users: []map[string]any{
			userObj(id, "Alice", "alice", "okta_user://renamed"),
		},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"clusterroletemplatebindings": {Items: []unstructured.Unstructured{
				revokeBinding("crtb-renamed", "c-x", "rv1", "roleTemplateName", "cluster-owner", id, "okta_user://renamed", ""),
			}},
		},
	}
	cmd := NewRevoke(fakeFactory{c: client})

	_, _, err := planRevoke(t, cmd, stubInput{
		strings: map[string]string{"cluster": "c-x"},
		args:    []string{requestedPrincipal},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code, "stale hash-id must return conflict, not match renamed user")
	assert.Contains(t, ce.Message, id, "conflict message must name the occupied user id")
}

func TestRevokeGlobalScope(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
		listsByGVR: map[string]*unstructured.UnstructuredList{
			"globalrolebindings": {Items: []unstructured.Unstructured{
				revokeBinding("grb-abc", "", "grb-rv", "globalRoleName", "admin", "u-abc", "", ""),
			}},
		},
		allowDelete: true,
	}
	cmd := NewRevoke(fakeFactory{c: client})

	eff, exec, err := planRevoke(t, cmd, stubInput{
		bools: map[string]bool{"global": true},
		args:  []string{"u-abc"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, "globalrolebindings/grb-abc", eff.Target)
	assert.Equal(t, "grb-rv", eff.ResourceVersion)
	assert.Equal(t, engine.LocalCluster, eff.Cluster)

	_, err = exec(context.Background())
	require.NoError(t, err)

	// Global scope: namespace is always "".
	for _, ns := range client.deletedNS {
		assert.Equal(t, "", ns, "global scope delete must use empty namespace")
	}
	assert.Contains(t, client.deletedNames, "grb-abc")
}
