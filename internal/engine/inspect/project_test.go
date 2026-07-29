package inspect

import (
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func projectFixtureObj(id, displayName, backingNamespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
		"spec": map[string]any{
			"displayName":   displayName,
			"resourceQuota": map[string]any{"limit": map[string]any{"pods": "10"}},
		},
	}}
	if backingNamespace != "" {
		_ = unstructured.SetNestedField(obj.Object, backingNamespace, "status", "backingNamespace")
	}
	return obj
}

func nsListItem(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}}}
}

func podListItem(ns, name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"containers": []any{}},
	}}
}

func deploymentListItem(ns, name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "namespace": ns}}}
}

func prtbListItem(user, roleTemplate string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":          map[string]any{"name": "prtb-" + user},
		"userPrincipalName": user,
		"roleTemplateName":  roleTemplate,
	}}
}

func setUpProjectFixture(t *testing.T, backingNamespace string) (*fakeFactory, *fakeClient, *fakeClient) {
	t.Helper()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	podKind := mustKind(t, "pod")
	depKind := mustKind(t, "deployment")
	prtbKind := mustKind(t, "projectroletemplatebinding")

	mgmt := newFakeClient()
	mgmt.withListNS(projectKind.GVR, "c-test", &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{*projectFixtureObj("p-1", "web", backingNamespace)},
	})
	mgmt.withObject(projectKind.GVR, "c-test", "p-1", projectFixtureObj("p-1", "web", backingNamespace))
	prtbNS := backingNamespace
	if prtbNS == "" {
		prtbNS = "p-1"
	}
	mgmt.withListNS(prtbKind.GVR, prtbNS, &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{prtbListItem("alice", "project-owner"), prtbListItem("bob", "project-member")},
	})

	downstream := newFakeClient()
	downstream.withList(nsKind.GVR, &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{nsListItem("ns-a"), nsListItem("ns-b")},
	})
	// Cluster-wide lists (ns=""), filtered client-side by membership — pins
	// the single-List-per-kind design (no more per-namespace List loop).
	downstream.withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podListItem("ns-a", "pod-a1"), podListItem("ns-b", "pod-b1"), podListItem("ns-b", "pod-b2"),
	}})
	downstream.withList(depKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		deploymentListItem("ns-a", "dep-a"),
	}})

	fac := &fakeFactory{perCluster: map[string]engine.ClusterClient{
		engine.LocalCluster: mgmt,
		"c-test":            downstream,
	}}
	return fac, mgmt, downstream
}

func TestInspectProjectAssemblesFullDocument(t *testing.T) {
	t.Parallel()
	fac, _, _ := setUpProjectFixture(t, "")
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	assert.Equal(t, "ProjectInspection", res.Kind)
	assert.Empty(t, res.Warnings)

	data, ok := res.Data.(projectResult)
	require.True(t, ok)
	assert.Equal(t, "p-1", data.ID)
	assert.Equal(t, "web", data.DisplayName)
	assert.Equal(t, "c-test", data.Cluster)

	require.Len(t, data.Namespaces, 2)
	assert.Equal(t, "ns-a", data.Namespaces[0].Name)
	assert.Equal(t, 1, data.Namespaces[0].Workloads.Pods)
	assert.Equal(t, 1, data.Namespaces[0].Workloads.Deployments)
	assert.Equal(t, "ns-b", data.Namespaces[1].Name)
	assert.Equal(t, 2, data.Namespaces[1].Workloads.Pods)
	assert.Equal(t, 0, data.Namespaces[1].Workloads.Deployments)

	require.Len(t, data.Members, 2)
	roleTemplates := map[string]string{}
	for _, m := range data.Members {
		roleTemplates[m.User] = m.RoleTemplate
	}
	assert.Equal(t, "project-owner", roleTemplates["alice"])
	assert.Equal(t, "project-member", roleTemplates["bob"])

	require.NotNil(t, data.ResourceQuota)
	require.NotNil(t, data.Usage)
}

func TestInspectProjectFallsBackToProjectIDNamespaceForMembers(t *testing.T) {
	t.Parallel()
	// backingNamespace absent → the prtb list must fall back to the
	// project-id namespace (fixture seeds prtbs there when passed "").
	fac, _, _ := setUpProjectFixture(t, "")
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)
	assert.Len(t, data.Members, 2)
}

func TestInspectProjectDegradesForbiddenMemberListToWarning(t *testing.T) {
	t.Parallel()
	fac, mgmt, _ := setUpProjectFixture(t, "")
	prtbKind := mustKind(t, "projectroletemplatebinding")
	mgmt.withListErr(prtbKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)
	assert.Empty(t, data.Members)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "projectroletemplatebindings")

	// everything else stays intact.
	assert.Equal(t, "p-1", data.ID)
	require.Len(t, data.Namespaces, 2)
	require.NotNil(t, data.ResourceQuota)
	require.NotNil(t, data.Usage)
}

func TestRenderProjectTextSections(t *testing.T) {
	t.Parallel()
	r := projectResult{
		ID: "p-cvrp4", DisplayName: "Default", Cluster: "c-phmj7",
		Namespaces: []projectNamespace{{Name: "default", Workloads: projectWorkloads{Pods: 3, Deployments: 1}}},
		Members: []projectMember{
			{User: "github_org://8341", RoleTemplate: "project-member", DisplayName: "DevOps Team"},
			{User: "local://u-alice", RoleTemplate: "project-owner", DisplayName: "Alice Ann"},
		},
		ResourceQuota: map[string]any{"limit": map[string]any{"pods": "10", "configMaps": "5"}},
		Usage:         &resourceTotals{CPURequest: "500m", CPULimit: "1", CPUUsage: "250m", MemoryRequest: "1Gi", MemoryLimit: "2Gi"},
	}
	text := renderProjectText(r)
	assert.Contains(t, text, "PROJECT\n  id: p-cvrp4\n  display: Default\n  cluster: c-phmj7\n")
	assert.Contains(t, text, "NAMESPACES\n  default (pods: 3, deployments: 1)\n")
	assert.Contains(t, text, "MEMBERS\n  DevOps Team (github_org://8341)  project-member\n  Alice Ann (local://u-alice)  project-owner\n")
	// quota keys render recursively sorted: configMaps before pods.
	assert.Contains(t, text, "QUOTA\n  limit:\n    configMaps: 5\n    pods: 10\n")
	// measured usage renders when present; absent measurements stay off the line.
	assert.Contains(t, text, "USAGE\n  cpu: request=500m limit=1 usage=250m\n  memory: request=1Gi limit=2Gi\n")
}

func TestRenderProjectTextEmptyAndUnavailable(t *testing.T) {
	t.Parallel()
	r := projectResult{ID: "p-x", DisplayName: "X", Cluster: "c-x", Members: nil}
	text := renderProjectText(r)
	assert.Contains(t, text, "NAMESPACES\n  none\n")
	assert.Contains(t, text, "MEMBERS\n  members: unavailable\n")
	assert.Contains(t, text, "QUOTA\n  none\n")
	// zero namespaces → verified-empty usage, NOT unavailable.
	assert.Contains(t, text, "USAGE\n  none (no namespaces)\n")

	r.Members = []projectMember{}
	assert.Contains(t, renderProjectText(r), "MEMBERS\n  none\n")

	// namespaces present but totals missing → genuinely unavailable.
	r.Namespaces = []projectNamespace{{Name: "ns1"}}
	assert.Contains(t, renderProjectText(r), "USAGE\n  usage: unavailable\n")
}

func TestProjectMemberJSONContract(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(projectMember{User: "local://u-a", RoleTemplate: "project-owner", DisplayName: "A"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"user":"local://u-a","roleTemplate":"project-owner","displayName":"A"}`, string(b))
	b, err = json.Marshal(projectMember{User: "local://u-a", RoleTemplate: "project-owner"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"user":"local://u-a","roleTemplate":"project-owner"}`, string(b))
}

// TestProjectMembersNullVsEmptyContract pins the WHOLE-payload encoding —
// the pre-existing non-omitempty field: nil slice → "members":null
// (unavailable), empty slice → "members":[] (verified empty).
func TestProjectMembersNullVsEmptyContract(t *testing.T) {
	t.Parallel()
	// nil slice → key present, value null.
	b, err := json.Marshal(projectResult{ID: "p", Members: nil})
	require.NoError(t, err)
	var nilDoc map[string]any
	require.NoError(t, json.Unmarshal(b, &nilDoc))
	raw, hasMembers := nilDoc["members"]
	require.True(t, hasMembers, "nil members: key must be present (non-omitempty)")
	assert.Nil(t, raw, "nil members: value must be null")

	// empty slice → key present, empty array.
	b, err = json.Marshal(projectResult{ID: "p", Members: []projectMember{}})
	require.NoError(t, err)
	var emptyDoc map[string]any
	require.NoError(t, json.Unmarshal(b, &emptyDoc))
	raw, hasMembers = emptyDoc["members"]
	require.True(t, hasMembers, "empty members: key must be present")
	slice, ok := raw.([]any)
	require.True(t, ok, "members value must be an array")
	assert.Empty(t, slice, "empty members: array must be empty")
}

func TestProjectMemberSystemJSONContract(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(projectMember{User: "system://x", RoleTemplate: "r", System: true})
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, true, doc["system"])

	b, err = json.Marshal(projectMember{User: "u-alice", RoleTemplate: "r"})
	require.NoError(t, err)
	doc = nil
	require.NoError(t, json.Unmarshal(b, &doc))
	_, has := doc["system"]
	assert.False(t, has, "zero System must omit the key")
}

func TestRenderProjectTextEscapeStripping(t *testing.T) {
	t.Parallel()
	r := projectResult{
		ID: "p-x", DisplayName: "X", Cluster: "c-x",
		Members: []projectMember{
			{User: "okta_user://evil", DisplayName: "Evil\x1b[2Jname", RoleTemplate: "project-member"},
		},
	}
	text := renderProjectText(r)
	assert.NotContains(t, text, "\x1b", "ANSI escape sequences from IdP display names must be stripped")
	assert.Contains(t, text, "Evil[2Jname", "printable chars around the escape are preserved")

	r.Members = []projectMember{
		{User: "okta_user://evil", DisplayName: "abc\u202edef\u200bghi", RoleTemplate: "project-member"},
	}
	text = renderProjectText(r)
	assert.NotContains(t, text, "\u202e", "bidi-override format characters must be stripped")
	assert.NotContains(t, text, "\u200b", "zero-width format characters must be stripped")
	assert.Contains(t, text, "abcdefghi", "visible runes around format characters are preserved")
}
