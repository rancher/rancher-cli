package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
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
	// Cluster-wide lists (ns=""), filtered client-side by membership - asserts
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
	roles := map[string]string{}
	bindings := map[string]string{}
	for _, m := range data.Members {
		roles[m.Principal] = m.Role
		bindings[m.Principal] = m.Binding
	}
	assert.Equal(t, "project-owner", roles["alice"])
	assert.Equal(t, "project-member", roles["bob"])
	// binding round-trips the seeded CRD object name (prtbListItem sets
	// metadata.name to "prtb-"+user).
	assert.Equal(t, "prtb-alice", bindings["alice"])
	assert.Equal(t, "prtb-bob", bindings["bob"])

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
	assert.Equal(t, "members unavailable: forbidden", res.Warnings[0])
	assert.Equal(t, []sectionOutcome{{Section: sectionMembers, State: stateUnavailable, Code: clierr.CodeRBAC}}, data.Degraded)

	// everything else stays intact.
	assert.Equal(t, "p-1", data.ID)
	require.Len(t, data.Namespaces, 2)
	require.NotNil(t, data.ResourceQuota)
	require.NotNil(t, data.Usage)
}

// TestInspectProjectMemberShapedAccess is the anchor case: a project member
// passes the project GET and the PRTB list but cannot list namespaces. The
// command succeeds with the readable sections and annotates the rest.
func TestInspectProjectMemberShapedAccess(t *testing.T) {
	t.Parallel()
	fac, _, downstream := setUpProjectFixture(t, "")
	nsKind := mustKind(t, "namespace")
	podKind := mustKind(t, "pod")
	depKind := mustKind(t, "deployment")
	downstream.withListErr(nsKind.GVR, clierr.New(clierr.CodeRBAC, "namespaces is forbidden"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	assert.Equal(t, "p-1", data.ID)
	assert.Equal(t, "web", data.DisplayName)
	require.Len(t, data.Members, 2)
	require.NotNil(t, data.ResourceQuota)
	assert.Nil(t, data.Namespaces)
	assert.Nil(t, data.Usage)
	assert.Equal(t, []sectionOutcome{
		{Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeRBAC},
		{Section: sectionUsage, State: stateUnavailable, Code: clierr.CodeRBAC},
	}, data.Degraded)
	assert.Equal(t, []string{
		"namespaces unavailable: namespaces is forbidden",
		"usage unavailable: namespaces unknown (namespaces is forbidden)",
	}, res.Warnings)

	// A lost membership set makes the workload lists meaningless; they are
	// not issued.
	assert.False(t, downstream.listed(podKind.GVR), "pod list must not run without a membership set")
	assert.False(t, downstream.listed(depKind.GVR), "deployment list must not run without a membership set")

	// The wire shape an agent sees: identity and members carry values, the
	// lost sections are null, and degraded names them.
	b, err := json.Marshal(data)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	nsRaw, has := doc["namespaces"]
	require.True(t, has, "namespaces key must be present")
	assert.Nil(t, nsRaw, "unavailable namespaces must serialize as null")
	usageRaw, has := doc["usage"]
	require.True(t, has, "usage key must be present")
	assert.Nil(t, usageRaw, "unavailable usage must serialize as null")
	members, ok := doc["members"].([]any)
	require.True(t, ok, "members must be an array")
	assert.Len(t, members, 2)
	assert.Len(t, doc["degraded"], 2)

	// Text card marks both sections in place.
	assert.Contains(t, res.Text, "NAMESPACES\n  namespaces: unavailable\n")
	assert.Contains(t, res.Text, "USAGE\n  usage: unavailable\n")
}

func TestInspectProjectMembershipAndMembersBothDenied(t *testing.T) {
	t.Parallel()
	fac, mgmt, downstream := setUpProjectFixture(t, "")
	nsKind := mustKind(t, "namespace")
	prtbKind := mustKind(t, "projectroletemplatebinding")
	downstream.withListErr(nsKind.GVR, clierr.New(clierr.CodeRBAC, "namespaces is forbidden"))
	mgmt.withListErr(prtbKind.GVR, clierr.New(clierr.CodeRBAC, "prtbs is forbidden"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	assert.Nil(t, data.Namespaces)
	assert.Nil(t, data.Members)
	assert.Nil(t, data.Usage)
	assert.Equal(t, []sectionOutcome{
		{Section: sectionMembers, State: stateUnavailable, Code: clierr.CodeRBAC},
		{Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeRBAC},
		{Section: sectionUsage, State: stateUnavailable, Code: clierr.CodeRBAC},
	}, data.Degraded)
	assert.Len(t, res.Warnings, 3)
}

func TestInspectProjectDownstreamClientFailureMatchesMembershipFailure(t *testing.T) {
	t.Parallel()
	fac, _, _ := setUpProjectFixture(t, "")
	fac.perClusterErr = map[string]error{"c-test": clierr.New(clierr.CodeGeneric, "no kubeconfig for c-test")}
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	assert.Nil(t, data.Namespaces)
	assert.Nil(t, data.Usage)
	require.Len(t, data.Members, 2, "the management plane is still readable")
	assert.Equal(t, []sectionOutcome{
		{Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeGeneric},
		{Section: sectionUsage, State: stateUnavailable, Code: clierr.CodeGeneric},
	}, data.Degraded)
}

func TestInspectProjectPodListDeniedBlocksUsage(t *testing.T) {
	t.Parallel()
	fac, _, downstream := setUpProjectFixture(t, "")
	podKind := mustKind(t, "pod")
	depKind := mustKind(t, "deployment")
	downstream.withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "pods is forbidden"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	assert.Nil(t, data.Namespaces, "unknown pod counts cannot be rendered as zeroes")
	assert.Nil(t, data.Usage, "usage aggregates pods")
	require.Len(t, data.Members, 2)
	assert.Equal(t, []sectionOutcome{
		{Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeRBAC},
		{Section: sectionUsage, State: stateUnavailable, Code: clierr.CodeRBAC},
	}, data.Degraded)
	assert.Equal(t, "usage unavailable: pods unknown (pods is forbidden)", res.Warnings[1])
	// The deployment list runs concurrently with the pod list and SUCCEEDS
	// here - a readable deployment list must not restore a namespace section
	// whose pod counts are unknown.
	assert.True(t, downstream.listed(depKind.GVR), "the lists fan out together")
}

func TestInspectProjectDeploymentListDeniedKeepsUsage(t *testing.T) {
	t.Parallel()
	fac, _, downstream := setUpProjectFixture(t, "")
	depKind := mustKind(t, "deployment")
	downstream.withListErr(depKind.GVR, clierr.New(clierr.CodeRBAC, "deployments is forbidden"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	assert.Nil(t, data.Namespaces)
	require.NotNil(t, data.Usage, "pods were readable, so the totals stand")
	require.Len(t, data.Members, 2)
	assert.Equal(t, []sectionOutcome{
		{Section: sectionNamespaces, State: stateUnavailable, Code: clierr.CodeRBAC},
	}, data.Degraded)
	assert.Equal(t, []string{"namespaces unavailable: deployments is forbidden"}, res.Warnings)
}

func TestInspectProjectMetricsOnlyFailureKeepsUsage(t *testing.T) {
	t.Parallel()
	fac, _, downstream := setUpProjectFixture(t, "")
	downstream.withListErr(podMetricsGVR, errors.New("no metrics server"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	require.Len(t, data.Namespaces, 2, "requests and limits come from the pod specs")
	require.NotNil(t, data.Usage)
	assert.Equal(t, "0", data.Usage.CPURequest)
	assert.Empty(t, data.Usage.CPUUsage, "no measurement, but the totals stand")
	assert.Equal(t, []sectionOutcome{
		{Section: sectionMetrics, State: stateUnavailable, Code: clierr.CodeGeneric},
	}, data.Degraded)
	assert.Equal(t, []string{"metrics unavailable: no metrics server"}, res.Warnings)
}

// TestInspectProjectZeroMembersSkipsWorkloadLists covers the verified-empty
// short-circuit: membership was read and is empty, so the workload and
// metrics reads that could not have contributed rows are never issued - a
// denial on them must not turn a verified-empty project into a failure.
func TestInspectProjectZeroMembersSkipsWorkloadLists(t *testing.T) {
	t.Parallel()
	fac, _, downstream := setUpProjectFixture(t, "")
	nsKind := mustKind(t, "namespace")
	podKind := mustKind(t, "pod")
	depKind := mustKind(t, "deployment")
	downstream.withList(nsKind.GVR, &unstructured.UnstructuredList{})
	downstream.withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "pods is forbidden"))
	downstream.withListErr(depKind.GVR, clierr.New(clierr.CodeRBAC, "deployments is forbidden"))
	downstream.withListErr(podMetricsGVR, errors.New("no metrics server"))
	cmd := NewInspectProject(fac)

	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	require.NotNil(t, data.Namespaces, "a read membership set is verified empty, not unavailable")
	assert.Empty(t, data.Namespaces)
	assert.Nil(t, data.Usage)
	assert.Empty(t, data.Degraded)
	assert.Empty(t, res.Warnings)
	assert.False(t, downstream.listed(podKind.GVR), "pod list must be skipped")
	assert.False(t, downstream.listed(depKind.GVR), "deployment list must be skipped")
	assert.False(t, downstream.listed(podMetricsGVR), "metrics list must be skipped")
	assert.Contains(t, res.Text, "USAGE\n  none (no namespaces)\n")
}

func TestInspectProjectCancellationStaysHard(t *testing.T) {
	t.Parallel()
	nsKind := mustKind(t, "namespace")
	prtbKind := mustKind(t, "projectroletemplatebinding")
	podKind := mustKind(t, "pod")

	for _, tc := range []struct {
		name string
		set  func(mgmt, downstream *fakeClient)
	}{
		{"membership", func(_, downstream *fakeClient) {
			downstream.withListErr(nsKind.GVR, context.Canceled)
		}},
		{"members", func(mgmt, _ *fakeClient) {
			mgmt.withListErr(prtbKind.GVR, context.Canceled)
		}},
		{"pods", func(_, downstream *fakeClient) {
			downstream.withListErr(podKind.GVR, context.Canceled)
		}},
		{"metrics", func(_, downstream *fakeClient) {
			downstream.withListErr(podMetricsGVR, context.Canceled)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fac, mgmt, downstream := setUpProjectFixture(t, "")
			tc.set(mgmt, downstream)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			rc := NewInspectProject(fac).(runtime.ReadCommand)
			_, err := rc.Execute(ctx, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}}, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestRenderProjectTextSections(t *testing.T) {
	t.Parallel()
	r := projectResult{
		ID: "p-cvrp4", DisplayName: "Default", Cluster: "c-phmj7",
		Namespaces: []projectNamespace{{Name: "default", Workloads: projectWorkloads{Pods: 3, Deployments: 1}}},
		Members: []rbac.MemberRow{
			{Principal: "github_org://8341", Role: "project-member", DisplayName: "DevOps Team"},
			{Principal: "local://u-alice", Role: "project-owner", DisplayName: "Alice Ann"},
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

// TestStyleProjectTextStripInvariant asserts inspect project's bold spans:
// every section header is bold, whole line; the strip invariant holds; and
// a disabled Style returns text unchanged.
func TestStyleProjectTextStripInvariant(t *testing.T) {
	t.Parallel()
	text := renderProjectText(projectResult{ID: "p-cvrp4", DisplayName: "Default", Cluster: "c-phmj7"})
	enabled := style.Style{Enabled: true}

	styled := styleProjectText(text, enabled)
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleProjectText(text, style.Style{}), "disabled style returns text unchanged")

	for _, header := range []string{"PROJECT", "NAMESPACES", "MEMBERS", "QUOTA", "USAGE"} {
		assert.Contains(t, styled, "\x1b[1m"+header+"\x1b[0m\n", "%s must be bold, whole line", header)
	}
	assert.NotContains(t, styled, "\x1b[1mid: p-cvrp4", "a data line must not be styled")
}

func TestRenderProjectTextEmptyAndUnavailable(t *testing.T) {
	t.Parallel()
	// null namespaces is a failed read, which says nothing about usage.
	r := projectResult{ID: "p-x", DisplayName: "X", Cluster: "c-x", Members: nil}
	text := renderProjectText(r)
	assert.Contains(t, text, "NAMESPACES\n  namespaces: unavailable\n")
	assert.Contains(t, text, "MEMBERS\n  members: unavailable\n")
	assert.Contains(t, text, "QUOTA\n  none\n")
	assert.Contains(t, text, "USAGE\n  usage: unavailable\n")

	// zero namespaces read successfully → verified-empty usage, NOT unavailable.
	r.Namespaces = []projectNamespace{}
	text = renderProjectText(r)
	assert.Contains(t, text, "NAMESPACES\n  none\n")
	assert.Contains(t, text, "USAGE\n  none (no namespaces)\n")

	r.Members = []rbac.MemberRow{}
	assert.Contains(t, renderProjectText(r), "MEMBERS\n  none\n")

	// Defensive: Usage nil with non-empty Namespaces renders an explicit
	// marker, not a bare header. Unreachable from Execute today but asserts the
	// renderer contract independent of the seeding invariant.
	r.Namespaces = []projectNamespace{{Name: "ns1"}}
	assert.Contains(t, renderProjectText(r), "USAGE\n  usage: unavailable\n")
}

// TestInspectProjectZeroPodsVerifiedZero covers the case that triggered the
// "usage: unavailable" regression: a project with member namespaces but zero
// pods must produce verified-zero totals (non-nil Usage with "0" values), not
// a nil Usage that renders as unavailable.
func TestInspectProjectZeroPodsVerifiedZero(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	podKind := mustKind(t, "pod")
	depKind := mustKind(t, "deployment")
	prtbKind := mustKind(t, "projectroletemplatebinding")

	mgmt := newFakeClient()
	mgmt.withListNS(projectKind.GVR, "c-test", &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{*projectFixtureObj("p-empty", "Empty", "")},
	})
	mgmt.withObject(projectKind.GVR, "c-test", "p-empty", projectFixtureObj("p-empty", "Empty", ""))
	mgmt.withListNS(prtbKind.GVR, "p-empty", &unstructured.UnstructuredList{})

	downstream := newFakeClient()
	downstream.withList(nsKind.GVR, &unstructured.UnstructuredList{
		Items: []unstructured.Unstructured{nsListItem("ns-a")},
	})
	downstream.withList(podKind.GVR, &unstructured.UnstructuredList{})
	downstream.withList(depKind.GVR, &unstructured.UnstructuredList{})

	fac := &fakeFactory{perCluster: map[string]engine.ClusterClient{
		engine.LocalCluster: mgmt,
		"c-test":            downstream,
	}}

	cmd := NewInspectProject(fac)
	res, err := execRead(t, cmd, input{args: []string{"p-empty"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data := res.Data.(projectResult)

	require.NotNil(t, data.Usage, "zero pods in a member namespace must produce verified-zero totals, not nil")
	assert.Equal(t, "0", data.Usage.CPURequest)
	assert.Equal(t, "0", data.Usage.CPULimit)
	assert.Equal(t, "0", data.Usage.MemoryRequest)
	assert.Equal(t, "0", data.Usage.MemoryLimit)

	// Text view must render explicit zeros, not "unavailable".
	text := res.Text
	assert.Contains(t, text, "cpu: request=0 limit=0")
	assert.Contains(t, text, "memory: request=0 limit=0")
	assert.NotContains(t, text, "unavailable")
}

// TestProjectUsageJSONContract asserts the verified-zero usage state at the JSON
// level: Usage non-nil with zero values must serialize as a non-null object
// (not null), so agents can distinguish "no pods measured" from "data absent".
func TestProjectUsageJSONContract(t *testing.T) {
	t.Parallel()

	zeroes := projectResult{
		ID:    "p-1",
		Usage: &resourceTotals{CPURequest: "0", CPULimit: "0", MemoryRequest: "0", MemoryLimit: "0"},
	}
	b, err := json.Marshal(zeroes)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	usageRaw, hasUsage := doc["usage"]
	require.True(t, hasUsage, "usage key must be present")
	require.NotNil(t, usageRaw, "verified-zero usage must be non-null in JSON")
	usageMap, ok := usageRaw.(map[string]any)
	require.True(t, ok, "usage must be a JSON object")
	assert.Equal(t, "0", usageMap["cpuRequest"])
	assert.Equal(t, "0", usageMap["memoryRequest"])
}

func TestProjectMemberJSONContract(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(rbac.MemberRow{Principal: "local://u-a", Role: "project-owner", Binding: "prtb-abc", DisplayName: "A"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"principal":"local://u-a","role":"project-owner","binding":"prtb-abc","displayName":"A"}`, string(b))
	b, err = json.Marshal(rbac.MemberRow{Principal: "local://u-a", Role: "project-owner", Binding: "prtb-abc"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"principal":"local://u-a","role":"project-owner","binding":"prtb-abc"}`, string(b))
}

// TestProjectMembersNullVsEmptyContract asserts the WHOLE-payload encoding -
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
	b, err = json.Marshal(projectResult{ID: "p", Members: []rbac.MemberRow{}})
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
	b, err := json.Marshal(rbac.MemberRow{Principal: "system://x", Role: "r", Binding: "prtb-x", System: true})
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, true, doc["system"])

	b, err = json.Marshal(rbac.MemberRow{Principal: "u-alice", Role: "r", Binding: "prtb-alice"})
	require.NoError(t, err)
	doc = nil
	require.NoError(t, json.Unmarshal(b, &doc))
	_, has := doc["system"]
	assert.False(t, has, "zero System must omit the key")
}

// TestInspectProjectNotFoundCurated asserts Fix 1: the post-Resolve project Get
// returns a hint-less NOT_FOUND; it must be curated to "project not found:
// <id>" with a list hint - the same shape inspect cluster uses.
func TestInspectProjectNotFoundCurated(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	mgmt := newFakeClient().
		withListNS(projectKind.GVR, "c-test", &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{*projectFixtureObj("p-gone", "Gone", "")},
		}).
		withGetErr(projectKind.GVR, "c-test", "p-gone", kinds.NotFoundCurated(projectKind, "p-gone", ""))
	downstream := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{})
	fac := &fakeFactory{perCluster: map[string]engine.ClusterClient{
		engine.LocalCluster: mgmt,
		"c-test":            downstream,
	}}

	cmd := NewInspectProject(fac)
	_, err := execRead(t, cmd, input{args: []string{"p-gone"}, s: map[string]string{"cluster": "c-test"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-gone", ce.Message)
	assert.NotEmpty(t, ce.Hint, "curated project error must carry a list hint")
}

// TestInspectProjectGetDeniedIsHardRBAC verifies the first required
// collector: the project GET is the document's substance, so a denial is the
// command's error with its RBAC identity intact, never a degraded envelope.
func TestInspectProjectGetDeniedIsHardRBAC(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	denied := clierr.New(clierr.CodeRBAC, "projects.management.cattle.io \"p-test\" is forbidden")
	mgmt := newFakeClient().
		withListNS(projectKind.GVR, "c-test", &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{*projectFixtureObj("p-test", "Test", "")},
		}).
		withGetErr(projectKind.GVR, "c-test", "p-test", denied)
	downstream := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{})
	fac := &fakeFactory{perCluster: map[string]engine.ClusterClient{
		engine.LocalCluster: mgmt,
		"c-test":            downstream,
	}}

	cmd := NewInspectProject(fac)
	res, err := execRead(t, cmd, input{args: []string{"p-test"}, s: map[string]string{"cluster": "c-test"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Nil(t, res.Data, "a required collector's denial yields no payload to degrade")
	assert.Empty(t, res.Warnings)
}

// TestInspectProjectNotFoundWithHintPassesThrough asserts that an already-curated
// NOT_FOUND (one that already carries a hint) passes through unchanged - a
// resolver's candidates error must not be re-wrapped.
func TestInspectProjectNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	alreadyCurated := clierr.New(clierr.CodeNotFound, "project not found: p-gone").
		WithHint("list with: rancher get projects")
	mgmt := newFakeClient().
		withListNS(projectKind.GVR, "c-test", &unstructured.UnstructuredList{
			Items: []unstructured.Unstructured{*projectFixtureObj("p-gone", "Gone", "")},
		}).
		withGetErr(projectKind.GVR, "c-test", "p-gone", alreadyCurated)
	downstream := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{})
	fac := &fakeFactory{perCluster: map[string]engine.ClusterClient{
		engine.LocalCluster: mgmt,
		"c-test":            downstream,
	}}

	cmd := NewInspectProject(fac)
	_, err := execRead(t, cmd, input{args: []string{"p-gone"}, s: map[string]string{"cluster": "c-test"}})
	require.Error(t, err)
	assert.Equal(t, alreadyCurated, err, "already-curated error must pass through unchanged")
}

func TestRenderProjectTextEscapeStripping(t *testing.T) {
	t.Parallel()
	r := projectResult{
		ID: "p-x", DisplayName: "X", Cluster: "c-x",
		Members: []rbac.MemberRow{
			{Principal: "okta_user://evil", DisplayName: "Evil\x1b[2Jname", Role: "project-member"},
		},
	}
	text := renderProjectText(r)
	assert.NotContains(t, text, "\x1b", "ANSI escape sequences from IdP display names must be stripped")
	assert.Contains(t, text, "Evil[2Jname", "printable chars around the escape are preserved")

	r.Members = []rbac.MemberRow{
		{Principal: "okta_user://evil", DisplayName: "abc\u202edef\u200bghi", Role: "project-member"},
	}
	text = renderProjectText(r)
	assert.NotContains(t, text, "\u202e", "bidi-override format characters must be stripped")
	assert.NotContains(t, text, "\u200b", "zero-width format characters must be stripped")
	assert.Contains(t, text, "abcdefghi", "visible runes around format characters are preserved")
}
