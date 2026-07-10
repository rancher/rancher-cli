package inspect

import (
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
