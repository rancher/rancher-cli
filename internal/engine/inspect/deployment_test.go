package inspect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deploymentObj(ns, name string, selector map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{},
	}}
	if selector != nil {
		_ = unstructured.SetNestedMap(obj.Object, selector, "spec", "selector")
	}
	return obj
}

func TestInspectDeploymentFetchesPodsBySelector(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	podKind := mustKind(t, "pod")

	dep := deploymentObj("ns1", "web", map[string]any{"matchLabels": map[string]any{"app": "web"}})
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podObj("ns1", "web-abc"),
	}}

	client := newFakeClient().
		withObject(depKind.GVR, "ns1", "web", dep).
		withList(podKind.GVR, pods)

	cmd := NewDeployment(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)

	assert.Equal(t, "DeploymentInspection", res.Kind)
	require.Len(t, client.listSelectors, 1)
	assert.Equal(t, "app=web", client.listSelectors[0])
	data, ok := res.Data.(deploymentResult)
	require.True(t, ok)
	require.Len(t, data.Pods, 1)
	assert.Equal(t, "deployment/web (namespace ns1, cluster c-abc)\n"+
		"  replicas: 0/0 ready\n"+
		"  pod web-abc: Running\n", res.Text)
}

func TestInspectDeploymentHonorsMatchExpressions(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	podKind := mustKind(t, "pod")

	dep := deploymentObj("ns1", "web", map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": "app", "operator": "In", "values": []any{"web", "api"}},
		},
	})

	client := newFakeClient().
		withObject(depKind.GVR, "ns1", "web", dep).
		withList(podKind.GVR, &unstructured.UnstructuredList{})

	cmd := NewDeployment(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "DeploymentInspection", res.Kind)
	require.Len(t, client.listSelectors, 1)
	assert.Equal(t, "app in (api,web)", client.listSelectors[0])
	assert.Equal(t, "deployment/web (namespace ns1, cluster c-abc)\n"+
		"  replicas: 0/0 ready\n"+
		"  pods: none\n", res.Text)
}

// TestInspectDeploymentPodListDeniedNullsPods is the deployment anchor: a
// caller who may read the deployment but not its pods gets the deployment,
// a null pods list, and one entry — the whole document used to be lost.
func TestInspectDeploymentPodListDeniedNullsPods(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	podKind := mustKind(t, "pod")

	dep := deploymentObj("ns1", "web", map[string]any{"matchLabels": map[string]any{"app": "web"}})
	client := newFakeClient().
		withObject(depKind.GVR, "ns1", "web", dep).
		withListErr(podKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden"))

	cmd := NewDeployment(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err, "a denied pod list must not fail the command")

	data, ok := res.Data.(deploymentResult)
	require.True(t, ok)
	require.NotNil(t, data.Deployment)
	assert.Nil(t, data.Pods, "pods serializes as null when the list is denied")
	require.Equal(t, []sectionOutcome{
		{Section: sectionPods, State: stateUnavailable, Code: clierr.CodeRBAC},
	}, data.Degraded)
	require.Equal(t, []string{"pods unavailable: forbidden"}, res.Warnings)
	assert.Equal(t, "deployment/web (namespace ns1, cluster c-abc)\n"+
		"  replicas: 0/0 ready\n"+
		"  pods: none\n"+
		"  degraded: pods unavailable (RBAC)\n", res.Text)

	var payload struct {
		Pods     []map[string]any `json:"pods"`
		Degraded []sectionOutcome `json:"degraded"`
	}
	raw, merr := json.Marshal(data)
	require.NoError(t, merr)
	require.NoError(t, json.Unmarshal(raw, &payload))
	assert.Nil(t, payload.Pods)
	require.Len(t, payload.Degraded, 1)
	assert.Equal(t, sectionPods, payload.Degraded[0].Section)
}

// TestInspectDeploymentCancellationStaysHard asserts a canceled caller
// context fails the command instead of degrading the pods section.
func TestInspectDeploymentCancellationStaysHard(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	podKind := mustKind(t, "pod")

	dep := deploymentObj("ns1", "web", map[string]any{"matchLabels": map[string]any{"app": "web"}})
	client := newFakeClient().
		withObject(depKind.GVR, "ns1", "web", dep).
		withListErr(podKind.GVR, context.Canceled)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rc := NewDeployment(&fakeFactory{client: client}).(runtime.ReadCommand)
	_, err := rc.Execute(ctx, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestInspectDeploymentEmptySelectorErrors(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	dep := deploymentObj("ns1", "web", nil)

	client := newFakeClient().withObject(depKind.GVR, "ns1", "web", dep)

	cmd := NewDeployment(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.Error(t, err)
	assert.Empty(t, client.listSelectors, "an empty selector must never reach List")
}

func TestInspectDeploymentNotFoundCurated(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	client := newFakeClient().withGetErr(depKind.GVR, "ns1", "web", kinds.NotFoundCurated(depKind, "web"))

	cmd := NewDeployment(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.Error(t, err)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "deployment not found: web", ce.Message)
	assert.NotEmpty(t, ce.Hint, "curated error must carry a hint")
}

func TestInspectDeploymentNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	depKind := mustKind(t, "deployment")
	alreadyCurated := clierr.New(clierr.CodeNotFound, "deployment not found: web").
		WithHint("list with: rancher get deployments")
	client := newFakeClient().withGetErr(depKind.GVR, "ns1", "web", alreadyCurated)

	cmd := NewDeployment(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.Error(t, err)
	assert.Equal(t, alreadyCurated, err, "already-curated error must pass through unchanged")
}
