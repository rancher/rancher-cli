package inspect

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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

// quantityForm is one quantity as the pod spec or metrics server writes it,
// the exact string the envelope keeps, and the cell the table prints.
type quantityForm struct{ in, exact, cell string }

var cpuForms = []quantityForm{
	{"270091638n", "270091638n", "271m"},
	{"1n", "1n", "1m"},
	{"1500u", "1500u", "2m"},
	{"250m", "250m", "250m"},
	{"2", "2", "2000m"},
	{"1500m", "1500m", "1500m"},
	{"0", "0", "0m"},
}

var memoryForms = []quantityForm{
	{"2934696Ki", "2934696Ki", "2865Mi"},
	{"256Mi", "256Mi", "256Mi"},
	{"1Gi", "1Gi", "1024Mi"},
	{"1.5Gi", "1536Mi", "1536Mi"},
	{"1023Ki", "1023Ki", "0Mi"},
	{"1048577", "1048577", "1Mi"},
	{"134217728", "134217728", "128Mi"},
	{"0", "0", "0Mi"},
}

// quantitySet is the six quantities of one scope: request, limit and usage,
// for CPU and for memory.
type quantitySet struct{ cpuReq, cpuLim, cpuUse, memReq, memLim, memUse quantityForm }

// quantitySets rotates the forms through every position, so each form is a
// request, a limit and a usage in some set.
func quantitySets() []quantitySet {
	n := max(len(cpuForms), len(memoryForms))
	sets := make([]quantitySet, n)
	for i := range sets {
		c := func(k int) quantityForm { return cpuForms[(i+k)%len(cpuForms)] }
		m := func(k int) quantityForm { return memoryForms[(i+k)%len(memoryForms)] }
		sets[i] = quantitySet{cpuReq: c(0), cpuLim: c(1), cpuUse: c(2), memReq: m(0), memLim: m(1), memUse: m(2)}
	}
	return sets
}

func (s quantitySet) exact() resourceTotals {
	return resourceTotals{
		CPURequest: s.cpuReq.exact, CPULimit: s.cpuLim.exact, CPUUsage: s.cpuUse.exact,
		MemoryRequest: s.memReq.exact, MemoryLimit: s.memLim.exact, MemoryUsage: s.memUse.exact,
	}
}

func (s quantitySet) cells() []string {
	return []string{s.cpuReq.cell, s.cpuLim.cell, s.cpuUse.cell, s.memReq.cell, s.memLim.cell, s.memUse.cell}
}

// TestUsageTableConvertsQuantitiesAndEnvelopeKeepsThem asserts, for every
// grouping, that the table prints CPU in millicores and memory in
// mebibytes in all six quantity columns while the envelope keeps the exact
// strings.
func TestUsageTableConvertsQuantitiesAndEnvelopeKeepsThem(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	nodeKind := mustKind(t, "node")

	podsFor := func(s quantitySet, node string) *unstructured.UnstructuredList {
		pod := podOnNode("ns1", "p1", node, s.cpuReq.in, s.memReq.in, s.cpuLim.in, s.memLim.in)
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*pod}}
	}
	podMetricsFor := func(s quantitySet) *unstructured.UnstructuredList {
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			podMetricsWithUsage("ns1", "p1", s.cpuUse.in, s.memUse.in)}}
	}

	for _, g := range []struct {
		name  string
		key   string
		scope string
		run   func(t *testing.T, s quantitySet) (runtime.Result, error)
	}{
		{"namespace", "ns1", "ns1", func(t *testing.T, s quantitySet) (runtime.Result, error) {
			t.Helper()
			c := newFakeClient().withList(podKind.GVR, podsFor(s, "")).withList(podMetricsGVR, podMetricsFor(s))
			return execRead(t, NewUsage(&fakeFactory{client: c}, nil), input{s: map[string]string{"cluster": "c-abc"}})
		}},
		{"pod", "ns1/p1", "ns1/p1", func(t *testing.T, s quantitySet) (runtime.Result, error) {
			t.Helper()
			c := newFakeClient().withList(podKind.GVR, podsFor(s, "")).withList(podMetricsGVR, podMetricsFor(s))
			return execRead(t, NewUsage(&fakeFactory{client: c}, nil), input{s: map[string]string{"cluster": "c-abc", "by": byPod}})
		}},
		{"project", "p-x", "web", func(t *testing.T, s quantitySet) (runtime.Result, error) {
			t.Helper()
			c := newFakeClient().
				withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"})}}).
				withList(podKind.GVR, podsFor(s, "")).withList(podMetricsGVR, podMetricsFor(s))
			root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
			return execRead(t, NewUsage(&fakeFactory{client: c}, root.roots()), input{s: map[string]string{"cluster": "c-abc", "by": byProject}})
		}},
		{"node", "n1", "n1", func(t *testing.T, s quantitySet) (runtime.Result, error) {
			t.Helper()
			c := newFakeClient().
				withList(nodeKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}).
				withList(podKind.GVR, podsFor(s, "n1")).
				withList(nodeMetricsGVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					nodeMetricsObj("n1", s.cpuUse.in, s.memUse.in)}})
			return execRead(t, NewUsage(&fakeFactory{client: c}, nil), input{s: map[string]string{"cluster": "c-abc", "by": byNode}})
		}},
		{"named node", "n1", "n1", func(t *testing.T, s quantitySet) (runtime.Result, error) {
			t.Helper()
			c := newFakeClient().
				withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
				withList(podKind.GVR, podsFor(s, "n1")).
				withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", s.cpuUse.in, s.memUse.in))
			return execRead(t, NewUsage(&fakeFactory{client: c}, nil), input{s: map[string]string{"cluster": "c-abc", "by": byNode, "node": "n1"}})
		}},
	} {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			for i, s := range quantitySets() {
				res, err := g.run(t, s)
				require.NoError(t, err, "set %d", i)
				env, ok := res.Data.(usageEnvelope)
				require.True(t, ok)
				require.Contains(t, env.Data, g.key, "set %d", i)
				assert.Equal(t, s.exact(), env.Data[g.key], "the envelope keeps the exact strings, set %d", i)
				rows := env.Rows()
				require.Len(t, rows, 1, "set %d", i)
				assert.Equal(t, g.scope, rows[0][0], "set %d", i)
				assert.Equal(t, s.cells(), rows[0][1:], "the table converts, set %d", i)
			}
		})
	}
}

// TestUsageTableLeavesEmptyAndUnparsedCellsAsStored asserts a cell with no
// value stays empty and a value that is not a quantity prints as stored.
func TestUsageTableLeavesEmptyAndUnparsedCellsAsStored(t *testing.T) {
	t.Parallel()
	rows := usageRows{"ns1": {CPURequest: "100m", CPULimit: "not-a-quantity", MemoryRequest: "1Gi", MemoryLimit: "??"}}
	assert.Equal(t, [][]string{{"ns1", "100m", "not-a-quantity", "", "1024Mi", "??", ""}}, rows.Rows())
}

// getAnswer scripts the fake's answer to one named GET: none leaves it
// unregistered (the fake answers an unregistered GET with an error, so a
// row that expects no read of it also asserts the GET log), an object, or
// an error.
type getAnswer struct {
	registered bool
	err        error
}

var getNone = getAnswer{}
var getOK = getAnswer{registered: true}

func getErr(err error) getAnswer { return getAnswer{registered: true, err: err} }

// emptyScopeGets renders the GETs a fake served as "kind/name"; nil for none.
func emptyScopeGets(client *fakeClient) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var out []string
	for _, k := range client.getCalls {
		out = append(out, k.gvr.Resource+"/"+k.name)
	}
	return out
}

// scopeFake serves pods as the apiserver does: a list in a namespace holds
// that namespace's pods, a cluster-wide list all of them. The namespace
// and the node answer as scripted.
func scopeFake(t *testing.T, pods []unstructured.Unstructured, namespaces []string, nsAnswer, nodeAnswer getAnswer, ns, node string) *fakeClient {
	t.Helper()
	podKind, nsKind, nodeKind := mustKind(t, "pod"), mustKind(t, "namespace"), mustKind(t, "node")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: pods}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})
	for _, name := range namespaces {
		var inNS []unstructured.Unstructured
		for _, p := range pods {
			if p.GetNamespace() == name {
				inNS = append(inNS, p)
			}
		}
		client.withListNS(podKind.GVR, name, &unstructured.UnstructuredList{Items: inNS})
	}
	answer := func(a getAnswer, kind kinds.Kind, name string, obj *unstructured.Unstructured) {
		switch {
		case !a.registered:
		case a.err != nil:
			client.withGetErr(kind.GVR, "", name, a.err)
		default:
			client.withObject(kind.GVR, "", name, obj)
		}
	}
	answer(nsAnswer, nsKind, ns, &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": ns}}})
	answer(nodeAnswer, nodeKind, node, nodeObj(node))
	return client
}

// TestUsageEmptyScopeReadsNamespaceAndNode asserts, for the namespace and
// pod groupings, that a --namespace or --node that matches no pods is
// read: a missing one is NOT_FOUND before any metrics read, one the caller
// cannot read leaves the result as it is with a warning, a scope with pods
// reads nothing, and the namespace is read before the node.
func TestUsageEmptyScopeReadsNamespaceAndNode(t *testing.T) {
	t.Parallel()
	nsKind, nodeKind := mustKind(t, "namespace"), mustKind(t, "node")
	forbidden := clierr.New(clierr.CodeRBAC, "forbidden")
	nsMissing := kinds.NotFoundCurated(nsKind, "web", "c-abc")
	nodeMissing := kinds.NotFoundCurated(nodeKind, "n1", "c-abc")
	nsWarning := `namespace "web" could not be checked: forbidden; no pods were found`
	nodeWarning := `node "n1" could not be checked: forbidden; no pods were found`
	webPod := func() unstructured.Unstructured { return *podOnNode("web", "p1", "n1", "10m", "10Mi", "20m", "20Mi") }
	otherNodePod := func() unstructured.Unstructured { return *podOnNode("web", "p2", "n2", "10m", "10Mi", "20m", "20Mi") }
	elsewherePod := func() unstructured.Unstructured { return *podOnNode("other", "p3", "n2", "10m", "10Mi", "20m", "20Mi") }

	for _, tc := range []struct {
		name         string
		flags        map[string]string
		pods         []unstructured.Unstructured
		ns, node     getAnswer
		metricsErr   error
		wantCode     string
		wantMessage  string
		wantErr      bool
		wantWarnings []string
		wantGets     []string
		wantNoPods   bool
	}{
		{name: "namespace without pods exists", flags: map[string]string{"namespace": "web"}, ns: getOK,
			wantGets: []string{"namespaces/web"}, wantNoPods: true},
		{name: "namespace without pods is missing", flags: map[string]string{"namespace": "web"}, ns: getErr(nsMissing),
			wantCode: clierr.CodeNotFound, wantMessage: "namespace not found: web", wantGets: []string{"namespaces/web"}},
		{name: "namespace without pods is unreadable", flags: map[string]string{"namespace": "web"}, ns: getErr(forbidden),
			wantWarnings: []string{nsWarning}, wantGets: []string{"namespaces/web"}, wantNoPods: true},
		{name: "unreadable namespace warns with the message and not the hint", flags: map[string]string{"namespace": "web"},
			ns:           getErr(clierr.New(clierr.CodeRBAC, "forbidden").WithHint("ask an admin")),
			wantWarnings: []string{nsWarning}, wantGets: []string{"namespaces/web"}, wantNoPods: true},
		{name: "namespace read fails otherwise", flags: map[string]string{"namespace": "web"}, ns: getErr(errors.New("boom")),
			wantErr: true, wantGets: []string{"namespaces/web"}},
		{name: "namespace with pods reads nothing", flags: map[string]string{"namespace": "web"}, pods: []unstructured.Unstructured{webPod()}},
		{name: "node without pods exists", flags: map[string]string{"node": "n1"}, node: getOK,
			wantGets: []string{"nodes/n1"}, wantNoPods: true},
		{name: "node without pods is missing", flags: map[string]string{"node": "n1"}, node: getErr(nodeMissing),
			wantCode: clierr.CodeNotFound, wantMessage: "node not found: n1", wantGets: []string{"nodes/n1"}},
		{name: "node without pods is unreadable", flags: map[string]string{"node": "n1"}, node: getErr(forbidden),
			wantWarnings: []string{nodeWarning}, wantGets: []string{"nodes/n1"}, wantNoPods: true},
		{name: "node with pods reads nothing", flags: map[string]string{"node": "n1"}, pods: []unstructured.Unstructured{webPod()}},
		{name: "pods on another node read the node", flags: map[string]string{"node": "n1"}, pods: []unstructured.Unstructured{elsewherePod()},
			node: getOK, wantGets: []string{"nodes/n1"}, wantNoPods: true},
		{name: "namespace with pods on another node reads only the node", flags: map[string]string{"namespace": "web", "node": "n1"},
			pods: []unstructured.Unstructured{otherNodePod()}, node: getOK, wantGets: []string{"nodes/n1"}, wantNoPods: true},
		{name: "namespace and node without pods read both in order", flags: map[string]string{"namespace": "web", "node": "n1"},
			ns: getOK, node: getOK, wantGets: []string{"namespaces/web", "nodes/n1"}, wantNoPods: true},
		{name: "a missing namespace ends the check before the node", flags: map[string]string{"namespace": "web", "node": "n1"},
			ns: getErr(nsMissing), wantCode: clierr.CodeNotFound, wantMessage: "namespace not found: web", wantGets: []string{"namespaces/web"}},
		{name: "an unreadable namespace and a missing node", flags: map[string]string{"namespace": "web", "node": "n1"},
			ns: getErr(forbidden), node: getErr(nodeMissing), wantCode: clierr.CodeNotFound, wantMessage: "node not found: n1",
			wantGets: []string{"namespaces/web", "nodes/n1"}},
		{name: "warnings: namespace, node, then metrics", flags: map[string]string{"namespace": "web", "node": "n1"},
			ns: getErr(forbidden), node: getErr(forbidden), metricsErr: errors.New("no metrics server"),
			wantWarnings: []string{nsWarning, nodeWarning, "metrics unavailable: no metrics server"},
			wantGets:     []string{"namespaces/web", "nodes/n1"}, wantNoPods: true},
	} {
		for _, by := range []string{byNamespace, byPod} {
			t.Run(by+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				client := scopeFake(t, tc.pods, []string{"web"}, tc.ns, tc.node, "web", "n1")
				if tc.metricsErr != nil {
					client.withListErr(podMetricsGVR, tc.metricsErr)
				}
				flags := map[string]string{"cluster": "c-abc", "by": by}
				maps.Copy(flags, tc.flags)
				res, err := execRead(t, NewUsage(&fakeFactory{client: client}, nil), input{s: flags})

				assert.Equal(t, tc.wantGets, emptyScopeGets(client), "the GETs")
				if tc.wantCode != "" || tc.wantErr {
					require.Error(t, err)
					assert.False(t, client.listed(podMetricsGVR), "an error ends the command before the metrics read")
					if tc.wantCode != "" {
						ce, ok := errors.AsType[*clierr.Error](err)
						require.True(t, ok)
						assert.Equal(t, tc.wantCode, ce.Code)
						assert.Equal(t, tc.wantMessage, ce.Message)
					}
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tc.wantWarnings, res.Warnings)
				env, ok := res.Data.(usageEnvelope)
				require.True(t, ok)
				if tc.wantNoPods && by == byNamespace && tc.flags["namespace"] != "" {
					require.Contains(t, env.Data, "web", "a namespace without pods keeps its zero row")
					assert.Equal(t, "0", env.Data["web"].CPURequest)
				} else if tc.wantNoPods {
					assert.Empty(t, env.Data)
				}
			})
		}
	}
}

// TestUsageByProjectEmptyScope asserts the project grouping's reads of an
// empty scoped result: --namespace is checked against the namespace list
// the grouping already reads (no extra request), a namespace the list
// holds without a project is not missing, --namespace with --project keeps
// the membership error, and --node follows the same rule as the other
// groupings.
func TestUsageByProjectEmptyScope(t *testing.T) {
	t.Parallel()
	podKind, nsKind, nodeKind := mustKind(t, "pod"), mustKind(t, "namespace"), mustKind(t, "node")
	forbidden := clierr.New(clierr.CodeRBAC, "forbidden")
	nodeMissing := kinds.NotFoundCurated(nodeKind, "n1", "c-abc")
	nodeWarning := `node "n1" could not be checked: forbidden; no pods were found`
	pod := func(ns, node string) unstructured.Unstructured {
		return *podOnNode(ns, "p1", node, "10m", "10Mi", "20m", "20Mi")
	}

	for _, tc := range []struct {
		name         string
		flags        map[string]string
		pods         []unstructured.Unstructured
		node         getAnswer
		scoped       bool
		wantCode     string
		wantMessage  string
		wantWarnings []string
		wantGets     []string
		wantPodList  bool
	}{
		{name: "namespace missing from the namespace list", flags: map[string]string{"namespace": "missing"},
			wantCode: clierr.CodeNotFound, wantMessage: "namespace not found: missing"},
		{name: "namespace without a project is not missing", flags: map[string]string{"namespace": "free"}, wantPodList: true},
		{name: "namespace of a project with pods", flags: map[string]string{"namespace": "ns1"},
			pods: []unstructured.Unstructured{pod("ns1", "n1")}, wantPodList: true},
		{name: "project and a missing namespace keep the membership error", flags: map[string]string{"namespace": "missing"}, scoped: true,
			wantCode: clierr.CodeValidation, wantMessage: `namespace "missing" is not in project "p-x"; pass a project namespace, or omit --namespace to scope the whole project`},
		{name: "node without pods exists", flags: map[string]string{"node": "n1"}, node: getOK, wantGets: []string{"nodes/n1"}, wantPodList: true},
		{name: "node without pods is missing", flags: map[string]string{"node": "n1"}, node: getErr(nodeMissing),
			wantCode: clierr.CodeNotFound, wantMessage: "node not found: n1", wantGets: []string{"nodes/n1"}, wantPodList: true},
		{name: "node without pods is unreadable", flags: map[string]string{"node": "n1"}, node: getErr(forbidden),
			wantWarnings: []string{nodeWarning}, wantGets: []string{"nodes/n1"}, wantPodList: true},
		{name: "node with pods reads nothing", flags: map[string]string{"node": "n1"},
			pods: []unstructured.Unstructured{pod("ns1", "n1")}, wantPodList: true},
		{name: "project and node without pods read the node", flags: map[string]string{"node": "n1"}, scoped: true,
			node: getOK, wantGets: []string{"nodes/n1"}, wantPodList: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := scopeFake(t, tc.pods, []string{"free", "ns1"}, getNone, tc.node, "", "n1").
				withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
					namespaceObj("free", nil),
				}})
			flags := map[string]string{"cluster": "c-abc", "by": byProject}
			maps.Copy(flags, tc.flags)
			ctx := t.Context()
			if tc.scoped {
				flags["project"] = "p-x"
				ctx = withProject(t, "c-abc", "p-x", "web", "ns1")
			}
			root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
			res, err := execReadCtx(t, ctx, NewUsage(&fakeFactory{client: client}, root.roots()), input{s: flags})

			assert.Equal(t, tc.wantGets, emptyScopeGets(client), "the GETs")
			assert.Equal(t, tc.wantPodList, client.listed(podKind.GVR), "the pod list")
			if tc.wantCode != "" {
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, tc.wantCode, ce.Code)
				assert.Equal(t, tc.wantMessage, ce.Message)
				assert.False(t, client.listed(podMetricsGVR), "an error ends the command before the metrics read")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantWarnings, res.Warnings)
		})
	}
}

// TestUsageEmptyProjectScopeReadsNothingForTheNamespace asserts a project
// scope never reads the namespace: an empty membership is a verified empty
// result with no request at all, and a namespace outside the project is
// refused by the membership check before any read; the node is still read
// when the project's pods on it are none.
func TestUsageEmptyProjectScopeReadsNothingForTheNamespace(t *testing.T) {
	t.Parallel()
	for _, by := range []string{byNamespace, byPod} {
		t.Run(by+"/empty membership", func(t *testing.T) {
			t.Parallel()
			client := scopeFake(t, nil, nil, getNone, getNone, "", "n1")
			res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web"), NewUsage(&fakeFactory{client: client}, nil),
				input{s: map[string]string{"cluster": "c-abc", "by": by, "project": "p-x", "node": "n1"}})
			require.NoError(t, err)
			assert.Empty(t, res.Warnings)
			assert.Empty(t, emptyScopeGets(client))
			assert.Empty(t, client.listNamespaces, "no list either")
		})
		t.Run(by+"/namespace outside the project", func(t *testing.T) {
			t.Parallel()
			client := scopeFake(t, nil, nil, getNone, getNone, "", "n1")
			_, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web", "ns1"), NewUsage(&fakeFactory{client: client}, nil),
				input{s: map[string]string{"cluster": "c-abc", "by": by, "project": "p-x", "namespace": "other"}})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Empty(t, emptyScopeGets(client))
		})
		t.Run(by+"/project namespace and node without pods", func(t *testing.T) {
			t.Parallel()
			client := scopeFake(t, nil, []string{"ns1"}, getNone, getOK, "", "n1")
			res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web", "ns1"), NewUsage(&fakeFactory{client: client}, nil),
				input{s: map[string]string{"cluster": "c-abc", "by": by, "project": "p-x", "namespace": "ns1", "node": "n1"}})
			require.NoError(t, err)
			assert.Empty(t, res.Warnings)
			assert.Equal(t, []string{"nodes/n1"}, emptyScopeGets(client), "the namespace is a member and is not read")
		})
	}
}

// TestUsageProjectNodeWithPodsOnlyOutsideTheProject asserts that with
// --project and --node, a node whose pods are all outside the project is
// read: the check runs on the pods left after the project's namespaces are
// applied. A 200 gives no warning and a 403 gives the node warning.
func TestUsageProjectNodeWithPodsOnlyOutsideTheProject(t *testing.T) {
	t.Parallel()
	nsKind := mustKind(t, "namespace")
	nodeWarning := `node "n1" could not be checked: forbidden; no pods were found`
	for _, by := range []string{byNamespace, byPod, byProject} {
		for _, tc := range []struct {
			name         string
			node         getAnswer
			wantWarnings []string
		}{
			{"node readable", getOK, nil},
			{"node unreadable", getErr(clierr.New(clierr.CodeRBAC, "forbidden")), []string{nodeWarning}},
		} {
			t.Run(by+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				outside := *podOnNode("other", "p1", "n1", "10m", "10Mi", "20m", "20Mi")
				client := scopeFake(t, []unstructured.Unstructured{outside}, nil, getNone, tc.node, "", "n1").
					withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
						namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
						namespaceObj("other", nil),
					}})
				root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
				res, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web", "ns1"), NewUsage(&fakeFactory{client: client}, root.roots()),
					input{s: map[string]string{"cluster": "c-abc", "by": by, "project": "p-x", "node": "n1"}})
				require.NoError(t, err)
				assert.Equal(t, []string{"nodes/n1"}, emptyScopeGets(client), "the node is read")
				assert.Equal(t, tc.wantWarnings, res.Warnings)
			})
		}
	}
}
