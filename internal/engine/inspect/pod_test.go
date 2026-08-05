package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func mustKind(t *testing.T, name string) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup(name)
	require.True(t, ok, "kind %q must exist", name)
	return k
}

func podObj(ns, name string, ownerRefs ...map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"status": map[string]any{
			"phase": "Running",
			"containerStatuses": []any{
				map[string]any{"name": "app", "ready": true},
			},
		},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "app"}},
		},
	}}
	if len(ownerRefs) > 0 {
		refs := make([]any, len(ownerRefs))
		for i, r := range ownerRefs {
			refs[i] = r
		}
		_ = unstructured.SetNestedSlice(obj.Object, refs, "metadata", "ownerReferences")
	}
	return obj
}

func ownerRefMap(apiVersion, kind, name string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name, "controller": true}
}

func execRead(t *testing.T, cmd runtime.Command, in runtime.Input) (runtime.Result, error) {
	t.Helper()
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok, "command must be a ReadCommand")
	return rc.Execute(context.Background(), in, nil)
}

func TestInspectPodAssemblesDocument(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	rsKind := mustKind(t, "replicaset")
	depKind := mustKind(t, "deployment")

	pod := podObj("ns1", "web", map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"name": "web-abc", "uid": "rs-uid-1", "controller": true,
	})
	rs := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "web-abc", "namespace": "ns1", "uid": "rs-uid-1",
			"ownerReferences": []any{
				ownerRefMap("apps/v1", "Deployment", "web"),
			},
		},
	}}
	metrics := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "ns1"},
		"usage":    map[string]any{"cpu": "5m", "memory": "10Mi"},
	}}

	client := newFakeClient().
		withObject(podKind.GVR, "ns1", "web", pod).
		withObject(rsKind.GVR, "ns1", "web-abc", rs).
		withObject(podMetricsGVR, "ns1", "web", metrics).
		withLog("app", "log line 1\nlog line 2\n")
	_ = depKind

	cmd := NewPod(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)

	assert.Equal(t, "PodInspection", res.Kind)
	assert.Empty(t, res.Warnings)
	data, ok := res.Data.(podResult)
	require.True(t, ok)
	require.Len(t, data.Owners, 2)
	assert.Equal(t, "ReplicaSet", data.Owners[0]["kind"])
	assert.Equal(t, "web-abc", data.Owners[0]["name"])
	assert.Equal(t, "Deployment", data.Owners[1]["kind"])
	assert.Equal(t, "web", data.Owners[1]["name"])
	require.NotNil(t, data.Metrics)
	assert.Equal(t, "log line 1\nlog line 2\n", data.Logs["app"])
}

func TestInspectPodDegradesWithoutMetricsAndLogs(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pod := podObj("ns1", "web")

	client := newFakeClient().
		withObject(podKind.GVR, "ns1", "web", pod).
		withGetErr(podMetricsGVR, "ns1", "web", errors.New("not found")).
		withLogErr("app", errors.New("container not ready"))

	cmd := NewPod(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)

	data, ok := res.Data.(podResult)
	require.True(t, ok)
	assert.Nil(t, data.Metrics)
	assert.Contains(t, res.Warnings, "metrics.k8s.io unavailable on cluster c-abc")
	require.Len(t, res.Warnings, 2)
	assert.Contains(t, res.Warnings[1], "logs unavailable for container app: container not ready")
}

func TestInspectPodUnknownOwnerKindReported(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pod := podObj("ns1", "web", ownerRefMap("custom.io/v1", "FooController", "x"))

	client := newFakeClient().
		withObject(podKind.GVR, "ns1", "web", pod).
		withGetErr(podMetricsGVR, "ns1", "web", errors.New("not found")).
		withLog("app", "ok")

	cmd := NewPod(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)

	data, ok := res.Data.(podResult)
	require.True(t, ok)
	require.Len(t, data.Owners, 1)
	assert.Equal(t, "FooController", data.Owners[0]["kind"])
	assert.Equal(t, "x", data.Owners[0]["name"])
}

func TestInspectPodFetchesInitContainerLogs(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "ns1"},
		"status":   map[string]any{"phase": "Init:CrashLoopBackOff"},
		"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": "init-setup"}},
			"containers":     []any{map[string]any{"name": "app"}},
		},
	}}

	client := newFakeClient().
		withObject(podKind.GVR, "ns1", "web", pod).
		withGetErr(podMetricsGVR, "ns1", "web", errors.New("not found")).
		withLog("app", "app log").
		withLog("init-setup", "init log")

	cmd := NewPod(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	require.NoError(t, err)

	data, ok := res.Data.(podResult)
	require.True(t, ok)
	assert.Equal(t, "init log", data.Logs["init-setup"])
	assert.Equal(t, "app log", data.Logs["app"])
}

func TestInspectPodTailFlagValidation(t *testing.T) {
	t.Parallel()
	client := newFakeClient()
	cmd := NewPod(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1", "tail": "garbage"},
	})
	require.Error(t, err)
}

// TestInspectPodOutputOmitsManagedFields drives the full runtime.Run →
// output.Render pipeline for `inspect pod` with a pod fixture that carries
// managedFields in its metadata, and asserts the field is absent from both
// json and yaml output while the pod's name survives (MF-3, MF-7a).
func TestInspectPodOutputOmitsManagedFields(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	rsKind := mustKind(t, "replicaset")

	pod := podObj("ns1", "web-1", map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"name": "web-1-abc", "uid": "rs-uid-1", "controller": true,
	})
	pod.Object["metadata"].(map[string]any)["managedFields"] = []any{
		map[string]any{"manager": "k3s"},
	}

	rs := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "web-1-abc", "namespace": "ns1", "uid": "rs-uid-1",
			"ownerReferences": []any{
				ownerRefMap("apps/v1", "Deployment", "web-1"),
			},
		},
	}}
	metrics := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web-1", "namespace": "ns1"},
		"usage":    map[string]any{"cpu": "5m", "memory": "10Mi"},
	}}

	client := newFakeClient().
		withObject(podKind.GVR, "ns1", "web-1", pod).
		withObject(rsKind.GVR, "ns1", "web-1-abc", rs).
		withObject(podMetricsGVR, "ns1", "web-1", metrics).
		withLog("app", "log line 1\nlog line 2\n")

	cmd := NewPod(&fakeFactory{client: client})
	in := input{
		args: []string{"web-1"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	}

	for _, format := range []string{"json", "yaml"} {
		format := format
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			_, ce := runtime.Run(context.Background(), cmd, in,
				runtime.IO{Out: &buf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			assert.NotContains(t, buf.String(), "managedFields",
				"the pod nested inside the composite document is scrubbed (MF-3)")
			raw := buf.Bytes()
			if format == "yaml" {
				var err error
				raw, err = yaml.YAMLToJSON(raw)
				require.NoError(t, err)
			}
			var doc map[string]any
			require.NoError(t, json.Unmarshal(raw, &doc))
			podMap := doc["data"].(map[string]any)["pod"].(map[string]any)
			assert.Equal(t, "web-1", podMap["metadata"].(map[string]any)["name"],
				"the rest of the pod survives")
		})
	}
}

// TestInspectPodNotFoundCurated verifies that a hint-less NOT_FOUND from the
// pod Get is curated to "pod not found: <name>" and that a hint-bearing
// NOT_FOUND passes through unchanged.
func TestInspectPodNotFoundCurated(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	// hint-less 404 → curated shape (produced by the dynamic client; fake simulates it)
	client := newFakeClient().withGetErr(podKind.GVR, "ns1", "web", kinds.NotFoundCurated(podKind, "web"))
	cmd := NewPod(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "pod not found: web", ce.Message)
	assert.Equal(t, "list with: rancher get pods", ce.Hint)

	// hint-bearing 404 passes through unchanged
	hinted := clierr.New(clierr.CodeNotFound, "unsupported group").WithHint("try another group")
	client2 := newFakeClient().withGetErr(podKind.GVR, "ns1", "web", hinted)
	cmd2 := NewPod(&fakeFactory{client: client2})
	_, err2 := execRead(t, cmd2, input{
		args: []string{"web"},
		s:    map[string]string{"cluster": "c-abc", "namespace": "ns1"},
	})
	var ce2 *clierr.Error
	require.ErrorAs(t, err2, &ce2)
	assert.Equal(t, "try another group", ce2.Hint, "hint-bearing error must pass through unchanged")
}
