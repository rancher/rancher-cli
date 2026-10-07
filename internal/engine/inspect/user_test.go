package inspect

import (
	"context"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func userGVR() (kinds.Kind, kinds.Kind, kinds.Kind, kinds.Kind, kinds.Kind) {
	uk, _ := kinds.Lookup("user")
	grb, _ := kinds.Lookup("globalrolebinding")
	crtb, _ := kinds.Lookup("clusterroletemplatebinding")
	prtb, _ := kinds.Lookup("projectroletemplatebinding")
	cluster, _ := kinds.Lookup("cluster")
	_ = cluster
	return uk, grb, crtb, prtb, cluster
}

func testUserObj(id, display, username string, principals ...string) *unstructured.Unstructured {
	pids := make([]any, len(principals))
	for i, p := range principals {
		pids[i] = p
	}
	obj := map[string]any{
		"apiVersion":   "management.cattle.io/v3",
		"kind":         "User",
		"metadata":     map[string]any{"name": id, "creationTimestamp": "2026-01-02T03:04:05Z"},
		"username":     username,
		"principalIds": pids,
	}
	if display != "" {
		obj["displayName"] = display
	}
	return &unstructured.Unstructured{Object: obj}
}

// newUserCmdFake is the command over c and a root that answers every steve
// cluster GET with 404.
func newUserCmdFake(c *fakeClient) *userCmd {
	return newUserCmdFakeRoot(c, &fakeRoot{})
}

func newUserCmdFakeRoot(c *fakeClient, root *fakeRoot) *userCmd {
	return &userCmd{clients: &fakeFactory{client: c}, roots: fakeRoots{rc: root}}
}

func TestInspectUserNeverLoggedInVerifiedEmpty(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r, ok := res.Data.(userResult)
	require.True(t, ok)
	assert.Equal(t, "u-abc", r.User.ID)
	require.NotNil(t, r.User.Groups)
	assert.Empty(t, *r.User.Groups, "never-logged-in user: verified-empty groups, never degraded")
	assert.Empty(t, r.User.LastLogin)
	assert.Empty(t, r.Degraded)
	require.NotNil(t, r.Roles.Global)
	assert.Empty(t, *r.Roles.Global)
	require.NotNil(t, r.Roles.Clusters)
	assert.Empty(t, *r.Roles.Clusters)
	require.NotNil(t, r.Roles.Projects)
	assert.Empty(t, *r.Roles.Projects)
	assert.Contains(t, res.Text, "ROLES")
	assert.Contains(t, res.Text, "\n  none\n")
}

func TestInspectUserUnreadableErrors(t *testing.T) {
	userKind, _, _, _, _ := userGVR()
	c := newFakeClient().withGetErr(userKind.GVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "no such user"))
	cmd := newUserCmdFake(c)
	_, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestInspectUserProjectListForbiddenDegradesOnlyThatSection(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, listOfCRTB(crtbObjForTest("crtb-1", "c-1", "u-abc", "local://u-abc", "", "cluster-owner"))).
		withListErr(prtbKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden"))

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)
	assert.Nil(t, r.Roles.Projects, "rolesProjects must be omitted, not an empty list")
	require.NotNil(t, r.Roles.Clusters)
	assert.Len(t, *r.Roles.Clusters, 1, "rolesClusters stays intact")
	require.Len(t, r.Degraded, 1)
	assert.Equal(t, "rolesProjects", r.Degraded[0].Section)
	assert.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Text, "PROJECT: unavailable")
}

// userWithClusterRoleClient is a client for u-abc holding one cluster role
// on c-1 and nothing else.
func userWithClusterRoleClient() *fakeClient {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	return newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, listOfCRTB(crtbObjForTest("crtb-1", "c-1", "u-abc", "local://u-abc", "", "cluster-owner"))).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})
}

// TestInspectUserClusterDisplayNameFromSteve asserts the display name of a
// cluster in the roles section is read with the steve GET by id, and never
// through the local client.
func TestInspectUserClusterDisplayNameFromSteve(t *testing.T) {
	_, _, _, _, clusterKind := userGVR()
	c := userWithClusterRoleClient()
	root := &fakeRoot{cluster: mgmtClusterFixture("c-1", "prod", "active", "")}

	cmd := newUserCmdFakeRoot(c, root)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)

	r, ok := res.Data.(userResult)
	require.True(t, ok)
	require.NotNil(t, r.Roles.Clusters)
	require.Len(t, *r.Roles.Clusters, 1)
	assert.Equal(t, "c-1", (*r.Roles.Clusters)[0].Cluster)
	assert.Equal(t, "prod", (*r.Roles.Clusters)[0].DisplayName)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters/c-1"}, root.clusterPaths)
	for _, k := range c.getCalls {
		assert.NotEqual(t, clusterKind.GVR, k.gvr, "the display name is never read through the local client")
	}
}

// TestInspectUserClusterDisplayNameUnreadableIsOmitted asserts a 403 or 404
// on the steve cluster GET leaves the display name out without failing the
// command or degrading the section.
func TestInspectUserClusterDisplayNameUnreadableIsOmitted(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := userWithClusterRoleClient()
			root := &fakeRoot{clusterErr: rootStatusErr(status, steveClusterPath("c-1"))}

			cmd := newUserCmdFakeRoot(c, root)
			res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
			require.NoError(t, err)

			r, ok := res.Data.(userResult)
			require.True(t, ok)
			require.NotNil(t, r.Roles.Clusters)
			require.Len(t, *r.Roles.Clusters, 1)
			assert.Equal(t, "c-1", (*r.Roles.Clusters)[0].Cluster)
			assert.Empty(t, (*r.Roles.Clusters)[0].DisplayName)
			assert.Empty(t, r.Degraded)
			assert.Empty(t, res.Warnings)
			assert.Equal(t, []string{"/v1/management.cattle.io.clusters/c-1"}, root.clusterPaths)
		})
	}
}

// TestInspectUserCanceledContextPropagatesFromClusterRead asserts that a
// context canceled during the steve cluster GET fails the command even
// though the GET's own error carries no cancellation sentinel, and that the
// project reads never start.
func TestInspectUserCanceledContextPropagatesFromClusterRead(t *testing.T) {
	_, _, _, prtbKind, _ := userGVR()
	c := userWithClusterRoleClient()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	root := newSteveRoot(t, nil)
	cmd := &userCmd{clients: &fakeFactory{client: c}, roots: fakeRoots{rc: cancelAfterRoot{RootClient: root, cancel: cancel}}}

	_, err := cmd.Execute(ctx, input{args: []string{"u-abc"}}, nil)
	require.Error(t, err)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters/c-1"}, root.requests())
	assert.False(t, c.listed(prtbKind.GVR), "project reads never start once the cluster read was canceled")
}

func TestInspectUserGroupsForbiddenBlocksAllThreeHeldSections(t *testing.T) {
	// A direct binding AND an otherwise-matching group binding are both
	// present; neither may render once groups degrade - held completeness
	// is unknowable without the user's group principals.
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeRBAC, "forbidden")).
		withList(grbKind.GVR, listOfGRB(grbObjForTest("grb-1", "u-abc", "local://u-abc", "", "admin"))).
		withList(crtbKind.GVR, listOfCRTB(crtbObjForTest("crtb-1", "c-1", "", "", "okta_group://eng", "cluster-member"))).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)
	assert.Nil(t, r.User.Groups)
	assert.Nil(t, r.Roles.Global)
	assert.Nil(t, r.Roles.Clusters)
	assert.Nil(t, r.Roles.Projects)
	sections := []string{}
	for _, d := range r.Degraded {
		sections = append(sections, d.Section)
	}
	assert.ElementsMatch(t, []string{"groups", "rolesGlobal", "rolesClusters", "rolesProjects"}, sections)
	assert.Contains(t, res.Text, "groups: unavailable")
	assert.Contains(t, res.Text, "lastLogin: unavailable")
	assert.Contains(t, res.Text, "GLOBAL: unavailable")
}

func TestInspectUserEnabledTriState(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	tests := []struct {
		name       string
		mutate     func(*unstructured.Unstructured)
		wantNil    bool
		wantVal    bool
		wantMarker bool
	}{
		{name: "absent means enabled", mutate: func(*unstructured.Unstructured) {}, wantNil: true},
		{name: "explicit true", mutate: func(u *unstructured.Unstructured) { u.Object["enabled"] = true }, wantVal: true},
		{name: "explicit false", mutate: func(u *unstructured.Unstructured) { u.Object["enabled"] = false }, wantVal: false, wantMarker: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := testUserObj("u-abc", "Jane", "jane", "local://u-abc")
			tt.mutate(u)
			c := newFakeClient().
				withObject(userKind.GVR, "", "u-abc", u).
				withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
				withList(grbKind.GVR, &unstructured.UnstructuredList{}).
				withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
				withList(prtbKind.GVR, &unstructured.UnstructuredList{})
			cmd := newUserCmdFake(c)
			res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
			require.NoError(t, err)
			r := res.Data.(userResult)
			if tt.wantNil {
				assert.Nil(t, r.User.Enabled)
				assert.Contains(t, res.Text, "enabled: true", "the card states the effective value even when the wire omits the field")
				assert.NotContains(t, res.Text, "[disabled]")
				return
			}
			require.NotNil(t, r.User.Enabled)
			assert.Equal(t, tt.wantVal, *r.User.Enabled)
			if tt.wantMarker {
				assert.Contains(t, res.Text, "enabled: false")
				assert.Contains(t, res.Text, "[disabled]")
			} else {
				assert.Contains(t, res.Text, "enabled: true")
				assert.NotContains(t, res.Text, "[disabled]")
			}
		})
	}
}

func TestInspectUserGroupsMultiProviderAndDuplicatePrincipal(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	ua := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "UserAttribute",
		"metadata":   map[string]any{"name": "u-abc"},
		"GroupPrincipals": map[string]any{
			"okta": map[string]any{"Items": []any{
				map[string]any{"metadata": map[string]any{"name": "okta_group://eng"}, "displayName": "Engineering"},
			}},
			"azure": map[string]any{"Items": []any{
				// Same principal string cached by a second provider: retained
				// as a distinct row, never merged.
				map[string]any{"metadata": map[string]any{"name": "okta_group://eng"}, "displayName": "Engineering (azure cache)"},
			}},
		},
		"lastLogin": "2026-08-01T00:00:00Z",
	}}
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withObject(rbac.UserAttrGVR, "", "u-abc", ua).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)
	require.NotNil(t, r.User.Groups)
	require.Len(t, *r.User.Groups, 2, "duplicate principal under two providers is retained, not merged")
	assert.Equal(t, "2026-08-01T00:00:00Z", r.User.LastLogin)
}

// TestInspectUserAttributesAllFields exercises every user-facing
// UserAttribute field on one read and asserts the text card distributes
// them by meaning: lastLogin/disableAfter/deleteAfter/extra in the USER
// block, lastRefresh/needsRefresh in GROUPS.
func TestInspectUserAttributesAllFields(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	ua := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "UserAttribute",
		"metadata":   map[string]any{"name": "u-abc"},
		"GroupPrincipals": map[string]any{
			"okta": map[string]any{"Items": []any{
				map[string]any{"metadata": map[string]any{"name": "okta_group://eng"}, "displayName": "Engineering"},
			}},
		},
		"lastLogin":    "2026-08-01T00:00:00Z",
		"LastRefresh":  "2026-08-02T00:00:00Z",
		"NeedsRefresh": false,
		"disableAfter": "720h0m0s",
		"deleteAfter":  "1440h0m0s",
		"ExtraByProvider": map[string]any{
			"okta": map[string]any{
				"principalid": []any{"okta_user://jane@example.com"},
				"username":    []any{"jane@example.com"},
			},
			"local": map[string]any{
				"principalid": []any{"local://u-abc"},
				"username":    []any{"jane"},
			},
		},
	}}
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withObject(rbac.UserAttrGVR, "", "u-abc", ua).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)

	assert.Equal(t, "2026-08-01T00:00:00Z", r.User.LastLogin)
	assert.Equal(t, "2026-08-02T00:00:00Z", r.User.LastRefresh)
	require.NotNil(t, r.User.NeedsRefresh)
	assert.False(t, *r.User.NeedsRefresh)
	assert.Equal(t, "720h0m0s", r.User.DisableAfter)
	assert.Equal(t, "1440h0m0s", r.User.DeleteAfter)
	require.Contains(t, r.User.Extra, "okta")
	assert.Equal(t, []string{"jane@example.com"}, r.User.Extra["okta"]["username"])
	require.Contains(t, r.User.Extra, "local")
	assert.Equal(t, []string{"local://u-abc"}, r.User.Extra["local"]["principalid"])

	userBlock, rest, found := strings.Cut(res.Text, "\nGROUPS\n")
	require.True(t, found)
	groupsBlock, _, _ := strings.Cut(rest, "\nROLES\n")
	assert.Contains(t, userBlock, "lastLogin: 2026-08-01T00:00:00Z")
	assert.Contains(t, userBlock, "disableAfter: 720h0m0s")
	assert.Contains(t, userBlock, "deleteAfter: 1440h0m0s")
	assert.Contains(t, userBlock, "extra[local]: principalid=local://u-abc  username=jane")
	assert.Contains(t, userBlock, "extra[okta]: principalid=okta_user://jane@example.com  username=jane@example.com")
	assert.Contains(t, groupsBlock, "lastRefresh: 2026-08-02T00:00:00Z")
	assert.Contains(t, groupsBlock, "needsRefresh: false")
	assert.NotContains(t, groupsBlock, "lastLogin", "lastLogin renders in the USER block, never GROUPS")
}

// TestInspectUserAttributesAbsentStayAbsent asserts a never-logged-in
// user (verified-empty UserAttributes) omits every attribute field on the
// wire and prints none of them.
func TestInspectUserAttributesAbsentStayAbsent(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
		withList(prtbKind.GVR, &unstructured.UnstructuredList{})

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)
	assert.Empty(t, r.User.LastRefresh)
	assert.Nil(t, r.User.NeedsRefresh)
	assert.Empty(t, r.User.DisableAfter)
	assert.Empty(t, r.User.DeleteAfter)
	assert.Nil(t, r.User.Extra)
	assert.NotContains(t, res.Text, "lastRefresh")
	assert.NotContains(t, res.Text, "needsRefresh")
	assert.NotContains(t, res.Text, "disableAfter")
	assert.NotContains(t, res.Text, "deleteAfter")
	assert.NotContains(t, res.Text, "extra[")
	assert.NotContains(t, res.Text, "unavailable")
}

func TestInspectUserCancellationDuringClusterEnrichmentAbortsBeforeProjects(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	ctx, cancel := context.WithCancel(t.Context())
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, listOfCRTB(crtbObjForTest("crtb-1", "c-1", "u-abc", "local://u-abc", "", "cluster-owner"))).
		withList(prtbKind.GVR, listOfPRTB(prtbObjForTest("prtb-1", "p-1", "c-1:p-1", "u-abc", "local://u-abc", "", "project-owner")))
	cancel()

	cmd := newUserCmdFakeRoot(c, &fakeRoot{clusterErr: context.Canceled})
	_, err := cmd.Execute(ctx, input{args: []string{"u-abc"}}, nil)
	require.Error(t, err)
	assert.False(t, c.listed(prtbKind.GVR), "project enrichment/list must never run once cluster enrichment aborted")
}

// TestInspectUserNoEffectiveVocabulary asserts the retired vocabulary
// stays retired: roles are facts about bindings, permissions are the
// server's answer - neither "effective" nor the old "held" qualifier
// may reappear in the command's user-facing strings.
func TestInspectUserNoEffectiveVocabulary(t *testing.T) {
	cmd := newUserCmdFake(newFakeClient())
	desc := cmd.Describe()
	for _, banned := range []string{"effective", "held"} {
		assert.NotContains(t, strings.ToLower(desc.Desc), banned)
		assert.NotContains(t, strings.ToLower(desc.Usage), banned)
	}
}

func crtbObjForTest(name, ns, userName, userPrincipal, groupPrincipal, role string) *unstructured.Unstructured {
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
	return &unstructured.Unstructured{Object: obj}
}

func prtbObjForTest(name, ns, projectName, userName, userPrincipal, groupPrincipal, role string) *unstructured.Unstructured {
	u := crtbObjForTest(name, ns, userName, userPrincipal, groupPrincipal, role)
	u.Object["kind"] = "ProjectRoleTemplateBinding"
	u.Object["projectName"] = projectName
	return u
}

func grbObjForTest(name, userName, userPrincipal, groupPrincipal, globalRole string) *unstructured.Unstructured {
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
	return &unstructured.Unstructured{Object: obj}
}

func listOfCRTB(items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	out := &unstructured.UnstructuredList{}
	for _, i := range items {
		out.Items = append(out.Items, *i)
	}
	return out
}

func listOfPRTB(items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	return listOfCRTB(items...)
}
func listOfGRB(items ...*unstructured.Unstructured) *unstructured.UnstructuredList {
	return listOfCRTB(items...)
}

// TestInspectUserMalformedProjectBindingSurfacesWarning asserts the composite
// end of the blank-scope drop: a matched prtb with no projectName never
// renders (no blank PROJECT heading), and the omission reaches the caller
// as a named warning - losing the collectRoles wiring must fail here, not
// only in the rbac unit test.
func TestInspectUserMalformedProjectBindingSurfacesWarning(t *testing.T) {
	userKind, grbKind, crtbKind, prtbKind, _ := userGVR()
	project, _ := kinds.Lookup("project")
	c := newFakeClient().
		withObject(userKind.GVR, "", "u-abc", testUserObj("u-abc", "Jane", "jane", "local://u-abc")).
		withGetErr(rbac.UserAttrGVR, "", "u-abc", clierr.New(clierr.CodeNotFound, "not found")).
		withList(grbKind.GVR, &unstructured.UnstructuredList{}).
		withList(crtbKind.GVR, &unstructured.UnstructuredList{}).
		withList(prtbKind.GVR, listOfPRTB(
			prtbObjForTest("prtb-ok", "p-1", "c-1:p-1", "u-abc", "local://u-abc", "", "project-owner"),
			prtbObjForTest("prtb-broken", "p-2", "", "u-abc", "local://u-abc", "", "project-member"),
		)).
		withGetErr(project.GVR, "c-1", "p-1", clierr.New(clierr.CodeNotFound, "no display"))

	cmd := newUserCmdFake(c)
	res, err := cmd.Execute(t.Context(), input{args: []string{"u-abc"}}, nil)
	require.NoError(t, err)
	r := res.Data.(userResult)
	require.NotNil(t, r.Roles.Projects)
	require.Len(t, *r.Roles.Projects, 1, "only the well-formed project renders")
	assert.Equal(t, "c-1:p-1", (*r.Roles.Projects)[0].Project)
	assert.Contains(t, res.Warnings, "1 project role binding(s) with no scope id omitted: prtb-broken")
	assert.NotContains(t, res.Text, "PROJECT \n", "no blank project heading")
}

// TestStyleUserTextStripInvariant asserts inspect user's bold spans: the
// dynamic USER header (by prefix, since it carries [system]/[disabled]
// qualifiers), the GROUPS/ROLES headers (exact), and each dynamic role
// sub-header (by prefix, since it carries a scope label) - all bold, whole
// line - plus the strip invariant and the inactive-style identity.
func TestStyleUserTextStripInvariant(t *testing.T) {
	t.Parallel()
	disabled := false
	r := userResult{
		User: userIdentity{ID: "u-1", Username: "alice", System: true, Enabled: &disabled},
		Roles: rolesResult{
			Global: &[]rbac.MemberRow{{Principal: "local://u-1", Role: "admin", Binding: "grb-1"}},
			Clusters: &[]rolesCluster{{Cluster: "c-1", DisplayName: "prod", Roles: []rbac.MemberRow{
				{Principal: "local://u-1", Role: "cluster-owner", Binding: "crtb-1"},
			}}},
			Projects: &[]rolesProject{{Project: "p-1", Roles: []rbac.MemberRow{
				{Principal: "local://u-1", Role: "project-member", Binding: "prtb-1"},
			}}},
		},
	}
	own := map[string]bool{"local://u-1": true}
	text := renderUserText(r, own)
	require.Contains(t, text, "USER [system] [disabled]\n")

	enabled := style.Style{Enabled: true}
	styled := styleUserText(text, enabled)
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleUserText(text, style.Style{}), "disabled style returns text unchanged")

	assert.Contains(t, styled, "\x1b[1mUSER [system] [disabled]\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1mGROUPS\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1mROLES\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1m  GLOBAL\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1m  CLUSTER prod (c-1)\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1m  PROJECT p-1\x1b[0m\n")
	assert.NotContains(t, styled, "\x1b[1m    admin", "a role row must not be styled")
}

// TestStyleUserTextGroupRowNeverTakesSubheaderBold asserts the role
// sub-header prefixes are scoped below the ROLES header: a GROUPS row whose
// provider-owned display name begins with a sub-header word (GLOBAL-ADMINS)
// stays plain, while the real GLOBAL sub-header below ROLES is bold.
func TestStyleUserTextGroupRowNeverTakesSubheaderBold(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"USER [system]",
		"  id: u-x",
		"",
		"GROUPS",
		"  GLOBAL-ADMINS (activedirectory)  [ad_group://cn=ga]",
		"",
		"ROLES",
		"  GLOBAL",
		"    admin  [grb-x]",
	}, "\n") + "\n"
	styled := styleUserText(text, style.Style{Enabled: true})
	assert.Equal(t, text, style.Strip(styled))
	assert.Contains(t, styled, "\x1b[1mUSER [system]\x1b[0m")
	assert.Contains(t, styled, "  GLOBAL-ADMINS (activedirectory)  [ad_group://cn=ga]\n", "the group row stays plain")
	assert.NotContains(t, styled, "\x1b[1m  GLOBAL-ADMINS")
	assert.Contains(t, styled, "\x1b[1m  GLOBAL\x1b[0m\n", "the real sub-header below ROLES is bold")
}
