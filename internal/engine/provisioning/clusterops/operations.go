package clusterops

import "slices"

// OperationKind names an operation Rancher runs on a provisioned cluster.
// The string value is the phrase the refusal text uses.
type OperationKind string

const (
	OperationSnapshot              OperationKind = "etcd snapshot"
	OperationRestore               OperationKind = "etcd snapshot restore"
	OperationCertificateRotation   OperationKind = "certificate rotation"
	OperationEncryptionKeyRotation OperationKind = "encryption key rotation"
	OperationUpgrade               OperationKind = "Kubernetes upgrade"
)

// Operation is one operation Rancher is running on a provisioned cluster.
type Operation struct {
	Kind OperationKind
	// Phase is the phase Rancher reports in the status of the RKEControlPlane:
	// "" when it reports none, and always "" for the certificate rotation and
	// the upgrade, which have no phase.
	Phase string
	// ReadyMessage is the message of the RKEControlPlane's Ready condition.
	ReadyMessage string
	// Request is the request as written: the value of the field of
	// spec.rkeConfig that starts the operation, and for the upgrade
	// {"kubernetesVersion": <spec.kubernetesVersion>}.
	Request map[string]any
}

// RunningOperations returns every operation Rancher is running on a
// provisioned cluster, in the order snapshot, restore, certificate rotation,
// encryption key rotation, upgrade. cluster is the provisioning Cluster and
// controlPlane its RKEControlPlane, both as read. An operation runs from the
// time its request differs from the copy Rancher keeps in the status of the
// RKEControlPlane until its phase says it ended.
func RunningOperations(cluster, controlPlane map[string]any) []Operation {
	var ops []Operation
	ready := conditionMessage(controlPlane, "Ready")
	add := func(kind OperationKind, phase string, request map[string]any) {
		ops = append(ops, Operation{Kind: kind, Phase: phase, ReadyMessage: ready, Request: request})
	}

	if req := nestedMap(cluster, "spec", "rkeConfig", "etcdSnapshotCreate"); req != nil {
		phase := nestedString(controlPlane, "status", "etcdSnapshotCreatePhase")
		if copyDiffers(req, controlPlane, "etcdSnapshotCreate") || !endedIn(phase, "Finished", "Failed") {
			add(OperationSnapshot, phase, req)
		}
	}

	if req := nestedMap(cluster, "spec", "rkeConfig", "etcdSnapshotRestore"); req != nil && nestedString(req, "name") != "" {
		phase := nestedString(controlPlane, "status", "etcdSnapshotRestorePhase")
		if copyDiffers(req, controlPlane, "etcdSnapshotRestore") || !endedIn(phase, "Finished", "Failed") {
			add(OperationRestore, phase, req)
		}
	}

	if req := nestedMap(cluster, "spec", "rkeConfig", "rotateCertificates"); req != nil {
		applied, _ := nested(controlPlane, "status", "certificateRotationGeneration")
		if !sameValue(req["generation"], applied) {
			add(OperationCertificateRotation, "", req)
		}
	}

	if req := nestedMap(cluster, "spec", "rkeConfig", "rotateEncryptionKeys"); req != nil && generation(req) > 0 {
		phase := nestedString(controlPlane, "status", "rotateEncryptionKeysPhase")
		if nestedMap(controlPlane, "status", "rotateEncryptionKeys") == nil ||
			copyDiffers(req, controlPlane, "rotateEncryptionKeys") || !endedIn(phase, "Done", "Failed") {
			add(OperationEncryptionKeyRotation, phase, req)
		}
	}

	if applied := nestedMap(controlPlane, "status", "appliedSpec"); applied != nil {
		want := nestedString(cluster, "spec", "kubernetesVersion")
		if nestedString(applied, "kubernetesVersion") != want {
			add(OperationUpgrade, "", map[string]any{"kubernetesVersion": want})
		}
	}

	return ops
}

// copyDiffers reports whether the copy of a request that Rancher keeps in
// status.<field> of the RKEControlPlane is absent or differs from the
// request. Fields compare as Rancher's comparison of the Go structs does: an
// absent field equals its zero value.
func copyDiffers(request, controlPlane map[string]any, field string) bool {
	applied := nestedMap(controlPlane, "status", field)
	return applied == nil || !sameValue(request, applied)
}

func endedIn(phase string, ended ...string) bool {
	return slices.Contains(ended, phase)
}

// generation returns the generation field of a request, 0 when absent.
func generation(request map[string]any) float64 {
	n, _ := number(request["generation"])
	return n
}

// number returns the numeric value of a decoded JSON value whatever its Go
// type: int64 from an unstructured object and float64 from encoding/json
// compare equal.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// isZero reports whether a decoded JSON value is the zero value of its type
// or an empty collection, which the objects' omitempty fields leave out.
func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	n, ok := number(v)
	return ok && n == 0
}

// sameValue compares two decoded JSON values field by field: an absent field
// equals its zero value and numbers compare by value.
func sameValue(a, b any) bool {
	if isZero(a) && isZero(b) {
		return true
	}
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range x {
			if !sameValue(v, y[k]) {
				return false
			}
		}
		for k, v := range y {
			if _, seen := x[k]; !seen && !isZero(v) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return false
}
