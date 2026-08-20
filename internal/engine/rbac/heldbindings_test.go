package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// crtbObj builds a raw ClusterRoleTemplateBinding. Any field left "" is
// omitted so the matcher sees it as genuinely absent, not an empty string.
func crtbObj(name, ns, userName, userPrincipal, groupPrincipal, role string) map[string]any {
	obj := map[string]any{
		"apiVersion":       "management.cattle.io/v3",
		"kind":             "ClusterRoleTemplateBinding",
		"metadata":         map[string]any{"name": name, "namespace": ns},
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

func heldPrtbObj(name, ns, projectName, userName, userPrincipal, groupPrincipal, role string) map[string]any {
	obj := crtbObj(name, ns, userName, userPrincipal, groupPrincipal, role)
	obj["kind"] = "ProjectRoleTemplateBinding"
	obj["projectName"] = projectName
	return obj
}

func grbObj(name, userName, userPrincipal, groupPrincipal, globalRole string) map[string]any {
	obj := map[string]any{
		"apiVersion":     "management.cattle.io/v3",
		"kind":           "GlobalRoleBinding",
		"metadata":       map[string]any{"name": name},
		"globalRoleName": globalRole,
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

func listOf(objs ...map[string]any) *unstructured.UnstructuredList {
	out := &unstructured.UnstructuredList{}
	for _, o := range objs {
		out.Items = append(out.Items, unstructured.Unstructured{Object: o})
	}
	return out
}

func heldFakeClient(resource string, list *unstructured.UnstructuredList) *fakeClient {
	return &fakeClient{listsByGVR: map[string]*unstructured.UnstructuredList{resource: list}}
}

func TestHeldGlobalBindingsDirectOnly(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-abc", "local://u-abc", "", "admin"),
		grbObj("grb-2", "u-other", "local://u-other", "", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "local://u-abc", rows[0].Principal)
	assert.Equal(t, "admin", rows[0].Role)
	assert.Equal(t, "grb-1", rows[0].Binding)
}

func TestHeldGlobalBindingsGroupOnly(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "", "", "okta_group://eng", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://eng"}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "okta_group://eng", rows[0].Principal)
}

func TestHeldGlobalBindingsGroupPrincipalNotOwnedIsUnmatched(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "", "", "okta_group://not-mine", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://eng"}, nil)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestHeldGlobalBindingsMixedDirectAndGroup(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-abc", "local://u-abc", "", "admin"),
		grbObj("grb-2", "", "", "okta_group://eng", "restricted-admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://eng"}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestHeldGlobalBindingsGrantShapedTwoFieldClassifiesWithPrincipal(t *testing.T) {
	// The common grant-created case: BOTH userName and userPrincipalName are
	// set. Classification is direct (via userName == id, the not-an-edge
	// common case), but the row principal must be the populated OWNED
	// principal, never the bare id.
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-abc", "okta_user://alice", "", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"okta_user://alice", "local://u-abc"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "okta_user://alice", rows[0].Principal)
}

func TestHeldGlobalBindingsConflictingFieldMalformedStaysDirect(t *testing.T) {
	// userName matches the user's own id (direct), but userPrincipalName
	// carries a value the user does not own — a malformed/multi-subject
	// binding. Direct wins (own field, own id); the row falls back to the
	// user's own principal rather than showing the unrelated value, and an
	// unrelated groupPrincipalName never redirects it to a group.
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-abc", "okta_user://someone-else", "okta_group://unrelated", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://unrelated"}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "local://u-abc", rows[0].Principal, "fallback to the own principal, never the unowned field")
}

func TestHeldGlobalBindingsMalformedEmptyPrincipalIDsStaysDirect(t *testing.T) {
	// A malformed user with no principalIds must still classify a userName
	// match as direct via the user id itself, and render inspection-safe
	// (no panic, no crash) with the user id as the row principal.
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-broken", "", "", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-broken", nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "u-broken", rows[0].Principal)
}

func TestHeldGlobalBindingsZeroBindingsEmptyNotNil(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf())
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	assert.NotNil(t, rows)
	assert.Empty(t, rows)
}

func TestHeldClusterBindingsGroupedByClusterID(t *testing.T) {
	c := heldFakeClient("clusterroletemplatebindings", listOf(
		crtbObj("crtb-1", "c-1", "u-abc", "local://u-abc", "", "cluster-owner"),
		crtbObj("crtb-2", "c-2", "u-abc", "local://u-abc", "", "cluster-member"),
		crtbObj("crtb-3", "c-1", "u-other", "local://u-other", "", "cluster-owner"),
	))
	groups, _, err := HeldClusterBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, groups, 2, "two clusters have a row; c-1's u-other row must not create an empty entry elsewhere")
	assert.Equal(t, "c-1", groups[0].ID)
	assert.Equal(t, "c-2", groups[1].ID)
	require.Len(t, groups[0].Rows, 1)
	assert.Equal(t, "cluster-owner", groups[0].Rows[0].Role)
}

func TestHeldClusterBindingsUnrelatedBindingNeverEmptyEntry(t *testing.T) {
	c := heldFakeClient("clusterroletemplatebindings", listOf(
		crtbObj("crtb-1", "c-1", "u-other", "local://u-other", "", "cluster-owner"),
	))
	groups, _, err := HeldClusterBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestHeldProjectBindingsGroupedByProjectName(t *testing.T) {
	c := heldFakeClient("projectroletemplatebindings", listOf(
		heldPrtbObj("prtb-1", "p-1", "c-1:p-1", "u-abc", "local://u-abc", "", "project-owner"),
		heldPrtbObj("prtb-2", "p-2", "c-1:p-2", "u-abc", "local://u-abc", "", "project-member"),
	))
	groups, _, err := HeldProjectBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "c-1:p-1", groups[0].ID, "grouping key is projectName, not the prtb's backing namespace")
	assert.Equal(t, "c-1:p-2", groups[1].ID)
}

func TestHeldProjectBindingsGroupHeldViaUserAttributesGroupPrincipal(t *testing.T) {
	c := heldFakeClient("projectroletemplatebindings", listOf(
		heldPrtbObj("prtb-1", "p-1", "c-1:p-1", "", "", "okta_group://eng", "project-member"),
	))
	groups, _, err := HeldProjectBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://eng"}, nil)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Rows, 1)
	assert.Equal(t, "okta_group://eng", groups[0].Rows[0].Principal)
}

func TestHeldBindingsRowsSortByPrincipalRoleBinding(t *testing.T) {
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-z", "", "", "okta_group://z", "admin"),
		grbObj("grb-a", "", "", "okta_group://a", "admin"),
	))
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", nil, []string{"okta_group://a", "okta_group://z"}, nil)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "okta_group://a", rows[0].Principal)
	assert.Equal(t, "okta_group://z", rows[1].Principal)
}

// TestHeldRowDisplayNameEnrichment makes sure held rows carry the same
// display-name enrichment the members sections get from the one shared
// MemberRow shape: the caller's map supplies the user's own name for a
// direct row and the cached group name for a group-held row; an absent
// entry leaves DisplayName blank, never a guess.
func TestHeldRowDisplayNameEnrichment(t *testing.T) {
	t.Parallel()
	c := heldFakeClient("globalrolebindings", listOf(
		grbObj("grb-1", "u-abc", "local://u-abc", "", "admin"),
		grbObj("grb-2", "", "", "okta_group://eng", "restricted-admin"),
	))
	names := map[string]string{"local://u-abc": "Alice", "okta_group://eng": "Engineering"}
	rows, err := HeldGlobalBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, []string{"okta_group://eng"}, names)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "Alice", rows[0].DisplayName)
	assert.Equal(t, "Engineering", rows[1].DisplayName)
}

// TestHeldProjectBindingsDropsEmptyScope makes sure a malformed prtb with no
// projectName never merges into a blank scope entry: the row is dropped and
// its binding name reported, while well-formed scopes are unaffected.
func TestHeldProjectBindingsDropsEmptyScope(t *testing.T) {
	t.Parallel()
	c := heldFakeClient("projectroletemplatebindings", listOf(
		heldPrtbObj("prtb-ok", "p-ns", "c-x:p-1", "u-abc", "local://u-abc", "", "project-member"),
		heldPrtbObj("prtb-broken", "p-ns2", "", "u-abc", "local://u-abc", "", "project-owner"),
	))
	groups, dropped, err := HeldProjectBindings(t.Context(), c, "u-abc", []string{"local://u-abc"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "c-x:p-1", groups[0].ID)
	assert.Equal(t, []string{"prtb-broken"}, dropped)
}
