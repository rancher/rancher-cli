package kinds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The CreateWarning of each day-2 operation row: what Rancher does once the
// object exists. The patch check repeats it when it resumes a paused object.
const (
	etcdSnapshotSaveWarning = "Rancher takes an etcd snapshot on every etcd node of the cluster, then restarts the Kubernetes server on each etcd node, " +
		"one node at a time; each snapshot appears as an etcd-snapshot object in the cluster's namespace once Rancher reads it from the cluster"

	etcdSnapshotRestoreWarning = "Rancher stops Kubernetes on every node of the cluster (its API server is down until the restore ends), " +
		"removes the server certificate and credential directories (server/tls, server/cred) on the etcd and control plane nodes, " +
		"replaces the cluster's etcd data with the snapshot in spec.args.name, then starts the nodes again one at a time: " +
		"every Kubernetes object created or changed after the snapshot was taken is lost, objects deleted since then come back, " +
		"workloads run again from the restored objects, and Rancher deletes the Node objects of nodes that are no longer part of the cluster; " +
		"deleting this object does not undo the restore, and a restore that fails can leave the cluster stopped"

	encryptionKeyRotationWarning = "Rancher creates a new secrets encryption key for the cluster and re-encrypts every Secret with it, " +
		"then restarts the Kubernetes server on each etcd and control plane node, one node at a time; " +
		"deleting this object does not undo the rotation, and Rancher asks for an etcd restore when a rotation fails: " +
		"changes made after the newest etcd snapshot cannot be restored then"

	certificateRotationWarning = "Rancher issues new certificates for the services in spec.args.services, every service when it is empty, " +
		"and restarts those services one node at a time; deleting this object does not undo the rotation"
)

// Phases of a day-2 operation object (status.phase). An operation that has
// ended is in one of the last three.
const (
	phasePending   = "Pending"
	phaseSucceeded = "Succeeded"
	phaseFailed    = "Failed"
	phaseCanceled  = "Canceled"
)

// managementClusterAPIVersion is the apiVersion of the object spec.clusterRef
// names: a management Cluster.
const managementClusterAPIVersion = "management.cattle.io/v3"

// importedDay2Checks returns the object checks of the row for a day-2
// operation of an imported cluster: kindName is the row's Name and warning its
// CreateWarning. needsSnapshotName is true for a restore, whose spec.args.name
// must name an etcd snapshot.
func importedDay2Checks(kindName, warning string, needsSnapshotName bool) *ObjectChecks {
	return &ObjectChecks{
		Create: func(obj *unstructured.Unstructured) Verdict {
			return checkDay2Create(kindName, needsSnapshotName, obj)
		},
		Patch: func(orig, patched *unstructured.Unstructured) Verdict {
			return checkDay2Patch(kindName, warning, orig, patched)
		},
		Delete: func(obj *unstructured.Unstructured) Verdict {
			return checkDay2Delete(kindName, obj)
		},
	}
}

// checkDay2Create runs the checks of the manifest alone: spec.clusterRef names
// a management Cluster as the Rancher UI writes it, the object's namespace is
// that cluster's id, a restore names its snapshot, spec.ttl is not 0 (a
// warning) and the object is not paused.
func checkDay2Create(kindName string, needsSnapshotName bool, obj *unstructured.Unstructured) Verdict {
	ref, _, _ := unstructured.NestedMap(obj.Object, "spec", "clusterRef")
	apiVersion, _ := ref["apiVersion"].(string)
	kind, _ := ref["kind"].(string)
	name, _ := ref["name"].(string)
	namespace, _ := ref["namespace"].(string)
	if apiVersion != managementClusterAPIVersion || kind != "Cluster" || namespace != "" || name == "" {
		return Verdict{Refusal: "spec.clusterRef must name a management cluster: apiVersion management.cattle.io/v3, kind Cluster, as the Rancher UI sets it"}
	}
	if ns := obj.GetNamespace(); ns != name {
		return Verdict{Refusal: fmt.Sprintf("the namespace %s must be the cluster's id %s from spec.clusterRef.name, as the Rancher UI creates it", ns, name)}
	}
	if needsSnapshotName {
		if snapshot, _, _ := unstructured.NestedString(obj.Object, "spec", "args", "name"); snapshot == "" {
			return Verdict{Refusal: "spec.args.name must name an etcd-snapshot of the cluster; without it Rancher stops the cluster before it fails"}
		}
	}
	if paused, _, _ := unstructured.NestedBool(obj.Object, "spec", "paused"); paused {
		return Verdict{Refusal: fmt.Sprintf("create the %s without spec.paused: true; Rancher does not start a paused day-2 operation, and this CLI does not un-pause one that has not started", kindName)}
	}
	var warnings []string
	if ttlIsZero(obj) {
		warnings = append(warnings, "spec.ttl is 0: Rancher deletes this object a few seconds after the day-2 operation ends, and a wait can then answer NOT_FOUND; the Rancher UI sets spec.ttl: 60")
	}
	return Verdict{Warnings: warnings}
}

// ttlIsZero reports whether spec.ttl is absent or the number 0. A value that
// is not a number is left to the server's schema.
func ttlIsZero(obj *unstructured.Unstructured) bool {
	v, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "ttl")
	if !found || v == nil {
		return true
	}
	switch n := v.(type) {
	case int64:
		return n == 0
	case int32:
		return n == 0
	case int:
		return n == 0
	case float64:
		return n == 0
	case json.Number:
		f, err := n.Float64()
		return err == nil && f == 0
	}
	return false
}

// checkDay2Patch refuses a change under spec other than spec.paused and
// spec.ttl, and an un-pause of an object that has not started. An un-pause of
// an object that is part way through carries the row's create warning.
func checkDay2Patch(kindName, warning string, orig, patched *unstructured.Unstructured) Verdict {
	if path, changed := changedSpecPath(orig, patched); changed {
		return Verdict{Refusal: fmt.Sprintf("only spec.paused and spec.ttl of a %s object can change; create a new %s to change spec.%s", kindName, kindName, path)}
	}
	wasPaused, _, _ := unstructured.NestedBool(orig.Object, "spec", "paused")
	isPaused, _, _ := unstructured.NestedBool(patched.Object, "spec", "paused")
	if !wasPaused || isPaused {
		return Verdict{}
	}
	phase, _, _ := unstructured.NestedString(orig.Object, "status", "phase")
	switch {
	case phase == "" || phase == phasePending:
		return Verdict{Refusal: fmt.Sprintf("this %s has not started; delete it and create a new %s from a manifest, which checks the cluster again", kindName, kindName)}
	case operationEnded(phase):
		return Verdict{}
	}
	return Verdict{Warnings: []string{"resuming: " + warning}}
}

// checkDay2Delete warns when the object has not ended: Rancher's work on the
// cluster stops part way.
func checkDay2Delete(kindName string, obj *unstructured.Unstructured) Verdict {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	if operationEnded(phase) {
		return Verdict{}
	}
	return Verdict{Warnings: []string{fmt.Sprintf("deleting this %s before it ends stops Rancher's work on it part way, which can leave the cluster in an unstable state", kindName)}}
}

// operationEnded reports whether a status.phase is one of the three that no
// further work follows.
func operationEnded(phase string) bool {
	return phase == phaseSucceeded || phase == phaseFailed || phase == phaseCanceled
}

// changedSpecPath returns the first path, in sorted order and below spec, at
// which the two objects differ, leaving out spec.paused and spec.ttl. The path
// is dotted and has no "spec." lead. A list is one value, compared whole; an
// empty map is one value.
func changedSpecPath(orig, patched *unstructured.Unstructured) (string, bool) {
	before, after := specLeaves(orig), specLeaves(patched)
	var paths []string
	for path := range before {
		paths = append(paths, path)
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	for _, path := range paths {
		if first, _, _ := strings.Cut(path, "."); first == "paused" || first == "ttl" {
			continue
		}
		b, inBefore := before[path]
		a, inAfter := after[path]
		if inBefore != inAfter || !bytes.Equal(marshalLeaf(b), marshalLeaf(a)) {
			return path, true
		}
	}
	return "", false
}

// specLeaves flattens the object's spec into dotted paths and values.
func specLeaves(obj *unstructured.Unstructured) map[string]any {
	out := map[string]any{}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for key, v := range m {
			path := prefix + key
			if child, ok := v.(map[string]any); ok && len(child) > 0 {
				walk(path+".", child)
				continue
			}
			out[path] = v
		}
	}
	walk("", spec)
	return out
}

// marshalLeaf is the JSON form of a leaf, so that 60 as an int64 and as a
// float64 compare equal.
func marshalLeaf(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf("%#v", v))
	}
	return b
}
