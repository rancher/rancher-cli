package inspect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sort"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
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

func TestImagesSingleClusterDedupes(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
		*podWithImages("ns1", "p2", []string{"nginx:1"}, []string{"busybox"}),
	}}
	client := newFakeClient().withList(podKind.GVR, pods)

	cmd := NewImages(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	assert.Equal(t, "ClusterImages", res.Kind)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	assert.Equal(t, byImage, env.By)
	rows, ok := env.RowData.(imageRows)
	require.True(t, ok)
	require.Len(t, rows, 2)
	assert.Equal(t, imageCount{Image: "busybox", Count: 1}, rows[0])
	assert.Equal(t, imageCount{Image: "nginx:1", Count: 2}, rows[1])
}

func TestImagesAllClustersFansOut(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	roots := clustersRoot(t, "c-a", "c-b").roots()
	clientA := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
	}})
	clientB := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"redis:7"}, nil),
	}})

	factory := &fakeFactory{
		perCluster: map[string]engine.ClusterClient{
			"c-a": clientA,
			"c-b": clientB,
		},
	}

	cmd := NewImages(factory, roots)
	res, err := execRead(t, cmd, input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)

	assert.Equal(t, "ClusterImages", res.Kind)
	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	assert.Equal(t, byImage, env.By)
	perCluster, ok := env.Clusters.(clusterImages)
	require.True(t, ok)
	require.Contains(t, perCluster, "c-a")
	require.Contains(t, perCluster, "c-b")
	assert.Equal(t, imageRows{{Image: "nginx:1", Count: 1}}, perCluster["c-a"])
	assert.Equal(t, imageRows{{Image: "redis:7", Count: 1}}, perCluster["c-b"])
}

func TestImagesOneClusterFailingDegrades(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	roots := clustersRoot(t, "c-a", "c-b").roots()
	clientA := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
	}})
	clientB := newFakeClient().withListErr(podKind.GVR, errors.New("connection refused"))

	factory := &fakeFactory{
		perCluster: map[string]engine.ClusterClient{
			"c-a": clientA,
			"c-b": clientB,
		},
	}

	cmd := NewImages(factory, roots)
	res, err := execRead(t, cmd, input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)

	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	assert.Equal(t, byImage, env.By)
	perCluster, ok := env.Clusters.(clusterImages)
	require.True(t, ok)
	require.Contains(t, perCluster, "c-a")
	require.NotContains(t, perCluster, "c-b")
	require.Equal(t, []string{"clusters unavailable for c-b: connection refused"}, res.Warnings)
	require.Equal(t, []sectionOutcome{
		{Section: sectionClusters, State: statePartial, Code: clierr.CodeGeneric, Target: "c-b"},
	}, env.Degraded)
}

// TestImagesSingleClusterPodListDeniedIsHardRBAC verifies the required
// collector for a single-cluster scan: the pod list is the whole answer, so a
// denial is the command's error, not a degraded envelope with no rows.
func TestImagesSingleClusterPodListDeniedIsHardRBAC(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "pods is forbidden"))

	cmd := NewImages(&fakeFactory{client: client}, nil)
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Nil(t, res.Data, "a required collector's denial yields no payload to degrade")
	assert.Empty(t, res.Warnings)
}

func TestImagesClusterAndAllClustersConflict(t *testing.T) {
	t.Parallel()
	cmd := NewImages(&fakeFactory{client: newFakeClient()}, nil)
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
	rows, err := imagesFor(t.Context(), client, nil)
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
	rows, err := imagesFor(t.Context(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a terminating pod stays included until it leaves the List")
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
	rows, err := imagesFor(t.Context(), client, nil)
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
	rows, err := imagesFor(t.Context(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Count, "same image in init and app container is two references")
}

func TestImagesMatchFiltersAndORs(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podWithPhase("a", "Running", "registry.corp/web/nginx:1.25"),
		podWithPhase("b", "Running", "registry.old.corp/legacy/app:1"),
		podWithPhase("c", "Running", "quay.io/tools/busybox:stable"),
	}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)

	rows, err := imagesFor(t.Context(), client, []string{"registry.old.corp/", "/nginx:"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "values OR: old-registry image plus the nginx repo")
	assert.Equal(t, "registry.corp/web/nginx:1.25", rows[0].Image)
	assert.Equal(t, "registry.old.corp/legacy/app:1", rows[1].Image)

	rows, err = imagesFor(t.Context(), client, nil)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "no needles matches everything")
}

func TestImagesEmptyMatchRejected(t *testing.T) {
	t.Parallel()
	cmd := NewImages(&fakeFactory{client: newFakeClient()}, nil)
	_, err := execRead(t, cmd, input{
		s:  map[string]string{},
		ss: map[string][]string{"match": {""}},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestImagesByWorkloadDispatch(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			ownedPod("p1", "web", "nginx:1.25", "rs", "rs-1"),
		}}).
		withObject(rsGVR, "web", "rs", replicaSet("web", "rs", "rs-1", "web", "dep-1"))
	factory := &fakeFactory{client: client}

	cmd := NewImages(factory, nil)
	res, err := execRead(t, cmd, input{
		s: map[string]string{"by": "workload", "cluster": "c-test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "ClusterImages", res.Kind)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	assert.Equal(t, "workload", env.By)
	rows, ok := env.RowData.(imageWorkloadRows)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, "dep-1", rows[0].Workload.UID)
}

func TestImagesDescriptorDeclaresByEnumAndMatchSlice(t *testing.T) {
	t.Parallel()
	d := NewImages(&fakeFactory{}, nil).Describe()
	flags := map[string]runtime.ArgSpec{}
	for _, f := range d.Flags {
		flags[f.Name] = f
	}
	require.Contains(t, flags, "by")
	assert.Equal(t, []string{"image", "workload"}, flags["by"].Enum,
		"pipeline Enum validation and static completion both hang off this")
	require.Contains(t, flags, "match")
	assert.Equal(t, "stringSlice", flags["match"].Type,
		"a plain string flag would keep one occurrence")
	assert.NotEmpty(t, d.Desc, "long-form semantics live on the schema-only Descriptor.Desc")
	assert.NotEmpty(t, d.Examples, "blast-radius recipe ships in the schema")
}

func TestImagesMatchNothingYieldsNonNilEmpty(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podWithPhase("a", "Running", "nginx:1.25"),
	}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(t.Context(), client, []string{"no-such-ref"})
	require.NoError(t, err)
	require.NotNil(t, rows, "non-nil even when no images match")
	assert.Len(t, rows, 0)
}

func TestImagesEmptyPodListYieldsNonNilEmpty(t *testing.T) {
	t.Parallel()
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{})
	rows, err := imagesFor(t.Context(), client, nil)
	require.NoError(t, err)
	require.NotNil(t, rows, "non-nil even for an empty pod list")
	assert.Len(t, rows, 0)
}

func TestImagesEphemeralSharingAppImageCountsTwo(t *testing.T) {
	t.Parallel()
	pod := podWithPhase("p", "Running", "shared-img")
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{"name": "debug", "image": "shared-img"},
	}, "spec", "ephemeralContainers"))
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}}
	podKind, _ := kinds.Lookup("pod")
	client := newFakeClient().withList(podKind.GVR, list)
	rows, err := imagesFor(t.Context(), client, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Count, "ephemeral container sharing the app image is a second reference")
}

func TestImagesResultZeroValueNilSafe(t *testing.T) {
	t.Parallel()
	var r imagesResult
	assert.Nil(t, r.Columns(), "zero-value Columns() must return nil without panicking")
	assert.Nil(t, r.Rows(), "zero-value Rows() must return nil without panicking")
}

func TestImagesInvalidByRejectedByPipeline(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), NewImages(&fakeFactory{client: newFakeClient()}, nil),
		input{s: map[string]string{"by": "bogus"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "json"}, safety.Binding{})
	require.NotNil(t, ce, "the pipeline must reject a non-enum --by before Execute")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// allClustersFactory builds the --all-clusters fixture: the steve cluster
// collection listing the ids, and one downstream client per id.
func allClustersFactory(t *testing.T, downstream map[string]engine.ClusterClient) (*fakeFactory, engine.RootClientFactory) {
	t.Helper()
	ids := make([]string, 0, len(downstream))
	for id := range downstream {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return &fakeFactory{perCluster: downstream}, clustersRoot(t, ids...).roots()
}

// TestImagesAllClustersListsClustersThroughSteve asserts the cluster ids come
// from the steve collection: a local client whose management cluster LIST is
// refused still gets the fan-out, and the only root read is the collection.
func TestImagesAllClustersListsClustersThroughSteve(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	clusterKind := mustKind(t, "cluster")
	local := newFakeClient().withListErr(clusterKind.GVR, clierr.New(clierr.CodeRBAC, "clusters is forbidden"))
	factory := &fakeFactory{
		client: local,
		perCluster: map[string]engine.ClusterClient{
			"c-a": newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
			}}),
			"c-b": newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*podWithImages("ns1", "p1", []string{"redis:7"}, nil),
			}}),
		},
	}
	root := clustersRoot(t, "c-a", "c-b")

	res, err := execRead(t, NewImages(factory, root.roots()), input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)

	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	perCluster, ok := env.Clusters.(clusterImages)
	require.True(t, ok)
	assert.Equal(t, imageRows{{Image: "nginx:1", Count: 1}}, perCluster["c-a"])
	assert.Equal(t, imageRows{{Image: "redis:7", Count: 1}}, perCluster["c-b"])
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters"}, root.requests())
	assert.False(t, local.listed(clusterKind.GVR), "the clusters are never listed through the local client")
	assert.NotContains(t, factory.clusterCalls, engine.LocalCluster)
}

// TestImagesAllClustersCollectionReadErrorIsReturned asserts a failed read of
// the steve cluster collection fails the command before any cluster client
// is built.
func TestImagesAllClustersCollectionReadErrorIsReturned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		status   int
		wantCode string
		wantText string
	}{
		{name: "forbidden", status: http.StatusForbidden, wantCode: clierr.CodeRBAC, wantText: "the server refused GET /v1/management.cattle.io.clusters (403)"},
		{name: "server error", status: http.StatusInternalServerError, wantCode: clierr.CodeGeneric, wantText: "GET /v1/management.cattle.io.clusters answered 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			factory := &fakeFactory{client: newFakeClient()}
			root := newSteveRoot(t, map[string]steveAnswer{steveClusterPath(""): {status: tc.status}})

			_, err := execRead(t, NewImages(factory, root.roots()), input{b: map[string]bool{"all-clusters": true}})
			require.ErrorContains(t, err, tc.wantText)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.wantCode, ce.Code)
			assert.Empty(t, factory.clusterCalls, "no cluster client is built after a failed collection read")
		})
	}
}

// TestImagesAllClustersZeroClustersIsVerifiedEmpty asserts a steve cluster
// collection the caller was authorized to read, returning no cluster, is an
// empty scan rather than a failure: no cluster client is built at all.
func TestImagesAllClustersZeroClustersIsVerifiedEmpty(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{}

	res, err := execRead(t, NewImages(factory, clustersRoot(t).roots()), input{b: map[string]bool{"all-clusters": true}})
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	assert.Nil(t, env.Degraded)
	perCluster, ok := env.Clusters.(clusterImages)
	require.True(t, ok)
	assert.Empty(t, perCluster)
	assert.Empty(t, factory.clusterCalls, "an empty scan domain must not build a cluster client")
}

// TestImagesAllClustersEveryScanFailedIsHardError asserts a fleet-wide scan
// that reached no cluster fails, keeping the code every cluster agreed on
// and listing the causes in cluster id order.
func TestImagesAllClustersEveryScanFailedIsHardError(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	factory, roots := allClustersFactory(t, map[string]engine.ClusterClient{
		"c-b": newFakeClient().withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden on b")),
		"c-a": newFakeClient().withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden on a")),
	})

	_, err := execRead(t, NewImages(factory, roots), input{b: map[string]bool{"all-clusters": true}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "no cluster could be scanned: c-a: forbidden on a; c-b: forbidden on b", ce.Message)
}

// TestImagesAllClustersEveryScanFailedDifferentlyIsGeneric asserts failures
// that disagree collapse to GENERIC rather than picking one cluster's code.
func TestImagesAllClustersEveryScanFailedDifferentlyIsGeneric(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	factory, roots := allClustersFactory(t, map[string]engine.ClusterClient{
		"c-a": newFakeClient().withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden")),
		"c-b": newFakeClient().withListErr(podKind.GVR, errors.New("connection refused")),
	})

	_, err := execRead(t, NewImages(factory, roots), input{b: map[string]bool{"all-clusters": true}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "no cluster could be scanned: c-a: forbidden; c-b: connection refused", ce.Message)
}

// TestImagesAllClustersOwnerWalkFailureIsNotAScanFailure asserts an owner
// walk that failed inside a successful scan keeps its rows and stays out of
// the every-scan-failed count.
func TestImagesAllClustersOwnerWalkFailureIsNotAScanFailure(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			ownedPod("p1", "web", "nginx:1.25", "rs", "rs-1"),
		}}).
		withGetErr(rsGVR, "web", "rs", clierr.New(clierr.CodeRBAC, "forbidden"))
	factory, roots := allClustersFactory(t, map[string]engine.ClusterClient{"c-a": client})

	res, err := execRead(t, NewImages(factory, roots), input{
		s: map[string]string{"by": "workload"},
		b: map[string]bool{"all-clusters": true},
	})
	require.NoError(t, err)
	env, ok := res.Data.(imagesResult)
	require.True(t, ok)
	require.Len(t, env.Degraded, 1)
	assert.Equal(t, sectionOwnerResolution, env.Degraded[0].Section)
	perCluster, ok := env.Clusters.(clusterImageWorkloads)
	require.True(t, ok)
	require.Len(t, perCluster["c-a"], 1)
}

// TestImagesAllClustersScanCancellationStaysHard asserts a canceled caller
// fails the scan instead of reporting every cluster as degraded.
func TestImagesAllClustersScanCancellationStaysHard(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	factory, _ := allClustersFactory(t, map[string]engine.ClusterClient{
		"c-a": newFakeClient().withListErr(podKind.GVR, context.Canceled),
		"c-b": newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
		}}),
	})

	// The interrupt lands once the cluster collection has answered, so the
	// scans run under a canceled context.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	roots := fakeRoots{rc: cancelAfterRoot{RootClient: clustersRoot(t, "c-a", "c-b"), cancel: cancel}}
	rc := NewImages(factory, roots).(runtime.ReadCommand)
	_, err := rc.Execute(ctx, input{b: map[string]bool{"all-clusters": true}}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestImagesAllClustersDegradedEntriesAreDeterministic asserts the
// concurrent fan-out yields the same sorted entries and warnings on every
// run, so the output is golden-stable.
func TestImagesAllClustersDegradedEntriesAreDeterministic(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	for range 20 {
		factory, roots := allClustersFactory(t, map[string]engine.ClusterClient{
			"c-c": newFakeClient().withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden")),
			"c-b": newFakeClient().withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
				*podWithImages("ns1", "p1", []string{"nginx:1"}, nil),
			}}),
			"c-a": newFakeClient().withListErr(podKind.GVR, errors.New("connection refused")),
		})

		res, err := execRead(t, NewImages(factory, roots), input{b: map[string]bool{"all-clusters": true}})
		require.NoError(t, err)
		env, ok := res.Data.(imagesResult)
		require.True(t, ok)
		require.Equal(t, []sectionOutcome{
			{Section: sectionClusters, State: statePartial, Code: clierr.CodeGeneric, Target: "c-a"},
			{Section: sectionClusters, State: statePartial, Code: clierr.CodeRBAC, Target: "c-c"},
		}, env.Degraded)
		require.Equal(t, []string{
			"clusters unavailable for c-a: connection refused",
			"clusters unavailable for c-c: forbidden",
		}, res.Warnings)
	}
}
