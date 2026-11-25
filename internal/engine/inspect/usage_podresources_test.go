package inspect

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// resourceList is a ResourceList of the given CPU and memory; an empty
// string leaves that resource out.
func resourceList(cpu, memory string) corev1.ResourceList {
	list := corev1.ResourceList{}
	if cpu != "" {
		list[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		list[corev1.ResourceMemory] = resource.MustParse(memory)
	}
	return list
}

// appContainer is a container with the given CPU and memory requests and
// limits.
func appContainer(name, cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{
		Requests: resourceList(cpuReq, memReq),
		Limits:   resourceList(cpuLim, memLim),
	}}
}

// sidecarContainer is an init container that keeps running beside the app
// containers (restartPolicy Always).
func sidecarContainer(name, cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	c := appContainer(name, cpuReq, memReq, cpuLim, memLim)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always
	return c
}

// podObject returns pod, in namespace ns and named name, as the unstructured
// object a pod list returns.
func podObject(t *testing.T, ns, name string, pod corev1.Pod) unstructured.Unstructured {
	t.Helper()
	pod.Namespace, pod.Name = ns, name
	obj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(&pod)
	require.NoError(t, err)
	return unstructured.Unstructured{Object: obj}
}

// initContainerPod is a pod with an init container that requests 700m and
// 300Mi (limits 900m and 400Mi) and two app containers that each request 100m
// and 50Mi (limits 200m and 60Mi): `kubectl describe node` shows it as 700m
// 900m 300Mi 400Mi.
func initContainerPod(t *testing.T, ns, name, node string) unstructured.Unstructured {
	t.Helper()
	return podObject(t, ns, name, corev1.Pod{Spec: corev1.PodSpec{
		NodeName: node,
		InitContainers: []corev1.Container{
			appContainer("init", "700m", "300Mi", "900m", "400Mi"),
		},
		Containers: []corev1.Container{
			appContainer("a", "100m", "50Mi", "200m", "60Mi"),
			appContainer("b", "100m", "50Mi", "200m", "60Mi"),
		},
	}})
}

// TestSumPodResourcesFollowsTheKubernetesRule asserts the totals of one pod
// are the ones Kubernetes computes: the larger of two figures, the app
// containers added to the restartable init containers, and each init container
// added to the restartable init containers that start before it; pod-level
// resources replacing the container totals; spec.overhead added to the
// requests and to a limit only when it is set; and the resources the pod's
// status reports for a resized container.
func TestSumPodResourcesFollowsTheKubernetesRule(t *testing.T) {
	t.Parallel()
	apps := []corev1.Container{
		appContainer("a", "100m", "50Mi", "200m", "60Mi"),
		appContainer("b", "100m", "50Mi", "200m", "60Mi"),
	}
	for _, tc := range []struct {
		name   string
		pod    corev1.Pod
		cpuReq string
		cpuLim string
		memReq string
		memLim string
	}{
		{
			name:   "no init containers",
			pod:    corev1.Pod{Spec: corev1.PodSpec{Containers: apps}},
			cpuReq: "200m", cpuLim: "400m", memReq: "100Mi", memLim: "120Mi",
		},
		{
			name: "an init container larger than the app containers",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{appContainer("init", "1", "1Gi", "2", "2Gi")},
				Containers:     apps,
			}},
			cpuReq: "1", cpuLim: "2", memReq: "1Gi", memLim: "2Gi",
		},
		{
			name: "an init container smaller than the app containers",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{appContainer("init", "50m", "10Mi", "100m", "20Mi")},
				Containers:     apps,
			}},
			cpuReq: "200m", cpuLim: "400m", memReq: "100Mi", memLim: "120Mi",
		},
		{
			name: "the largest of several init containers",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					appContainer("one", "300m", "100Mi", "300m", "100Mi"),
					appContainer("two", "500m", "200Mi", "600m", "300Mi"),
					appContainer("three", "400m", "150Mi", "400m", "150Mi"),
				},
				Containers: apps,
			}},
			cpuReq: "500m", cpuLim: "600m", memReq: "200Mi", memLim: "300Mi",
		},
		{
			name: "an init container beside two app containers",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{appContainer("init", "700m", "300Mi", "900m", "400Mi")},
				Containers:     apps,
			}},
			cpuReq: "700m", cpuLim: "900m", memReq: "300Mi", memLim: "400Mi",
		},
		{
			name: "a sidecar adds to the app containers and to the init container after it",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecarContainer("sidecar", "100m", "64Mi", "200m", "128Mi"),
					appContainer("init", "500m", "100Mi", "600m", "200Mi"),
				},
				Containers: []corev1.Container{appContainer("app", "200m", "50Mi", "300m", "60Mi")},
			}},
			// The init container runs beside the sidecar: 600m and 164Mi
			// (limits 800m and 328Mi) exceed the 300m and 114Mi (limits 500m
			// and 188Mi) of the app container and the sidecar together.
			cpuReq: "600m", cpuLim: "800m", memReq: "164Mi", memLim: "328Mi",
		},
		{
			name: "a sidecar after an init container adds to the app containers only",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					appContainer("init", "500m", "100Mi", "600m", "200Mi"),
					sidecarContainer("sidecar", "100m", "64Mi", "200m", "128Mi"),
				},
				Containers: []corev1.Container{appContainer("app", "200m", "50Mi", "300m", "60Mi")},
			}},
			// The init container runs before the sidecar starts: its 500m, and
			// its 200Mi limit, exceed the 300m and 188Mi of the app container
			// and the sidecar together; its 100Mi request does not exceed
			// their 114Mi.
			cpuReq: "500m", cpuLim: "600m", memReq: "114Mi", memLim: "200Mi",
		},
		{
			name: "a sidecar and no other init container",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{sidecarContainer("sidecar", "100m", "64Mi", "200m", "128Mi")},
				Containers:     []corev1.Container{appContainer("app", "200m", "50Mi", "300m", "60Mi")},
			}},
			cpuReq: "300m", cpuLim: "500m", memReq: "114Mi", memLim: "188Mi",
		},
		{
			name: "overhead adds to the requests and to a limit that is set",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Overhead:   resourceList("50m", "16Mi"),
				Containers: []corev1.Container{appContainer("app", "100m", "64Mi", "200m", "128Mi")},
			}},
			cpuReq: "150m", cpuLim: "250m", memReq: "80Mi", memLim: "144Mi",
		},
		{
			name: "overhead does not add to a limit that is not set",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Overhead:   resourceList("50m", "16Mi"),
				Containers: []corev1.Container{appContainer("app", "100m", "64Mi", "", "128Mi")},
			}},
			cpuReq: "150m", cpuLim: "0", memReq: "80Mi", memLim: "144Mi",
		},
		{
			name: "overhead with an init container",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Overhead:       resourceList("50m", "16Mi"),
				InitContainers: []corev1.Container{appContainer("init", "700m", "300Mi", "900m", "400Mi")},
				Containers:     apps,
			}},
			cpuReq: "750m", cpuLim: "950m", memReq: "316Mi", memLim: "416Mi",
		},
		{
			name: "pod-level resources replace the container totals",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: resourceList("1", "1Gi"),
					Limits:   resourceList("2", "2Gi"),
				},
				Containers: apps,
			}},
			cpuReq: "1", cpuLim: "2", memReq: "1Gi", memLim: "2Gi",
		},
		{
			name: "the resources the status reports for a resized container",
			pod: corev1.Pod{
				Spec: corev1.PodSpec{Containers: []corev1.Container{appContainer("app", "100m", "64Mi", "200m", "128Mi")}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name: "app",
					Resources: &corev1.ResourceRequirements{
						Requests: resourceList("300m", "256Mi"),
						Limits:   resourceList("400m", "512Mi"),
					},
					AllocatedResources: resourceList("300m", "256Mi"),
				}}},
			},
			cpuReq: "300m", cpuLim: "400m", memReq: "256Mi", memLim: "512Mi",
		},
		{
			name:   "a container with no requests or limits",
			pod:    corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}},
			cpuReq: "0", cpuLim: "0", memReq: "0", memLim: "0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pods := []unstructured.Unstructured{podObject(t, "ns1", "p1", tc.pod)}
			cpuReq, cpuLim, memReq, memLim, err := sumPodResources(pods)
			require.NoError(t, err)
			assert.Equal(t, tc.cpuReq, cpuReq.String(), "CPU request")
			assert.Equal(t, tc.cpuLim, cpuLim.String(), "CPU limit")
			assert.Equal(t, tc.memReq, memReq.String(), "memory request")
			assert.Equal(t, tc.memLim, memLim.String(), "memory limit")
		})
	}
}

// TestSumPodResourcesAddsThePods asserts the totals of several pods are the
// sums of each pod's own totals, not one computation over all their
// containers: the init container of one pod does not combine with another
// pod's containers.
func TestSumPodResourcesAddsThePods(t *testing.T) {
	t.Parallel()
	pods := []unstructured.Unstructured{
		initContainerPod(t, "ns1", "p1", ""),
		initContainerPod(t, "ns2", "p2", ""),
		podObject(t, "ns1", "p3", corev1.Pod{Spec: corev1.PodSpec{
			Containers: []corev1.Container{appContainer("app", "100m", "64Mi", "200m", "128Mi")},
		}}),
	}
	cpuReq, cpuLim, memReq, memLim, err := sumPodResources(pods)
	require.NoError(t, err)
	assert.Equal(t, "1500m", cpuReq.String())
	assert.Equal(t, "2", cpuLim.String())
	assert.Equal(t, "664Mi", memReq.String())
	assert.Equal(t, "928Mi", memLim.String())
}

// TestSumPodResourcesLeavesOutFinishedPods asserts a pod in phase Succeeded
// or Failed adds nothing, as in the pod list of `kubectl describe node` and
// in ResourceQuota, and a pod in any other phase, or with none, counts.
func TestSumPodResourcesLeavesOutFinishedPods(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		phase  corev1.PodPhase
		cpuReq string
	}{
		{corev1.PodRunning, "100m"},
		{corev1.PodPending, "100m"},
		{corev1.PodUnknown, "100m"},
		{"", "100m"},
		{corev1.PodSucceeded, "0"},
		{corev1.PodFailed, "0"},
	} {
		name := "phase " + string(tc.phase)
		if tc.phase == "" {
			name = "no phase"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pod := podObject(t, "ns1", "p1", corev1.Pod{
				Spec:   corev1.PodSpec{Containers: []corev1.Container{appContainer("app", "100m", "64Mi", "200m", "128Mi")}},
				Status: corev1.PodStatus{Phase: tc.phase},
			})
			cpuReq, _, _, _, err := sumPodResources([]unstructured.Unstructured{pod})
			require.NoError(t, err)
			assert.Equal(t, tc.cpuReq, cpuReq.String())
		})
	}
}

// TestSumPodResourcesDoesNotParseAFinishedPod asserts a pod in phase Succeeded
// or Failed is skipped before it is converted: one with a value that does not
// parse is not an error.
func TestSumPodResourcesDoesNotParseAFinishedPod(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"Succeeded", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			bad := podWithResources("demo", "bad", "not-a-quantity", "64Mi", "200m", "128Mi")
			require.NoError(t, unstructured.SetNestedField(bad.Object, phase, "status", "phase"))
			cpuReq, _, _, _, err := sumPodResources([]unstructured.Unstructured{*bad})
			require.NoError(t, err)
			assert.True(t, cpuReq.IsZero())
		})
	}
}

// TestSumPodResourcesNamesAPodItCannotParse asserts a pod with a value that
// does not parse is a GENERIC error that names the pod, never a pod left out
// of the totals.
func TestSumPodResourcesNamesAPodItCannotParse(t *testing.T) {
	t.Parallel()
	bad := podWithResources("demo", "bad", "not-a-quantity", "64Mi", "200m", "128Mi")
	good := initContainerPod(t, "demo", "good", "")
	cpuReq, cpuLim, memReq, memLim, err := sumPodResources([]unstructured.Unstructured{good, *bad})
	require.ErrorContains(t, err, "pod demo/bad has a value this CLI cannot parse")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, cpuReq.IsZero() && cpuLim.IsZero() && memReq.IsZero() && memLim.IsZero(), "an error returns no totals")
}

// TestUsageCountsInitContainersInEveryGrouping asserts a pod with an init
// container shows 700m 900m 300Mi 400Mi in every grouping of `usage` and in
// the usage of `inspect project`: each one gets its totals from
// sumPodResources.
func TestUsageCountsInitContainersInEveryGrouping(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nodeKind := mustKind(t, "node")
	nsKind := mustKind(t, "namespace")
	want := resourceTotals{CPURequest: "700m", CPULimit: "900m", MemoryRequest: "300Mi", MemoryLimit: "400Mi"}
	podList := func(t *testing.T) *unstructured.UnstructuredList {
		t.Helper()
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{initContainerPod(t, "demo", "initpod", "n1")}}
	}
	// requests and limits only: the usage columns depend on the metrics server.
	requestsAndLimits := func(got resourceTotals) resourceTotals {
		return resourceTotals{CPURequest: got.CPURequest, CPULimit: got.CPULimit, MemoryRequest: got.MemoryRequest, MemoryLimit: got.MemoryLimit}
	}
	usageRow := func(t *testing.T, client *fakeClient, roots *steveRoot, flags map[string]string, key string) resourceTotals {
		t.Helper()
		cmd := NewUsage(&fakeFactory{client: client}, roots.roots())
		res, err := execRead(t, cmd, input{s: flags})
		require.NoError(t, err)
		env, ok := res.Data.(usageEnvelope)
		require.True(t, ok)
		require.Contains(t, env.Data, key)
		return requestsAndLimits(env.Data[key])
	}
	t.Run("by namespace", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().withList(podKind.GVR, podList(t)).withList(podMetricsGVR, &unstructured.UnstructuredList{})
		got := usageRow(t, client, nil, map[string]string{"cluster": "c-abc", "by": "namespace"}, "demo")
		assert.Equal(t, want, got)
	})
	t.Run("by pod", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().withList(podKind.GVR, podList(t)).withList(podMetricsGVR, &unstructured.UnstructuredList{})
		got := usageRow(t, client, nil, map[string]string{"cluster": "c-abc", "by": "pod"}, "demo/initpod")
		assert.Equal(t, want, got)
	})
	t.Run("by project", func(t *testing.T) {
		t.Parallel()
		root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
		client := newFakeClient().
			withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{projectNamespaceObj("demo", "c-abc", "p-x")}}).
			withList(podKind.GVR, podList(t)).
			withList(podMetricsGVR, &unstructured.UnstructuredList{})
		got := usageRow(t, client, root, map[string]string{"cluster": "c-abc", "by": "project"}, "p-x")
		assert.Equal(t, want, got)
	})
	t.Run("by node", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().
			withList(nodeKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}).
			withList(podKind.GVR, podList(t)).
			withList(nodeMetricsGVR, &unstructured.UnstructuredList{})
		got := usageRow(t, client, nil, map[string]string{"cluster": "c-abc", "by": "node"}, "n1")
		assert.Equal(t, want, got)
	})
	t.Run("by node with --node", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().
			withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
			withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", "40m", "50Mi")).
			withList(podKind.GVR, podList(t))
		got := usageRow(t, client, nil, map[string]string{"cluster": "c-abc", "by": "node", "node": "n1"}, "n1")
		assert.Equal(t, want, got)
	})
	t.Run("inspect project", func(t *testing.T) {
		t.Parallel()
		fac, root, _, downstream := setUpProjectFixture(t, "")
		downstream.withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{initContainerPod(t, "ns-a", "initpod", "")}})
		cmd := NewInspectProject(fac, root.roots())
		res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
		require.NoError(t, err)
		data, ok := res.Data.(projectResult)
		require.True(t, ok)
		require.NotNil(t, data.Usage)
		assert.Equal(t, want, requestsAndLimits(*data.Usage))
	})
}

// TestUsageFailsForAPodItCannotParse asserts `usage` fails, with the GENERIC
// error that names the pod, when a pod in the list has a value that does not
// parse: its totals would leave the pod out.
func TestUsageFailsForAPodItCannotParse(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			initContainerPod(t, "demo", "good", ""),
			*podWithResources("demo", "bad", "not-a-quantity", "64Mi", "200m", "128Mi"),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})
	cmd := NewUsage(&fakeFactory{client: client}, nil)
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.ErrorContains(t, err, "pod demo/bad has a value this CLI cannot parse")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

// TestUsageNamesTheSameUnparsablePodEveryRun asserts that when pods in two
// groups cannot be parsed, `usage` names the pod of the group that sorts
// first, in every run, with `--by namespace` and with `--by pod`.
func TestUsageNamesTheSameUnparsablePodEveryRun(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	for _, by := range []string{"namespace", "pod"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			for range 20 {
				client := newFakeClient().
					withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
						*podWithResources("zeta", "bad", "not-a-quantity", "64Mi", "200m", "128Mi"),
						*podWithResources("alpha", "bad", "not-a-quantity", "64Mi", "200m", "128Mi"),
						*podWithResources("mid", "bad", "not-a-quantity", "64Mi", "200m", "128Mi"),
					}}).
					withList(podMetricsGVR, &unstructured.UnstructuredList{})
				cmd := NewUsage(&fakeFactory{client: client}, nil)
				_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": by}})
				require.ErrorContains(t, err, "pod alpha/bad has a value this CLI cannot parse")
			}
		})
	}
}

// TestInspectProjectUsageDegradesForAPodItCannotParse asserts `inspect
// project` keeps its namespaces, members and resource quota, and reports the
// usage section as unavailable with a warning that names the pod, when a pod
// in the list has a value that does not parse.
func TestInspectProjectUsageDegradesForAPodItCannotParse(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	fac, root, _, downstream := setUpProjectFixture(t, "")
	downstream.withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		initContainerPod(t, "ns-a", "good", ""),
		*podWithResources("ns-a", "bad", "not-a-quantity", "64Mi", "200m", "128Mi"),
	}})
	cmd := NewInspectProject(fac, root.roots())
	res, err := execRead(t, cmd, input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-test"}})
	require.NoError(t, err)
	data, ok := res.Data.(projectResult)
	require.True(t, ok)
	assert.Nil(t, data.Usage)
	assert.Len(t, data.Namespaces, 2)
	assert.Len(t, data.Members, 2)
	assert.Equal(t, map[string]any{"limit": map[string]any{"pods": "10"}}, data.ResourceQuota)
	assert.Equal(t, []sectionOutcome{{Section: sectionUsage, State: stateUnavailable, Code: clierr.CodeGeneric}}, data.Degraded)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "usage unavailable: pod ns-a/bad has a value this CLI cannot parse")
}
