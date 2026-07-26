package inspect

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podWithImages(ns, name string, containers, initContainers []string) *unstructured.Unstructured {
	toSlice := func(images []string) []any {
		out := make([]any, len(images))
		for i, img := range images {
			out[i] = map[string]any{"name": img, "image": img}
		}
		return out
	}
	spec := map[string]any{"containers": toSlice(containers)}
	if len(initContainers) > 0 {
		spec["initContainers"] = toSlice(initContainers)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     spec,
		"status":   map[string]any{"phase": "Running"},
	}}
}

func podWithPhase(name, phase string, image string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": image}},
		},
		"status": map[string]any{"phase": phase},
	}}
}

func managementClusterObj(id string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
	}}
}

func TestImagesSingleClusterDedupes(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
		*podWithImages("ns1", "p2", []string{"nginx:1"}, []string{"busybox"}),
	}}
	client := newFakeClient().withList(podKind.GVR, pods)

	cmd := NewImages(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	assert.Equal(t, "ClusterImages", res.Kind)
	rows, ok := res.Data.(imageRows)
	require.True(t, ok)
	require.Len(t, rows, 2)
	assert.Equal(t, imageCount{Image: "busybox", Count: 1}, rows[0])
	assert.Equal(t, imageCount{Image: "nginx:1", Count: 2}, rows[1])
}

func TestImagesAllClustersFansOut(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	clusterKind := mustKind(t, "cluster")

	mgmt := newFakeClient().withList(clusterKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		managementClusterObj("c-a"), managementClusterObj("c-b"),
	}})
	clientA := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
	}})
	clientB := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"redis:7"}, nil),
	}})

	factory := &fakeFactory{
		client: mgmt,
		perCluster: map[string]engine.ClusterClient{
			"c-a": clientA,
			"c-b": clientB,
		},
	}

	cmd := NewImages(factory)
	res, err := execRead(t, cmd, input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)

	assert.Equal(t, "ClusterImages", res.Kind)
	assert.Empty(t, res.Warnings)
	perCluster, ok := res.Data.(clusterImages)
	require.True(t, ok)
	require.Contains(t, perCluster, "c-a")
	require.Contains(t, perCluster, "c-b")
	assert.Equal(t, imageRows{{Image: "nginx:1", Count: 1}}, perCluster["c-a"])
	assert.Equal(t, imageRows{{Image: "redis:7", Count: 1}}, perCluster["c-b"])
}

func TestImagesOneClusterFailingDegrades(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	clusterKind := mustKind(t, "cluster")

	mgmt := newFakeClient().withList(clusterKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		managementClusterObj("c-a"), managementClusterObj("c-b"),
	}})
	clientA := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
	}})
	clientB := newFakeClient().withListErr(podKind.GVR, errors.New("connection refused"))

	factory := &fakeFactory{
		client: mgmt,
		perCluster: map[string]engine.ClusterClient{
			"c-a": clientA,
			"c-b": clientB,
		},
	}

	cmd := NewImages(factory)
	res, err := execRead(t, cmd, input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)

	perCluster, ok := res.Data.(clusterImages)
	require.True(t, ok)
	require.Contains(t, perCluster, "c-a")
	require.NotContains(t, perCluster, "c-b")
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "cluster c-b: connection refused")
}

func TestImagesClusterAndAllClustersConflict(t *testing.T) {
	t.Parallel()
	cmd := NewImages(&fakeFactory{client: newFakeClient()})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}, b: map[string]bool{"all-clusters": true}})
	require.Error(t, err)
}

func TestImagesSkipsInactivePods(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podWithPhase("running-pod", "Running", "img-a"),
		podWithPhase("pending-pod", "Pending", "img-b"),
		podWithPhase("done-pod", "Succeeded", "img-gone"),
		podWithPhase("failed-pod", "Failed", "img-gone"),
	}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(context.Background(), client)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "img-a", rows[0].Image)
	assert.Equal(t, "img-b", rows[1].Image)
}

func TestImagesIncludesTerminatingRunningPod(t *testing.T) {
	t.Parallel()
	pod := podWithPhase("terminating", "Running", "img-t")
	require.NoError(t, unstructured.SetNestedField(pod.Object, "2026-07-26T00:00:00Z", "metadata", "deletionTimestamp"))
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(context.Background(), client)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a terminating pod stays included until it leaves the List (IM-14)")
	assert.Equal(t, "img-t", rows[0].Image)
}

func TestImagesCountsEphemeralContainers(t *testing.T) {
	t.Parallel()
	pod := podWithPhase("p", "Running", "app-img")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "debug", "image": "debug-img"},
	}, "spec", "ephemeralContainers"))
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(context.Background(), client)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "app-img", rows[0].Image)
	assert.Equal(t, "debug-img", rows[1].Image)
}

func TestImagesCountIsPerReference(t *testing.T) {
	t.Parallel()
	pod := podWithPhase("p", "Running", "shared-img")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "init", "image": "shared-img"},
	}, "spec", "initContainers"))
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(context.Background(), client)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Count, "same image in init and app container is two references (IM-18)")
}
