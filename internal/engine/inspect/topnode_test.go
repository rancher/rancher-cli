package inspect

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nodeObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
	}}
}

func nodeMetricsObj(name, cpu, memory string) unstructured.Unstructured {
	return *nodeMetricsObjPtr(name, cpu, memory)
}

func nodeMetricsObjPtr(name, cpu, memory string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"usage":    map[string]any{"cpu": cpu, "memory": memory},
	}}
}

func TestTopNodeJoinsMetrics(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")

	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1"), *nodeObj("n2")}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{nodeMetricsObj("n1", "100m", "200Mi")}}

	client := newFakeClient().
		withList(nodeKind.GVR, nodes).
		withList(nodeMetricsGVR, metrics)

	cmd := NewTopNode(&fakeFactory{client: client, ctxCluster: "c-abc"})
	res, err := execRead(t, cmd, input{s: map[string]string{}})
	require.NoError(t, err)

	assert.Equal(t, "NodeMetrics", res.Kind)
	assert.Empty(t, res.Warnings)
	rows, ok := res.Data.(nodeRows)
	require.True(t, ok)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"NAME", "CPU", "MEMORY"}, rows.Columns())
	assert.Equal(t, [][]string{
		{"n1", "100m", "200Mi"},
		{"n2", "<none>", "<none>"},
	}, rows.Rows())
}

func TestTopNodeDegradesWithoutMetricsServer(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}

	client := newFakeClient().
		withList(nodeKind.GVR, nodes).
		withListErr(nodeMetricsGVR, errors.New("no metrics server"))

	cmd := NewTopNode(&fakeFactory{client: client, ctxCluster: "c-abc"})
	res, err := execRead(t, cmd, input{s: map[string]string{}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "metrics.k8s.io unavailable on cluster c-abc", res.Warnings[0])
	rows, ok := res.Data.(nodeRows)
	require.True(t, ok)
	assert.Equal(t, [][]string{{"n1", "<none>", "<none>"}}, rows.Rows())
}

func TestTopNodeSingle(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")

	client := newFakeClient().
		withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
		withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", "50m", "64Mi"))

	cmd := NewTopNode(&fakeFactory{client: client, ctxCluster: "c-abc"})
	res, err := execRead(t, cmd, input{args: []string{"n1"}, s: map[string]string{}})
	require.NoError(t, err)

	assert.Empty(t, res.Warnings)
	rows, ok := res.Data.(nodeRows)
	require.True(t, ok)
	require.Len(t, rows, 1)
	assert.Equal(t, [][]string{{"n1", "50m", "64Mi"}}, rows.Rows())
}
