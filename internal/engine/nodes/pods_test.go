package nodes

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// pod builds a pod of namespace apps with the owner, volumes, annotations and
// phase given.
func pod(name string, owner map[string]any, volumes []any, annotations map[string]any, phase string) unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "apps"}
	if owner != nil {
		meta["ownerReferences"] = []any{owner}
	}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	spec := map[string]any{"nodeName": "worker-1"}
	if volumes != nil {
		spec["volumes"] = volumes
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec, "status": map[string]any{"phase": phase},
	}}
}

func controlledBy(kind string) map[string]any {
	return controlledByAPI(kind, "apps/v1")
}

func controlledByAPI(kind, apiVersion string) map[string]any {
	return map[string]any{"apiVersion": apiVersion, "kind": kind, "name": "owner", "controller": true}
}

var emptyDirVolume = []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{}}}

// TestPodRulesMatchKubectlDrain asserts every pod rule of the preview against
// kubectl drain's filters, with kubectl's texts: a pod being deleted passes,
// a DaemonSet pod is skipped (an orphaned one cannot be told apart without
// reading its DaemonSet), a mirror pod is skipped, a pod with an emptyDir
// volume blocks the drain without --delete-emptydir-data, a pod with no
// controller blocks it without --force, and a finished pod passes the filters
// that have an exception for it.
func TestPodRulesMatchKubectlDrain(t *testing.T) {
	t.Parallel()
	deleted := pod("deleted", controlledBy("ReplicaSet"), nil, nil, "Running")
	now := metav1.Now()
	deleted.SetDeletionTimestamp(&now)
	tests := []struct {
		name                  string
		pod                   unstructured.Unstructured
		force, deleteEmptyDir bool
		outcome, message      string
	}{
		{"deleted pod passes", deleted, false, false, safety.OutcomeWillBeEvicted, ""},
		{"daemonset pod", pod("ds", controlledBy("DaemonSet"), nil, nil, "Running"), false, false, safety.OutcomeSkipped, "DaemonSet pod"},
		{"orphaned daemonset pod without force", pod("orphan", controlledBy("DaemonSet"), nil, nil, "Running"), false, false, safety.OutcomeSkipped, "DaemonSet pod"},
		{"orphaned daemonset pod with force", pod("orphan", controlledBy("DaemonSet"), nil, nil, "Running"), true, false, safety.OutcomeSkipped, "DaemonSet pod"},
		{"finished daemonset pod", pod("ds-done", controlledBy("DaemonSet"), nil, nil, "Succeeded"), false, false, safety.OutcomeWillBeEvicted, ""},
		{"daemonset kind of another API group is an ordinary controller", pod("kruise", controlledByAPI("DaemonSet", "apps.kruise.io/v1alpha1"), nil, nil, "Running"), false, false, safety.OutcomeWillBeEvicted, ""},
		{"mirror pod", pod("static", nil, nil, map[string]any{"kubernetes.io/config.mirror": "abc"}, "Running"), false, false, safety.OutcomeSkipped, "mirror pod"},
		{"emptyDir without the flag", pod("cache", controlledBy("ReplicaSet"), emptyDirVolume, nil, "Running"), false, false, safety.OutcomeBlocksTheDrain, "Pods with local storage (use --delete-emptydir-data to override)"},
		{"emptyDir with the flag", pod("cache", controlledBy("ReplicaSet"), emptyDirVolume, nil, "Running"), false, true, safety.OutcomeWillBeEvicted, ""},
		{"no controller without force", pod("bare", nil, nil, nil, "Running"), false, false, safety.OutcomeBlocksTheDrain, "Pods that declare no controller (use --force to override)"},
		{"no controller with force", pod("bare", nil, nil, nil, "Running"), true, false, safety.OutcomeWillBeEvicted, ""},
		{"owner that is no controller", pod("owned", map[string]any{"kind": "ReplicaSet", "name": "rs"}, nil, nil, "Running"), false, false, safety.OutcomeBlocksTheDrain, "Pods that declare no controller (use --force to override)"},
		{"emptyDir and no controller block on the emptyDir first", pod("both", nil, emptyDirVolume, nil, "Running"), false, false, safety.OutcomeBlocksTheDrain, "Pods with local storage (use --delete-emptydir-data to override)"},
		{"succeeded pod with no controller and an emptyDir", pod("job", nil, emptyDirVolume, nil, "Succeeded"), false, false, safety.OutcomeWillBeEvicted, ""},
		{"failed pod with no controller and an emptyDir", pod("job", nil, emptyDirVolume, nil, "Failed"), false, false, safety.OutcomeWillBeEvicted, ""},
		{"controlled pod with a volume that is no emptyDir", pod("db", controlledBy("StatefulSet"), []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "x"}}}, nil, "Running"), false, false, safety.OutcomeWillBeEvicted, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outcome, message := podOutcome(tc.pod.Object, tc.force, tc.deleteEmptyDir)
			assert.Equal(t, tc.outcome, outcome)
			assert.Equal(t, tc.message, message)
		})
	}
}

// TestPlanPodsSortsAndSummarizes asserts the pod plan lists every pod as
// namespace/name in sorted order, lists each distinct blocking text once in
// listing order, the sorted namespaces of the pods that will be evicted, and
// whether a DaemonSet pod is listed.
func TestPlanPodsSortsAndSummarizes(t *testing.T) {
	t.Parallel()
	web := pod("web", controlledBy("ReplicaSet"), nil, nil, "Running")
	other := pod("api", controlledBy("ReplicaSet"), nil, nil, "Running")
	other.SetNamespace("zeta")
	bare := pod("bare", nil, nil, nil, "Running")
	bare2 := pod("bare2", nil, nil, nil, "Running")
	cache := pod("cache", controlledBy("ReplicaSet"), emptyDirVolume, nil, "Running")
	ds := pod("proxy", controlledBy("DaemonSet"), nil, nil, "Running")
	got := planPods([]unstructured.Unstructured{web, other, ds, cache, bare2, bare}, false, false)
	names := make([]string, len(got.objects))
	for i, o := range got.objects {
		assert.Equal(t, "pod", o.Kind)
		names[i] = o.Name
	}
	assert.Equal(t, []string{"apps/bare", "apps/bare2", "apps/cache", "apps/proxy", "apps/web", "zeta/api"}, names)
	assert.Equal(t, []string{"Pods that declare no controller (use --force to override)", "Pods with local storage (use --delete-emptydir-data to override)"}, got.blockers)
	assert.Equal(t, []string{"apps", "zeta"}, got.evictedNamespaces)
	assert.True(t, got.daemonSet)

	assert.False(t, planPods([]unstructured.Unstructured{web}, false, false).daemonSet)

	kruise := pod("kruise", controlledByAPI("DaemonSet", "apps.kruise.io/v1alpha1"), nil, nil, "Running")
	kruise.SetNamespace("kruise-ns")
	custom := planPods([]unstructured.Unstructured{kruise}, false, false)
	assert.False(t, custom.daemonSet, "a DaemonSet of another API group is an ordinary controller")
	assert.Equal(t, []string{"kruise-ns"}, custom.evictedNamespaces)
}
