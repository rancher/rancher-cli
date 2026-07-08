package provider

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

var projectGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

func newFakeClient(objs ...runtime.Object) (*dynamicClient, *dynamicfake.FakeDynamicClient) {
	scheme := runtime.NewScheme()
	// Register the GVR->ListKind so the fake tracker can serve List.
	gvrToListKind := map[schema.GroupVersionResource]string{projectGVR: "ProjectList"}
	fc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, objs...)
	return &dynamicClient{dyn: fc}, fc
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
