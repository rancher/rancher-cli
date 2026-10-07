package inspect

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

var rsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}

// workloadImagesWith runs the scan with its own accumulator and returns what
// a caller would publish: the rows plus the finalized degradation entries and
// their paired warnings.
func workloadImagesWith(ctx context.Context, client engine.ClusterClient, clusterID string, needles []string) (imageWorkloadRows, []sectionOutcome, []string, error) {
	sec := newSections(ctx, sectionOwnerResolution)
	rows, err := workloadImagesFor(ctx, client, sec, clusterID, needles)
	degraded, warns := sec.Finalize()
	return rows, degraded, warns, err
}

func ownedPod(name, ns, image, rsName, rsUID string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": ns, "uid": "pod-uid-" + name,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"name": rsName, "uid": rsUID, "controller": true,
			}},
		},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": image}},
		},
		"status": map[string]any{"phase": "Running"},
	}}
}

func replicaSet(ns, name, uid, depName, depUID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "uid": uid,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"name": depName, "uid": depUID, "controller": true,
			}},
		},
	}}
}

func TestWorkloadImagesEmptyPodListYieldsNonNilEmpty(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{})
	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.NotNil(t, rows, "non-nil even for an empty pod list")
	assert.Len(t, rows, 0)
	assert.Empty(t, warns)
}

func TestWorkloadImagesMalformedOwnerRefsFallsBackToPod(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	pod := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "bare", "namespace": "ns", "uid": "pod-uid-1"},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": "img:1"}},
		},
		"status": map[string]any{"phase": "Running"},
	}}
	// ownerReferences: one non-map entry, one map missing both kind and name.
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		"not-a-map",
		map[string]any{"apiVersion": "apps/v1"},
	}, "metadata", "ownerReferences"))
	client := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}})

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Empty(t, client.getCalls, "no Get calls for unresolvable owner references")
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{APIVersion: "v1", Kind: "pod", Name: "bare", UID: "pod-uid-1"}, rows[0].Workload)
}

func TestWorkloadImagesResolvesAndMemoizes(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("web-1", "web", "nginx:1.25", "web-rs", "rs-1"),
		ownedPod("web-2", "web", "nginx:1.25", "web-rs", "rs-1"),
		ownedPod("web-3", "web", "nginx:1.25", "web-rs", "rs-1"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withObject(rsGVR, "web", "web-rs", replicaSet("web", "web-rs", "rs-1", "web", "dep-1"))

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	require.Len(t, rows, 1)
	assert.Equal(t, imageWorkloadRow{
		Image: "nginx:1.25", Namespace: "web",
		Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "web", UID: "dep-1"},
		Pods:     3,
	}, rows[0])
	assert.Len(t, client.getCalls, 1, "three replicas, one memoized ReplicaSet fetch")
}

func TestWorkloadImagesGenerationsStaySeparate(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("old-1", "web", "app:1", "old-rs", "rs-old"),
		ownedPod("new-1", "web", "app:2", "new-rs", "rs-new"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withObject(rsGVR, "web", "old-rs", replicaSet("web", "old-rs", "rs-old", "app", "dep-GEN1")).
		withObject(rsGVR, "web", "new-rs", replicaSet("web", "new-rs", "rs-new", "app", "dep-GEN2"))

	rows, _, _, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, rows, 2, "same-name generations must not collapse")
	assert.NotEqual(t, rows[0].Workload.UID, rows[1].Workload.UID)
}

func TestWorkloadImagesDegradesToFirstLevelRef(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("p1", "web", "app:1", "gone-rs", "rs-gone"),
		ownedPod("p2", "web", "app:1", "gone-rs", "rs-gone"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withGetErr(rsGVR, "web", "gone-rs", fmt.Errorf("boom"))

	rows, degraded, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{
		APIVersion: "apps/v1", Kind: "replicaset", Name: "gone-rs", UID: "rs-gone",
	}, rows[0].Workload, "failure keeps the first-level ref")
	assert.Equal(t, 2, rows[0].Pods)
	require.Len(t, warns, 1, "memoization dedupes the warning")
	assert.Equal(t, "ownerResolution unavailable for apps/v1|ReplicaSet|web|gone-rs|rs-gone: boom", warns[0])
	require.Len(t, degraded, 1)
	assert.Equal(t, sectionOutcome{
		Section: sectionOwnerResolution, State: statePartial,
		Code: clierr.CodeGeneric, Target: "apps/v1|ReplicaSet|web|gone-rs|rs-gone",
	}, degraded[0])
}

// TestWorkloadImagesOwnerRefWithDelimiterIsEscaped verifies the owner walk
// survives an ownerReference this CLI never validated: a name carrying the
// target delimiter degrades to an escaped target, never a crash.
func TestWorkloadImagesOwnerRefWithDelimiterIsEscaped(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("p1", "web", "app:1", "od|d-rs", "rs-odd"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withGetErr(rsGVR, "web", "od|d-rs", fmt.Errorf("boom"))

	rows, degraded, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Len(t, degraded, 1)
	assert.Equal(t, "apps/v1|ReplicaSet|web|od%7Cd-rs|rs-odd", degraded[0].Target)
	assert.Equal(t, []string{
		"ownerResolution unavailable for apps/v1|ReplicaSet|web|od%7Cd-rs|rs-odd: boom",
	}, warns)
}

func TestWorkloadImagesOwnerlessPodFallback(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	bare := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "bare", "namespace": "default", "uid": "bare-uid"},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": "busybox"}},
		},
		"status": map[string]any{"phase": "Running"},
	}}
	client := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{bare}})

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{APIVersion: "v1", Kind: "pod", Name: "bare", UID: "bare-uid"}, rows[0].Workload)
}

func TestWorkloadImagesDeepWalkFailureKeepsFirstLevelRef(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	stsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	rs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name": "mid-rs", "namespace": "web", "uid": "rs-1",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "StatefulSet",
				"name": "deep-sts", "uid": "sts-1", "controller": true,
			}},
		},
	}}
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			ownedPod("p", "web", "app:1", "mid-rs", "rs-1"),
		}}).
		withObject(rsGVR, "web", "mid-rs", rs).
		withGetErr(stsGVR, "web", "deep-sts", fmt.Errorf("boom"))

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, warns, 1)
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{
		APIVersion: "apps/v1", Kind: "replicaset", Name: "mid-rs", UID: "rs-1",
	}, rows[0].Workload, "a failure past the first hop still falls back to the FIRST-level ref")
}

func TestWorkloadImagesMemoKeyIsolatesUID(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	// Two pods reference the SAME ReplicaSet name with different uids: a
	// stale pod from a deleted generation and a live one. The fetched
	// object carries the new uid, so the stale ref must uid-mismatch and
	// stand as its own first-level row - two rows, two resolutions.
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("stale", "web", "app:1", "app-rs", "rs-OLD"),
		ownedPod("live", "web", "app:2", "app-rs", "rs-NEW"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withObject(rsGVR, "web", "app-rs", replicaSet("web", "app-rs", "rs-NEW", "app", "dep-1"))

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, rows, 2, "identical ns/kind/name with distinct uids must not share a memo entry")
	assert.Len(t, client.getCalls, 2, "one resolution per distinct owner reference")
	require.Len(t, warns, 1, "the stale reference uid-mismatches")
	uids := []string{rows[0].Workload.UID, rows[1].Workload.UID}
	assert.Contains(t, uids, "rs-OLD", "stale generation keeps its first-level identity")
	assert.Contains(t, uids, "dep-1", "live generation resolves to its Deployment")
}

func TestWorkloadImagesIdentityIsolatesAPIVersion(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	// Everything held constant - namespace, kind, name, uid (synthetic
	// duplicate, deliberately), image, terminal shape (non-recursable kind,
	// so neither owner is fetched) - EXCEPT the API group. Two rows with
	// exact identities proves apiVersion alone separates the memo and
	// aggregation keys; a key that dropped apiVersion would collapse them.
	withOwner := func(podName, apiVersion string) unstructured.Unstructured {
		p := ownedPod(podName, "web", "app:1", "same-name", "u-1")
		require.NoError(t, unstructured.SetNestedSlice(p.Object, []any{map[string]any{
			"apiVersion": apiVersion, "kind": "CustomApp",
			"name": "same-name", "uid": "u-1", "controller": true,
		}}, "metadata", "ownerReferences"))
		return p
	}
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		withOwner("a", "widgets.io/v1"),
		withOwner("b", "gadgets.io/v1"),
	}}
	client := newFakeClient().withList(podKind.GVR, list)

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Empty(t, client.getCalls, "non-recursable owners are never fetched")
	require.Len(t, rows, 2, "identical except API group must stay two rows")
	want := []workloadIdentity{
		{APIVersion: "gadgets.io/v1", Kind: "customapp", Name: "same-name", UID: "u-1"},
		{APIVersion: "widgets.io/v1", Kind: "customapp", Name: "same-name", UID: "u-1"},
	}
	got := []workloadIdentity{rows[0].Workload, rows[1].Workload}
	assert.ElementsMatch(t, want, got)
	assert.Equal(t, 1, rows[0].Pods)
	assert.Equal(t, 1, rows[1].Pods)
}

func TestWorkloadImagesMatchAndEphemeralThroughGrouping(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	pod := ownedPod("p", "web", "keep-me:1", "rs", "rs-1")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "debug", "image": "drop-me:1"},
	}, "spec", "ephemeralContainers"))
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}).
		withObject(rsGVR, "web", "rs", replicaSet("web", "rs", "rs-1", "app", "dep-1"))

	rows, _, _, err := workloadImagesWith(t.Context(), client, "", []string{"drop-me"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "ephemeral-container image is visible to workload grouping")
	assert.Equal(t, "drop-me:1", rows[0].Image, "--match filters inside the workload path")

	rows, _, _, err = workloadImagesWith(t.Context(), client, "", []string{"keep-me", "drop-me"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "repeatable needles OR through workload grouping")
	images := []string{rows[0].Image, rows[1].Image}
	assert.ElementsMatch(t, []string{"keep-me:1", "drop-me:1"}, images)
}

func TestWorkloadImagesWarningsDistinctAcrossNamespaces(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("p1", "ns-a", "app:1", "same-name", "u1"),
		ownedPod("p2", "ns-b", "app:1", "same-name", "u2"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withGetErr(rsGVR, "ns-a", "same-name", fmt.Errorf("boom")).
		withGetErr(rsGVR, "ns-b", "same-name", fmt.Errorf("boom"))

	_, degraded, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, warns, 2)
	assert.Contains(t, warns, "ownerResolution unavailable for apps/v1|ReplicaSet|ns-a|same-name|u1: boom")
	assert.Contains(t, warns, "ownerResolution unavailable for apps/v1|ReplicaSet|ns-b|same-name|u2: boom")
	require.Len(t, degraded, 2)
	assert.Equal(t, "apps/v1|ReplicaSet|ns-a|same-name|u1", degraded[0].Target)
	assert.Equal(t, "apps/v1|ReplicaSet|ns-b|same-name|u2", degraded[1].Target)
}

func TestWorkloadImagesPerPodImageDedup(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	pod := ownedPod("p", "web", "shared-img", "rs", "rs-1")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "init", "image": "shared-img"},
	}, "spec", "initContainers"))
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}).
		withObject(rsGVR, "web", "rs", replicaSet("web", "rs", "rs-1", "app", "dep-1"))

	rows, _, _, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, rows[0].Pods, "one pod, twice-referenced image, pods = 1")
}

func TestWorkloadRowsTableDisambiguatesGenerations(t *testing.T) {
	t.Parallel()
	rows := imageWorkloadRows{
		{Image: "app:1", Namespace: "web",
			Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "app", UID: "aaaaaaaa-1111"}, Pods: 2},
		{Image: "app:2", Namespace: "web",
			Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "app", UID: "bbbbbbbb-2222"}, Pods: 3},
		{Image: "other:1", Namespace: "web",
			Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "solo", UID: "cccccccc-3333"}, Pods: 1},
	}
	got := rows.Rows()
	require.Len(t, got, 3)
	assert.Equal(t, "deployment.apps/app@aaaaaaaa", got[0][2],
		"different-image generations still get the suffix")
	assert.Equal(t, "deployment.apps/app@bbbbbbbb", got[1][2])
	assert.Equal(t, "deployment.apps/solo", got[2][2], "single generation, no suffix")
}

func TestWorkloadRowsSuffixGrowsPastSharedPrefix(t *testing.T) {
	t.Parallel()
	rows := imageWorkloadRows{
		{Image: "a", Namespace: "n",
			Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "x", UID: "aaaaaaaa-one"}, Pods: 1},
		{Image: "a", Namespace: "n",
			Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "x", UID: "aaaaaaaa-two"}, Pods: 1},
	}
	got := rows.Rows()
	assert.Equal(t, "deployment.apps/x@aaaaaaaa-o", got[0][2],
		"shared 8-char prefix forces the shortest unique prefix")
	assert.Equal(t, "deployment.apps/x@aaaaaaaa-t", got[1][2])
}

func TestClusterImageWorkloadsTable(t *testing.T) {
	t.Parallel()
	c := clusterImageWorkloads{
		"c-b": {{Image: "i", Namespace: "n",
			Workload: workloadIdentity{APIVersion: "v1", Kind: "pod", Name: "p", UID: "u"}, Pods: 1}},
		"c-a": {{Image: "i", Namespace: "n",
			Workload: workloadIdentity{APIVersion: "v1", Kind: "pod", Name: "p", UID: "u"}, Pods: 1}},
	}
	got := c.Rows()
	require.Len(t, got, 2)
	assert.Equal(t, "c-a", got[0][0], "cluster ids sorted")
	assert.Equal(t, []string{"CLUSTER", "IMAGE", "NAMESPACE", "WORKLOAD", "PODS"}, c.Columns())
}

func TestWorkloadImagesSkipsInactivePods(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")

	running := ownedPod("running-pod", "default", "img-a", "phase-rs", "rs-1")

	pending := ownedPod("pending-pod", "default", "img-b", "phase-rs", "rs-1")
	require.NoError(t, unstructured.SetNestedField(pending.Object, "Pending", "status", "phase"))

	succeeded := ownedPod("done-pod", "default", "img-gone", "phase-rs", "rs-1")
	require.NoError(t, unstructured.SetNestedField(succeeded.Object, "Succeeded", "status", "phase"))

	failed := ownedPod("failed-pod", "default", "img-gone", "phase-rs", "rs-1")
	require.NoError(t, unstructured.SetNestedField(failed.Object, "Failed", "status", "phase"))

	// Terminating: phase stays Running until the pod leaves the List.
	terminating := ownedPod("terminating-pod", "default", "img-c", "phase-rs", "rs-1")
	require.NoError(t, unstructured.SetNestedField(terminating.Object, "2026-07-26T00:00:00Z", "metadata", "deletionTimestamp"))

	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		running, pending, succeeded, failed, terminating,
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withObject(rsGVR, "default", "phase-rs", replicaSet("default", "phase-rs", "rs-1", "phase-dep", "dep-1"))

	rows, _, warns, err := workloadImagesWith(t.Context(), client, "", nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	require.Len(t, rows, 3, "Succeeded and Failed pods must not contribute rows")

	var images []string
	for _, r := range rows {
		images = append(images, r.Image)
	}
	assert.ElementsMatch(t, []string{"img-a", "img-b", "img-c"}, images,
		"Running, Pending, and terminating pods all contribute; Succeeded/Failed do not")
	assert.NotContains(t, images, "img-gone", "completed-job images must not appear in the inventory")
}

func TestAllClustersWorkloadDegradationTargetedAndSorted(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	roots := clustersRoot(t, "c-bbb", "c-aaa").roots()
	// c-aaa: owner walk fails -> per-cluster warning. c-bbb: whole-cluster failure.
	aClient := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			ownedPod("p", "web", "app:1", "gone-rs", "rs-gone"),
		}}).
		withGetErr(rsGVR, "web", "gone-rs", fmt.Errorf("boom"))
	factory := &fakeFactory{
		perCluster: map[string]engine.ClusterClient{
			"c-aaa": aClient,
		},
		perClusterErr: map[string]error{"c-bbb": fmt.Errorf("unreachable")},
	}

	cmd := NewImages(factory, roots).(*imagesCmd)
	res, err := cmd.allClusters(t.Context(), byWorkload, nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"clusters unavailable for c-bbb: unreachable",
		"ownerResolution unavailable for c-aaa|apps/v1|ReplicaSet|web|gone-rs|rs-gone: boom",
	}, res.Warnings, "entries sort by section then target, and warnings follow them")
	data, ok := res.Data.(imagesResult)
	require.True(t, ok)
	require.Equal(t, []sectionOutcome{
		{Section: sectionClusters, State: statePartial, Code: clierr.CodeGeneric, Target: "c-bbb"},
		{Section: sectionOwnerResolution, State: statePartial, Code: clierr.CodeGeneric,
			Target: "c-aaa|apps/v1|ReplicaSet|web|gone-rs|rs-gone"},
	}, data.Degraded, "an all-clusters owner target carries its cluster prefix")
}
