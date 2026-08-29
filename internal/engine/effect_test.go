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
func (s *stubClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (s *stubClient) Logs(context.Context, string, string, LogOptions) (string, error) {
	return "", nil
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

// obsClient serves a configurable dry-run response and records calls.
type obsClient struct {
	dryRunResult *unstructured.Unstructured
	dryRuns      []bool
}

func (c *obsClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *obsClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (c *obsClient) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.dryRuns = append(c.dryRuns, dryRun)
	if dryRun && c.dryRunResult != nil {
		return c.dryRunResult, nil
	}
	return obj, nil
}
func (c *obsClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *obsClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}
func (c *obsClient) Logs(context.Context, string, string, LogOptions) (string, error) {
	return "", nil
}

// TestPlanCreateObservedExposesDryRunResult pins the MC-12 seam: the
// caller receives the SERVER's dry-run object (not its own submission),
// the Effect stays the diff-less create shape, and the executor still
// performs the real write.
func TestPlanCreateObservedExposesDryRunResult(t *testing.T) {
	t.Parallel()
	serverView := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke-machine-config.cattle.io/v1",
		"kind":       "DigitaloceanConfig",
		"metadata":   map[string]any{"name": "web-pool", "namespace": "fleet-default"},
		"region":     "nyc3",
	}}
	client := &obsClient{dryRunResult: serverView}
	gvr := schema.GroupVersionResource{Group: "rke-machine-config.cattle.io", Version: "v1", Resource: "digitaloceanconfigs"}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke-machine-config.cattle.io/v1",
		"kind":       "DigitaloceanConfig",
		"metadata":   map[string]any{"name": "web-pool", "namespace": "fleet-default"},
		"region":     "nyc3",
		"regoin":     "typo",
	}}

	eff, dryRun, exec, err := PlanCreateObserved(t.Context(), client, gvr, "fleet-default", obj, LocalCluster, "digitaloceanconfig", "DigitaloceanConfig", false)
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []bool{true}, client.dryRuns)
	assert.Same(t, serverView, dryRun)
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "digitaloceanconfigs/web-pool", eff.Target)
	assert.Equal(t, "digitaloceanconfig", eff.DisplayKind)
	assert.Equal(t, "web-pool", eff.DisplayName)
	assert.Empty(t, eff.Diff, "create previews are diff-less (MC-11)")
	assert.False(t, eff.Destructive)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false}, client.dryRuns)
	assert.Equal(t, "digitaloceanconfig/web-pool created\n", res.Text)
}

func TestPlanCreate(t *testing.T) {
	client := &stubClient{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x"},
	}}
	eff, exec, err := PlanCreate(t.Context(), client, testGVR, "c-x", obj, "c-x", "project", "Project", false)
	require.NoError(t, err)
	assert.True(t, client.lastCreateDryRun, "Plan must call Create with dryRun=true")
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "projects/p-1", eff.Target)
	assert.Equal(t, "project", eff.DisplayKind)
	assert.Equal(t, "p-1", eff.DisplayName)
	assert.Equal(t, "c-x", eff.Cluster) // Effect targets the downstream cluster, not the proxy
	assert.True(t, eff.ConflictDetection)
	assert.False(t, eff.Destructive)
	assert.Empty(t, eff.ResourceVersion) // create has no prior resourceVersion

	// The returned closure performs the real (non-dry-run) create.
	require.NotNil(t, exec)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.False(t, client.lastCreateDryRun, "exec must call Create with dryRun=false")
	assert.Equal(t, "Project", res.Kind)
	assert.Equal(t, "project/p-1 created\n", res.Text)
}

func TestPlanUpdate(t *testing.T) {
	client := &stubClient{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x", "resourceVersion": "9"},
	}}
	diff := []safety.DiffEntry{{Path: "/spec/x", From: nil, To: "y"}}
	eff, exec, err := PlanUpdate(t.Context(), client, testGVR, "c-x", obj, "c-x", "project", "Project", diff, true)
	require.NoError(t, err)
	assert.True(t, client.lastUpdateDryRun, "Plan must call Update with dryRun=true")
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, "projects/p-1", eff.Target)
	assert.Equal(t, "project", eff.DisplayKind)
	assert.Equal(t, "p-1", eff.DisplayName)
	assert.Equal(t, "c-x", eff.Cluster)
	assert.Equal(t, "9", eff.ResourceVersion) // the RV precondition carried on the object
	assert.True(t, eff.ConflictDetection)
	assert.True(t, eff.Destructive) // caller's per-kind policy
	assert.Equal(t, diff, eff.Diff)

	require.NotNil(t, exec)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.False(t, client.lastUpdateDryRun, "exec must call Update with dryRun=false")
	assert.Equal(t, "Project", res.Kind)
	assert.Equal(t, "project/p-1 patched\n", res.Text)
}

func TestPlanUpdatePropagatesError(t *testing.T) {
	client := &stubClient{updateErr: assert.AnError}
	_, _, err := PlanUpdate(t.Context(), client, testGVR, "c-x",
		&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}}},
		"c-x", "project", "Project", nil, false)
	require.Error(t, err)
}

func TestPlanCreatePropagatesError(t *testing.T) {
	client := &stubClient{createErr: assert.AnError}
	_, _, err := PlanCreate(t.Context(), client, testGVR, "c-x",
		&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}}}, "c-x", "project", "Project", false)
	require.Error(t, err)
}

// TestPlanCreateDestructiveParameter pins the fix for the class of bug where
// a Sensitive kind's create silently skipped the gate because the caller
// forgot the after-the-fact `eff.Destructive = kind.Sensitive` assignment:
// destructive is now a required parameter, not an opt-in follow-up.
func TestPlanCreateDestructiveParameter(t *testing.T) {
	client := &stubClient{}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1", "namespace": "c-x"},
	}}
	eff, _, err := PlanCreate(t.Context(), client, testGVR, "c-x", obj, "c-x", "project", "Project", true)
	require.NoError(t, err)
	assert.True(t, eff.Destructive)
}
