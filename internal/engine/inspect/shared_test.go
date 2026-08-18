package inspect

import (
	"context"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestCompositesRejectProjectScope pins PC-3's composite-rejection rule: the
// non-project inspect composites bypass Route, so without an explicit check
// the global --project flag would reach them and be silently ignored — the
// exact no-op class DR-004 removed --project for.
func TestCompositesRejectProjectScope(t *testing.T) {
	tests := []struct {
		name string
		cmd  runtime.Command
		in   runtime.Input
	}{
		{"inspect pod", NewPod(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"inspect deployment", NewDeployment(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"images", NewImages(&fakeFactory{}),
			input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execRead(t, tt.cmd, tt.in)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, tt.name)
		})
	}
}

func TestOwnerChainUIDMismatchStopsWalk(t *testing.T) {
	t.Parallel()
	rsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	rs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name":      "web-7d9f8b",
			"namespace": "web",
			"uid":       "rs-uid-NEW",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"name": "web", "uid": "dep-uid-1", "controller": true,
			}},
		},
	}}
	client := newFakeClient().withObject(rsGVR, "web", "web-7d9f8b", rs)
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "web-7d9f8b-x1", "namespace": "web",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"name": "web-7d9f8b", "uid": "rs-uid-OLD", "controller": true,
			}},
		},
	}}
	owners, warnings := ownerChain(context.Background(), client, "web", pod)
	require.Len(t, owners, 1, "walk must stop at the stale ReplicaSet ref")
	assert.Equal(t, "ReplicaSet", owners[0]["kind"])
	assert.Equal(t, "rs-uid-OLD", owners[0]["uid"])
	assert.Equal(t, "apps/v1", owners[0]["apiVersion"])
	require.Len(t, warnings, 1)
	assert.Equal(t,
		"owner web/replicaset.apps/web-7d9f8b unavailable: uid mismatch (stale owner reference)",
		warnings[0])
}

func TestOwnerChainWarningIncludesNamespaceAndGroup(t *testing.T) {
	t.Parallel()
	rsGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	client := newFakeClient().withGetErr(rsGVR, "ns-a", "same-name", fmt.Errorf("boom"))
	pod := func(ns string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{
				"name": "p", "namespace": ns,
				"ownerReferences": []any{map[string]any{
					"apiVersion": "apps/v1", "kind": "ReplicaSet",
					"name": "same-name", "uid": "u-a", "controller": true,
				}},
			},
		}}
	}
	_, warnA := ownerChain(context.Background(), client, "ns-a", pod("ns-a"))
	require.Len(t, warnA, 1)
	assert.Equal(t, "owner ns-a/replicaset.apps/same-name unavailable: boom", warnA[0])
}

func TestOwnerChainNoUIDStopsWalk(t *testing.T) {
	t.Parallel()
	client := newFakeClient() // no objects registered; Get must never be called
	pod := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "p", "namespace": "ns",
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet",
				"name": "web-rs", "uid": "", "controller": true,
			}},
		},
	}}
	owners, warnings := ownerChain(context.Background(), client, "ns", pod)
	require.Len(t, owners, 1, "walk must append the ref then stop on empty uid")
	assert.Equal(t, "ReplicaSet", owners[0]["kind"])
	assert.Empty(t, client.getCalls, "Get must not be called when the owner reference has no uid")
	require.Len(t, warnings, 1)
	assert.Equal(t,
		"owner ns/replicaset.apps/web-rs unavailable: owner reference has no uid",
		warnings[0])
}

func TestKindGroupName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "deployment.apps/frontend", kindGroupName("apps/v1", "Deployment", "frontend"))
	assert.Equal(t, "pod/web-1", kindGroupName("v1", "Pod", "web-1"))
	assert.Equal(t, "pod/web-1", kindGroupName("", "pod", "web-1"))
}
