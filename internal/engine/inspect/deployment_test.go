package inspect

import (
	"testing"

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
