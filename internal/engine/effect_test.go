package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var testGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

// stubRepo records the last call and returns canned results.
type stubRepo struct {
	lastCreateDryRun bool
	createOut        *unstructured.Unstructured
	createErr        error
}

func (s *stubRepo) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (s *stubRepo) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (s *stubRepo) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	s.lastCreateDryRun = dryRun
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.createOut != nil {
		return s.createOut, nil
	}
	return obj, nil
}
func (s *stubRepo) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (s *stubRepo) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func TestPlanCreate(t *testing.T) {
	repo := &stubRepo{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x"},
	}}
	eff, err := PlanCreate(context.Background(), repo, testGVR, "c-x", obj, "public", "stable")
	require.NoError(t, err)
	assert.True(t, repo.lastCreateDryRun, "Plan must call Create with dryRun=true")
	assert.Equal(t, "create", eff.Action)
	assert.Equal(t, "projects/p-1", eff.Target)
	assert.True(t, eff.ConflictDetection)
	assert.False(t, eff.Destructive)
	assert.Empty(t, eff.ResourceVersion) // create has no prior resourceVersion
	assert.Equal(t, "public", eff.Tier)
	assert.Equal(t, "stable", eff.Stability)
}

func TestPlanCreatePropagatesError(t *testing.T) {
	repo := &stubRepo{createErr: assert.AnError}
	_, err := PlanCreate(context.Background(), repo, testGVR, "c-x",
		&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}}}, "public", "stable")
	require.Error(t, err)
}
