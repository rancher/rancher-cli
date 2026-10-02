package inspect

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podWithResources(ns, name, cpuReq, memReq, cpuLim, memLim string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "app",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": cpuReq, "memory": memReq},
						"limits":   map[string]any{"cpu": cpuLim, "memory": memLim},
					},
				},
			},
		},
	}}
}

func podMetricsWithUsage(ns, name, cpu, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": memory}},
		},
	}}
}

func namespaceObj(name string, labels map[string]any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "labels": labels},
	}}
}

func TestUsageAggregatesPerNamespace(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
		*podWithResources("ns2", "p1", "50m", "50Mi", "100m", "100Mi"),
	}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podMetricsWithUsage("ns1", "p1", "80m", "90Mi"),
		podMetricsWithUsage("ns2", "p1", "40m", "45Mi"),
	}}

	client := newFakeClient().
		withList(podKind.GVR, pods).
		withList(podMetricsGVR, metrics)

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	assert.Equal(t, "ResourceUsage", res.Kind)
	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	rows := env.Data
	require.Contains(t, rows, "ns1")
	require.Contains(t, rows, "ns2")
	assert.Equal(t, "100m", rows["ns1"].CPURequest)
	assert.Equal(t, "200Mi", rows["ns1"].MemoryLimit)
	assert.Equal(t, "80m", rows["ns1"].CPUUsage)
	assert.Equal(t, "50m", rows["ns2"].CPURequest)
	assert.Equal(t, "45Mi", rows["ns2"].MemoryUsage)
}

func TestUsageDegradesWithoutMetrics(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, pods).
		withListErr(podMetricsGVR, errors.New("no metrics server"))

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "metrics unavailable: no metrics server", res.Warnings[0])
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, []sectionOutcome{{Section: sectionMetrics, State: stateUnavailable, Code: clierr.CodeGeneric}}, env.Degraded)
	rows := env.Data
	assert.Empty(t, rows["ns1"].CPUUsage)
}

// TestUsageProjectGroupsByLabel asserts --project scopes the rows to the
// namespaces of the project the pipeline resolved; the command lists no
// namespaces itself.
func TestUsageProjectGroupsByLabel(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	client := newFakeClient().
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
			*podWithResources("ns-other", "p2", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1", "ns2")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)

	assert.False(t, client.listed(nsKind.GVR), "the project's namespaces come from the resolved project")
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	rows := env.Data
	require.Contains(t, rows, "ns1")
	assert.Contains(t, rows, "ns2")
	assert.NotContains(t, rows, "ns-other", "a namespace outside the project gets no row")
	assert.NotContains(t, rows, "p-x")
}

func TestUsageNamespaceScopesMetricsFetch(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
	}}
	client := newFakeClient().
		withListNS(podKind.GVR, "ns1", pods).
		withListNS(podMetricsGVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			podMetricsWithUsage("ns1", "p1", "80m", "90Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns1"}})
	require.NoError(t, err)

	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	rows := env.Data
	assert.Equal(t, "80m", rows["ns1"].CPUUsage, "metrics List must be scoped to ns1, not cluster-wide")

	for _, ns := range client.listNamespaces {
		assert.Equal(t, "ns1", ns, "every List call in --namespace mode must be scoped, never cluster-wide")
	}
}

// TestUsageProjectEmptyMembersReturnsEmptyAggregate asserts the empty-project
// consistency decision (get's behavior wins): an empty member set is a valid
// empty result, not an error - a typo'd project name already fails loudly at
// the project lookup with NOT_FOUND, so an existing-but-empty project legitimately
// has nothing, exactly like `get pods --project empty` returns zero rows.
func TestUsageProjectEmptyMembersReturnsEmptyAggregate(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web"), cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	rows := env.Data
	assert.Empty(t, rows, "an empty-member project must return an empty aggregate, not a project-keyed row")
}

func TestUsageNamespaceAndProjectValidatesMembership(t *testing.T) {
	t.Parallel()
	client := newFakeClient()

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1")
	_, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns2", "project": "p-x"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns2")
	assert.Contains(t, ce.Message, "p-x")
}

func TestUsageNamespaceAndProjectMemberScopesAsNamespace(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withListNS(podKind.GVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns1", "project": "p-x"}})
	require.NoError(t, err)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	rows := env.Data
	assert.Contains(t, rows, "ns1", "a member namespace scopes the result by namespace, not by project bucket")
	assert.NotContains(t, rows, "p-x")
}

func TestUsageEnvelopeHasByDiscriminator(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok, "Result.Data must be usageEnvelope")
	assert.Equal(t, "namespace", env.By)
}

func TestUsageByNodeRejectsNamespaceFlag(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	cmd := NewUsage(&fakeFactory{client: client}, nil)
	_, err := execRead(t, cmd, input{s: map[string]string{
		"cluster": "c-abc", "by": "node", "namespace": "ns1",
	}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestUsageByNodeRejectsProjectFlag asserts --by node refuses a typed
// --project before the project is read.
func TestUsageByNodeRejectsProjectFlag(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
		t.Error("the project was read")
		return runtime.Project{}, errors.New("unexpected")
	})
	_, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{
		"cluster": "c-abc", "by": "node", "project": "p-x",
	}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, client.listGVRs, "no read before the refusal")
}

// --project alone (no --by) → member-namespace rows, not a single
// project bucket.
func TestUsageProjectAloneGivesMemberNamespaceRows(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	client := newFakeClient().
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
			*podWithResources("ns2", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1", "ns2")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "namespace", env.By)
	assert.Contains(t, env.Data, "ns1", "--project without --by must return member-namespace rows")
	assert.Contains(t, env.Data, "ns2")
	assert.NotContains(t, env.Data, "p-x")
}

func TestUsageProjectAloneIncludesZeroPodMembers(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	client := newFakeClient().
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1", "ns-empty")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Contains(t, env.Data, "ns1")
	assert.Contains(t, env.Data, "ns-empty", "zero-pod member namespace must still appear")
	assert.Equal(t, "0", env.Data["ns-empty"].CPURequest)
}

// --by project --project X → single project bucket.
func TestUsageByProjectWithProjectFlagGivesSingleBucket(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, newSteveRoot(t, nil).roots())
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "project": "p-x"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "project", env.By)
	assert.Contains(t, env.Data, "p-x", "--by project --project X must return a single project bucket")
	assert.NotContains(t, env.Data, "ns1")
}

// podOnNode lives here (usage_test.go) and usage_node_test.go reuses it.
func podOnNode(ns, name, nodeName, cpuReq, memReq, cpuLim, memLim string) *unstructured.Unstructured {
	p := podWithResources(ns, name, cpuReq, memReq, cpuLim, memLim)
	_ = unstructured.SetNestedField(p.Object, nodeName, "spec", "nodeName")
	return p
}

// --node must narrow the default (namespace) grouping - never a silent no-op.
func TestUsageNodeFilterDefaultGrouping(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	client := newFakeClient().
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
			*podOnNode("ns2", "p2", "n2", "50m", "64Mi", "100m", "128Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "ns1")
	assert.NotContains(t, env.Data, "ns2", "--node must exclude namespaces whose pods are on other nodes")
	assert.Equal(t, "100m", env.Data["ns1"].CPURequest)
}

func TestSumPodResourcesSinglePod(t *testing.T) {
	t.Parallel()
	pods := []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
	}
	cpuReq, cpuLim, memReq, memLim := sumPodResources(pods)
	assert.Equal(t, "100m", cpuReq.String())
	assert.Equal(t, "200m", cpuLim.String())
	assert.Equal(t, "128Mi", memReq.String())
	assert.Equal(t, "256Mi", memLim.String())
}

func TestSumPodResourcesMultiContainer(t *testing.T) {
	t.Parallel()
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p1", "namespace": "ns1"},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "app",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "100m", "memory": "64Mi"},
						"limits":   map[string]any{"cpu": "200m", "memory": "128Mi"},
					},
				},
				map[string]any{
					"name": "sidecar",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "50m", "memory": "32Mi"},
						"limits":   map[string]any{"cpu": "100m", "memory": "64Mi"},
					},
				},
			},
		},
	}}
	cpuReq, cpuLim, memReq, memLim := sumPodResources([]unstructured.Unstructured{*pod})
	assert.Equal(t, "150m", cpuReq.String())
	assert.Equal(t, "300m", cpuLim.String())
	assert.Equal(t, "96Mi", memReq.String())
	assert.Equal(t, "192Mi", memLim.String())
}

// TestUsageVerifiedEmptyMembershipSkipsPodList asserts a membership list the
// caller could read, returning zero namespaces, short-circuits: no pod or
// metrics read is issued, so a denied read on either cannot degrade a scope
// that provably has nothing in it. The fake fails the test by serving errors
// for the reads that must not happen.
func TestUsageVerifiedEmptyMembershipSkipsPodList(t *testing.T) {
	t.Parallel()
	for _, by := range []string{byNamespace, byPod} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			podKind := mustKind(t, "pod")

			client := newFakeClient().
				withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden")).
				withListErr(podMetricsGVR, clierr.New(clierr.CodeRBAC, "forbidden"))

			cmd := NewUsage(&fakeFactory{client: client}, nil)
			res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web"), cmd, input{s: map[string]string{
				"cluster": "c-abc", "project": "p-x", "by": by,
			}})
			require.NoError(t, err)

			env, ok := res.Data.(usageEnvelope)
			require.True(t, ok)
			assert.Empty(t, env.Data)
			assert.Nil(t, env.Degraded, "a verified-empty scope degrades nothing")
			assert.Empty(t, res.Warnings)
			assert.False(t, client.listed(podKind.GVR), "no pod read for a scope with no namespaces")
			assert.False(t, client.listed(podMetricsGVR), "no metrics read either")
		})
	}
}

// TestUsageByProjectVerifiedZeroBucketSkipsReads asserts the same rule for
// --by project: an ownership map proving the named project owns no namespace
// yields the verified-zero bucket with no pod or metrics read.
func TestUsageByProjectVerifiedZeroBucketSkipsReads(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-y"}),
		}}).
		withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden")).
		withListErr(podMetricsGVR, clierr.New(clierr.CodeRBAC, "forbidden"))

	cmd := NewUsage(&fakeFactory{client: client}, newSteveRoot(t, nil).roots())
	res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web"), cmd, input{s: map[string]string{
		"cluster": "c-abc", "by": byProject, "project": "p-x",
	}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Equal(t, usageRows{"p-x": {
		CPURequest: "0", CPULimit: "0", MemoryRequest: "0", MemoryLimit: "0",
	}}, env.Data, "the bucket is a verified zero, not a missing row")
	assert.Nil(t, env.Degraded)
	assert.False(t, client.listed(podKind.GVR), "no pod read for a project that owns no namespace")
	assert.False(t, client.listed(podMetricsGVR), "no metrics read either")
}

// TestUsageRequiredCollectorsHardError asserts every collector the matrix
// calls required keeps its error identity instead of degrading: the scope
// reads define which rows may exist, and a fabricated or unscoped result
// would lie about what the caller can see.
func TestUsageRequiredCollectorsHardError(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	nodeKind := mustKind(t, "node")
	denied := clierr.New(clierr.CodeRBAC, "forbidden")

	base := func() *fakeClient {
		return newFakeClient().
			withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
			}}).
			withList(nodeKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*nodeObj("n1"),
			}}).
			withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
			withList(podKind.GVR, &unstructured.UnstructuredList{}).
			withList(podMetricsGVR, &unstructured.UnstructuredList{})
	}

	// membership is the resolved project's namespace read failing.
	deniedMembers := func(t *testing.T) context.Context {
		t.Helper()
		return runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-abc", ID: "p-x", DisplayName: "web",
			Namespaces: func(context.Context) (map[string]bool, error) { return nil, denied }})
	}
	for _, tc := range []struct {
		name string
		in   input
		set  func(c *fakeClient)
		ctx  func(t *testing.T) context.Context
	}{
		{"pod list", input{s: map[string]string{"cluster": "c-abc"}}, func(c *fakeClient) {
			c.withListErr(podKind.GVR, denied)
		}, nil},
		{"membership", input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}}, func(*fakeClient) {}, deniedMembers},
		{"ownership map", input{s: map[string]string{"cluster": "c-abc", "by": byProject}}, func(c *fakeClient) {
			c.withListErr(nsKind.GVR, denied)
		}, nil},
		{"node list", input{s: map[string]string{"cluster": "c-abc", "by": byNode}}, func(c *fakeClient) {
			c.withListErr(nodeKind.GVR, denied)
		}, nil},
		{"named node get", input{s: map[string]string{"cluster": "c-abc", "by": byNode, "node": "n1"}}, func(c *fakeClient) {
			c.withGetErr(nodeKind.GVR, "", "n1", denied)
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := base()
			tc.set(client)
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}

			cmd := NewUsage(&fakeFactory{client: client}, projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web")).roots())
			_, err := execReadCtx(t, ctx, cmd, tc.in)
			require.Error(t, err)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeRBAC, ce.Code, "a required collector keeps its error identity")
		})
	}
}

// TestUsageNamedNodeNeverListsNodes asserts the filtered mode gets exactly
// the named node: a caller allowed to get one node must not be forced to
// list all of them.
func TestUsageNamedNodeNeverListsNodes(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	client := newFakeClient().
		withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
		withListErr(nodeKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden")).
		withList(podKind.GVR, &unstructured.UnstructuredList{}).
		withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", "100m", "1Gi"))

	cmd := NewUsage(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": byNode, "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Contains(t, env.Data, "n1")
	assert.False(t, client.listed(nodeKind.GVR), "the filtered mode must never list nodes")
}

// TestUsageMetricsCancellationStaysHard asserts a canceled caller context
// fails the command instead of recording a metrics section, in every mode
// that reads metrics.
func TestUsageMetricsCancellationStaysHard(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nodeKind := mustKind(t, "node")

	for _, tc := range []struct {
		name string
		in   input
		set  func(c *fakeClient)
	}{
		{"pod metrics", input{s: map[string]string{"cluster": "c-abc"}}, func(c *fakeClient) {
			c.withListErr(podMetricsGVR, context.Canceled)
		}},
		{"node metrics list", input{s: map[string]string{"cluster": "c-abc", "by": byNode}}, func(c *fakeClient) {
			c.withListErr(nodeMetricsGVR, context.Canceled)
		}},
		{"named node metrics", input{s: map[string]string{"cluster": "c-abc", "by": byNode, "node": "n1"}}, func(c *fakeClient) {
			c.withGetErr(nodeMetricsGVR, "", "n1", context.Canceled)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newFakeClient().
				withList(podKind.GVR, &unstructured.UnstructuredList{}).
				withList(podMetricsGVR, &unstructured.UnstructuredList{}).
				withList(nodeKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}).
				withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
				withList(nodeMetricsGVR, &unstructured.UnstructuredList{}).
				withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", "100m", "1Gi"))
			tc.set(client)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			rc := NewUsage(&fakeFactory{client: client}, nil).(runtime.ReadCommand)
			_, err := rc.Execute(ctx, tc.in, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}
