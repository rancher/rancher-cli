package inspect

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

var rsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}

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

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	require.Len(t, rows, 1)
	assert.Equal(t, imageWorkloadRow{
		Image: "nginx:1.25", Namespace: "web",
		Workload: workloadIdentity{APIVersion: "apps/v1", Kind: "deployment", Name: "web", UID: "dep-1"},
		Pods:     3,
	}, rows[0])
	assert.Len(t, client.getCalls, 1, "three replicas, one memoized ReplicaSet fetch (IM-15)")
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

	rows, _, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 2, "same-name generations must not collapse (IM-17)")
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

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{
		APIVersion: "apps/v1", Kind: "replicaset", Name: "gone-rs", UID: "rs-gone",
	}, rows[0].Workload, "failure keeps the first-level ref (IM-16)")
	assert.Equal(t, 2, rows[0].Pods)
	require.Len(t, warns, 1, "memoization dedupes the warning")
	assert.Equal(t, "owner web/replicaset.apps/gone-rs unavailable: boom", warns[0])
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

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
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

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, warns, 1)
	require.Len(t, rows, 1)
	assert.Equal(t, workloadIdentity{
		APIVersion: "apps/v1", Kind: "replicaset", Name: "mid-rs", UID: "rs-1",
	}, rows[0].Workload, "a failure past the first hop still falls back to the FIRST-level ref (IM-16)")
}

func TestWorkloadImagesMemoKeyIsolatesUID(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	// Two pods reference the SAME ReplicaSet name with different uids: a
	// stale pod from a deleted generation and a live one. The fetched
	// object carries the new uid, so the stale ref must uid-mismatch and
	// stand as its own first-level row — two rows, two resolutions.
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		ownedPod("stale", "web", "app:1", "app-rs", "rs-OLD"),
		ownedPod("live", "web", "app:2", "app-rs", "rs-NEW"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, list).
		withObject(rsGVR, "web", "app-rs", replicaSet("web", "app-rs", "rs-NEW", "app", "dep-1"))

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 2, "identical ns/kind/name with distinct uids must not share a memo entry (IM-15)")
	assert.Len(t, client.getCalls, 2, "one resolution per distinct owner reference")
	require.Len(t, warns, 1, "the stale reference uid-mismatches")
	uids := []string{rows[0].Workload.UID, rows[1].Workload.UID}
	assert.Contains(t, uids, "rs-OLD", "stale generation keeps its first-level identity")
	assert.Contains(t, uids, "dep-1", "live generation resolves to its Deployment")
}

func TestWorkloadImagesIdentityIsolatesAPIVersion(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	// Everything held constant — namespace, kind, name, uid (synthetic
	// duplicate, deliberately), image, terminal shape (non-recursable kind,
	// so neither owner is fetched) — EXCEPT the API group. Two rows with
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

	rows, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	assert.Empty(t, warns)
	assert.Empty(t, client.getCalls, "non-recursable owners are never fetched")
	require.Len(t, rows, 2, "identical except API group must stay two rows (IM-15/IM-17)")
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

	rows, _, err := workloadImagesFor(context.Background(), client, []string{"drop-me"})
	require.NoError(t, err)
	require.Len(t, rows, 1, "ephemeral-container image is visible to workload grouping (IM-6)")
	assert.Equal(t, "drop-me:1", rows[0].Image, "--match filters inside the workload path (IM-3)")

	rows, _, err = workloadImagesFor(context.Background(), client, []string{"keep-me", "drop-me"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "repeatable needles OR through workload grouping (IM-3)")
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

	_, warns, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, warns, 2)
	assert.Contains(t, warns, "owner ns-a/replicaset.apps/same-name unavailable: boom")
	assert.Contains(t, warns, "owner ns-b/replicaset.apps/same-name unavailable: boom")
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

	rows, _, err := workloadImagesFor(context.Background(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, rows[0].Pods, "one pod, twice-referenced image, pods = 1 (IM-17)")
}
