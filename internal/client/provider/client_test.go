package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// stubDiscovery serves only ServerGroups (the sole method resolveVersion needs)
// and counts how many times it is consulted, so the memoization can be
// asserted. The embedded interface panics on any other call.
type stubDiscovery struct {
	discovery.DiscoveryInterface
	groups map[string]string // group name → preferred version
	calls  int
}

func (s *stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	s.calls++
	gl := &metav1.APIGroupList{}
	for name, ver := range s.groups {
		gl.Groups = append(gl.Groups, metav1.APIGroup{
			Name:             name,
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: name + "/" + ver, Version: ver},
		})
	}
	return gl, nil
}

func TestEmptyVersionResolvedViaDiscovery(t *testing.T) {
	t.Parallel()
	machinesV1 := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machines"}
	clustersV1 := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"}
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{machinesV1: "MachineList", clustersV1: "ClusterList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	var lastVersion string
	fc.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		lastVersion = a.GetResource().Version
		return false, nil, nil // fall through to the tracker
	})
	disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": "v1beta1"}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	_, err := c.List(context.Background(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"}, "fleet-default")
	require.NoError(t, err)
	assert.Equal(t, "v1beta1", lastVersion, "the empty version is filled from discovery before the API call")

	// Second call, same group: discovery is memoized, not consulted again.
	_, err = c.List(context.Background(), schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "clusters"}, "fleet-default")
	require.NoError(t, err)
	assert.Equal(t, 1, disc.calls)
}

// TestEmptyPreferredVersionIsNotCached guards the cache-poisoning fix: a group
// whose preferred version is the empty string must error, and the empty value
// must NOT be memoized — a later call re-consults discovery so a group that
// starts serving a version can recover without recreating the client.
func TestEmptyPreferredVersionIsNotCached(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{})
	disc := &stubDiscovery{groups: map[string]string{"cluster.x-k8s.io": ""}}
	c := &dynamicClient{dyn: fc, disc: disc, versions: map[string]string{}}

	machines := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"}
	_, err := c.List(context.Background(), machines, "fleet-default")
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, 1, disc.calls)

	// Second call: the empty version was not cached, so discovery is consulted again.
	_, err = c.List(context.Background(), machines, "fleet-default")
	require.Error(t, err)
	assert.Equal(t, 2, disc.calls, "an empty preferred version must not poison the memo")
}

var projectGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

// newFakeClient builds a dynamicClient over the fake dynamic client. Discovery
// is stubbed (empty groups) and the version memo initialized, so exercising an
// empty-version GVR resolves through the stub instead of panicking on a nil
// discovery client.
func newFakeClient(objs ...runtime.Object) (*dynamicClient, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	// Register the GVR->ListKind so the fake tracker can serve List.
	gvrToListKind := map[schema.GroupVersionResource]string{projectGVR: "ProjectList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
	return &dynamicClient{dyn: fc, disc: &stubDiscovery{groups: map[string]string{}}, versions: map[string]string{}}, fc
}

func project(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"clusterName": ns, "displayName": name},
	}}
}

func TestDynamicRepoCreateAndGet(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	out, err := r.Create(context.Background(), projectGVR, "c-x", project("c-x", "p-1"), false)
	require.NoError(t, err)
	assert.Equal(t, "p-1", out.GetName())

	got, err := r.Get(context.Background(), projectGVR, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", got.GetName())
}

func TestDynamicRepoCreatePassesDryRun(t *testing.T) {
	t.Parallel()
	r, fc := newFakeClient()
	var sawDryRun []string
	fc.PrependReactor("create", "projects", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sawDryRun = a.(k8stesting.CreateActionImpl).GetCreateOptions().DryRun
		return false, nil, nil
	})
	_, err := r.Create(context.Background(), projectGVR, "c-x", project("c-x", "p-2"), true)
	require.NoError(t, err)
	assert.Equal(t, []string{"All"}, sawDryRun)
}

func TestDynamicRepoListAndDelete(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient(project("c-x", "p-1"), project("c-x", "p-2"))
	list, err := r.List(context.Background(), projectGVR, "c-x")
	require.NoError(t, err)
	assert.Len(t, list.Items, 2)

	require.NoError(t, r.Delete(context.Background(), projectGVR, "c-x", "p-1", "", false))
	after, err := r.List(context.Background(), projectGVR, "c-x")
	require.NoError(t, err)
	assert.Len(t, after.Items, 1)
}

func TestDynamicClientBoundsRequestWithDeadline(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()
	_, err := r.Get(ctx, projectGVR, "c-x", "p-1")
	require.Error(t, err, "an already-expired parent context must surface a deadline error")
}

func TestDynamicRepoGetNotFoundMapsToClierr(t *testing.T) {
	t.Parallel()
	r, _ := newFakeClient()
	_, err := r.Get(context.Background(), projectGVR, "c-x", "nope")
	require.Error(t, err)
	// mapErr(Task 1) turns the k8s NotFound into a CodeNotFound clierr.
	assert.Contains(t, err.Error(), "not found")
}
