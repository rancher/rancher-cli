package engine

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

// stubClient records the last call and returns canned results.
type stubClient struct {
	lastCreateDryRun bool
	createOut        *unstructured.Unstructured
	createErr        error
	lastUpdateDryRun bool
	updateErr        error
}

func (s *stubClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (s *stubClient) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (s *stubClient) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	s.lastCreateDryRun = dryRun
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.createOut != nil {
		return s.createOut, nil
	}
	return obj, nil
}
func (s *stubClient) Update(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	s.lastUpdateDryRun = dryRun
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return obj, nil
}
func (s *stubClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func TestPlanCreate(t *testing.T) {
	client := &stubClient{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x"},
	}}
	eff, exec, err := PlanCreate(context.Background(), client, testGVR, "c-x", obj, "c-x", "Project")
	require.NoError(t, err)
	assert.True(t, client.lastCreateDryRun, "Plan must call Create with dryRun=true")
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "projects/p-1", eff.Target)
	assert.Equal(t, "c-x", eff.Cluster) // Effect targets the downstream cluster, not the proxy
	assert.True(t, eff.ConflictDetection)
	assert.False(t, eff.Destructive)
	assert.Empty(t, eff.ResourceVersion) // create has no prior resourceVersion

	// The returned closure performs the real (non-dry-run) create.
	require.NotNil(t, exec)
	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.False(t, client.lastCreateDryRun, "exec must call Create with dryRun=false")
	assert.Equal(t, "Project", res.Kind)
}

func TestPlanUpdate(t *testing.T) {
	client := &stubClient{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x", "resourceVersion": "9"},
	}}
	diff := []safety.DiffEntry{{Path: "/spec/x", From: nil, To: "y"}}
	eff, exec, err := PlanUpdate(context.Background(), client, testGVR, "c-x", obj, "c-x", "Project", diff, true)
	require.NoError(t, err)
	assert.True(t, client.lastUpdateDryRun, "Plan must call Update with dryRun=true")
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, "projects/p-1", eff.Target)
	assert.Equal(t, "c-x", eff.Cluster)
	assert.Equal(t, "9", eff.ResourceVersion) // the RV precondition carried on the object
	assert.True(t, eff.ConflictDetection)
	assert.True(t, eff.Destructive) // caller's per-kind policy
	assert.Equal(t, diff, eff.Diff)

	require.NotNil(t, exec)
	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.False(t, client.lastUpdateDryRun, "exec must call Update with dryRun=false")
	assert.Equal(t, "Project", res.Kind)
}

func TestPlanUpdatePropagatesError(t *testing.T) {
	client := &stubClient{updateErr: assert.AnError}
	_, _, err := PlanUpdate(context.Background(), client, testGVR, "c-x",
		&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}}},
		"c-x", "Project", nil, false)
	require.Error(t, err)
}

func TestPlanCreatePropagatesError(t *testing.T) {
	client := &stubClient{createErr: assert.AnError}
	_, _, err := PlanCreate(context.Background(), client, testGVR, "c-x",
		&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}}}, "c-x", "Project")
	require.Error(t, err)
}
