package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// provisioningCluster builds a provisioning Cluster as decoded JSON, with the
// given spec.rkeConfig fields and spec.kubernetesVersion.
func provisioningCluster(version string, rkeConfig map[string]any) map[string]any {
	spec := map[string]any{"kubernetesVersion": version}
	if rkeConfig != nil {
		spec["rkeConfig"] = rkeConfig
	}
	return map[string]any{"spec": spec}
}

// controlPlane builds an RKEControlPlane as decoded JSON with the given
// status fields and a Ready condition carrying ready as its message.
func controlPlane(status map[string]any, ready string) map[string]any {
	if status == nil {
		status = map[string]any{}
	}
	if ready != "" {
		status["conditions"] = []any{
			map[string]any{"type": "Updated", "status": "True", "message": "not this one"},
			map[string]any{"type": "Ready", "status": "False", "message": ready},
		}
	}
	return map[string]any{"status": status}
}

func kinds(ops []Operation) []OperationKind {
	var out []OperationKind
	for _, op := range ops {
		out = append(out, op.Kind)
	}
	return out
}

// TestRunningOperationsStartAndEnd asserts every operation is reported from
// the time its request differs from the status copy until its phase ends it,
// and not otherwise, with the phase, the Ready message and the request of each
// reported operation.
func TestRunningOperationsStartAndEnd(t *testing.T) {
	t.Parallel()
	const version = "v1.31.4+rke2r1"
	type row struct {
		name   string
		rke    map[string]any
		status map[string]any
		want   []OperationKind
		phase  string
	}
	snap := func(g any) map[string]any { return map[string]any{"generation": g} }
	rows := []row{
		{name: "nothing requested", rke: map[string]any{}, status: map[string]any{}},
		{name: "no rkeConfig", status: map[string]any{}},

		{name: "snapshot request, no status copy", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{}, want: []OperationKind{OperationSnapshot}},
		{name: "snapshot request, lower generation copy", rke: map[string]any{"etcdSnapshotCreate": snap(int64(2))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Finished"}, want: []OperationKind{OperationSnapshot}, phase: "Finished"},
		{name: "snapshot started", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Started"}, want: []OperationKind{OperationSnapshot}, phase: "Started"},
		{name: "snapshot restarting the cluster", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "RestartCluster"}, want: []OperationKind{OperationSnapshot}, phase: "RestartCluster"},
		{name: "snapshot with an equal copy and no phase", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, want: []OperationKind{OperationSnapshot}},
		{name: "snapshot with an equal copy and an unknown phase", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Other"}, want: []OperationKind{OperationSnapshot}, phase: "Other"},
		{name: "snapshot finished", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Finished"}},
		{name: "snapshot failed", rke: map[string]any{"etcdSnapshotCreate": snap(int64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Failed"}},
		{name: "snapshot finished, int64 against float64", rke: map[string]any{"etcdSnapshotCreate": snap(float64(1))}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Finished"}},
		{name: "snapshot finished, absent field against zero", rke: map[string]any{"etcdSnapshotCreate": map[string]any{}}, status: map[string]any{"etcdSnapshotCreate": snap(int64(0)), "etcdSnapshotCreatePhase": "Finished"}},
		{name: "snapshot request removed, status copy kept", rke: map[string]any{}, status: map[string]any{"etcdSnapshotCreate": snap(int64(1)), "etcdSnapshotCreatePhase": "Started"}},

		{name: "restore with no name", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"generation": int64(1)}}, status: map[string]any{}},
		{name: "restore, no status copy", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}}, status: map[string]any{}, want: []OperationKind{OperationRestore}},
		{name: "restore mid-way", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}}, status: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}, "etcdSnapshotRestorePhase": "Restore"}, want: []OperationKind{OperationRestore}, phase: "Restore"},
		{name: "restore, equal copy and no phase", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}}, status: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}}, want: []OperationKind{OperationRestore}},
		{name: "restore, another snapshot name", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s2", "generation": int64(1)}}, status: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}, "etcdSnapshotRestorePhase": "Finished"}, want: []OperationKind{OperationRestore}, phase: "Finished"},
		{name: "restore finished", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1), "restoreRKEConfig": "none"}}, status: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": float64(1), "restoreRKEConfig": "none"}, "etcdSnapshotRestorePhase": "Finished"}},
		{name: "restore failed", rke: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}}, status: map[string]any{"etcdSnapshotRestore": map[string]any{"name": "s1", "generation": int64(1)}, "etcdSnapshotRestorePhase": "Failed"}},

		{name: "certificates requested, no status generation", rke: map[string]any{"rotateCertificates": map[string]any{"generation": int64(1)}}, status: map[string]any{}, want: []OperationKind{OperationCertificateRotation}},
		{name: "certificates, another generation", rke: map[string]any{"rotateCertificates": map[string]any{"generation": int64(3)}}, status: map[string]any{"certificateRotationGeneration": int64(2)}, want: []OperationKind{OperationCertificateRotation}},
		{name: "certificates done", rke: map[string]any{"rotateCertificates": map[string]any{"generation": int64(2)}}, status: map[string]any{"certificateRotationGeneration": float64(2)}},
		{name: "certificates generation 0 against none", rke: map[string]any{"rotateCertificates": map[string]any{"generation": int64(0)}}, status: map[string]any{}},
		{name: "certificates request removed, status generation kept", rke: map[string]any{}, status: map[string]any{"certificateRotationGeneration": int64(2)}},

		{name: "keys generation 0", rke: map[string]any{"rotateEncryptionKeys": snap(int64(0))}, status: map[string]any{}},
		{name: "keys, no status copy", rke: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, status: map[string]any{}, want: []OperationKind{OperationEncryptionKeyRotation}},
		{name: "keys, lower generation", rke: map[string]any{"rotateEncryptionKeys": snap(int64(2))}, status: map[string]any{"rotateEncryptionKeys": snap(int64(1)), "rotateEncryptionKeysPhase": "Done"}, want: []OperationKind{OperationEncryptionKeyRotation}, phase: "Done"},
		{name: "keys rotating", rke: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, status: map[string]any{"rotateEncryptionKeys": snap(int64(1)), "rotateEncryptionKeysPhase": "Rotate"}, want: []OperationKind{OperationEncryptionKeyRotation}, phase: "Rotate"},
		{name: "keys, equal copy and no phase", rke: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, status: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, want: []OperationKind{OperationEncryptionKeyRotation}},
		{name: "keys done", rke: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, status: map[string]any{"rotateEncryptionKeys": snap(int64(1)), "rotateEncryptionKeysPhase": "Done"}},
		{name: "keys failed", rke: map[string]any{"rotateEncryptionKeys": snap(int64(1))}, status: map[string]any{"rotateEncryptionKeys": snap(int64(1)), "rotateEncryptionKeysPhase": "Failed"}},

		{name: "no applied spec", rke: map[string]any{}, status: map[string]any{}},
		{name: "applied version equal", rke: map[string]any{}, status: map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": version}}},
		{name: "applied version different", rke: map[string]any{}, status: map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.5+rke2r1"}}, want: []OperationKind{OperationUpgrade}},

		{name: "several at once, in Rancher's order",
			rke: map[string]any{
				"etcdSnapshotCreate":   snap(int64(1)),
				"etcdSnapshotRestore":  map[string]any{"name": "s1", "generation": int64(1)},
				"rotateCertificates":   map[string]any{"generation": int64(1)},
				"rotateEncryptionKeys": snap(int64(1)),
			},
			status: map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.5+rke2r1"}},
			want:   []OperationKind{OperationSnapshot, OperationRestore, OperationCertificateRotation, OperationEncryptionKeyRotation, OperationUpgrade},
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := provisioningCluster(version, tc.rke)
			ops := RunningOperations(cluster, controlPlane(tc.status, "waiting for the plan"))
			assert.Equal(t, tc.want, kinds(ops))
			for _, op := range ops {
				assert.Equal(t, "waiting for the plan", op.ReadyMessage)
			}
			if len(tc.want) == 1 {
				assert.Equal(t, tc.phase, ops[0].Phase)
			}
		})
	}
}

// TestRunningOperationsRequest asserts each operation carries its request as
// written, and the upgrade the spec version.
func TestRunningOperationsRequest(t *testing.T) {
	t.Parallel()
	cluster := provisioningCluster("v1.31.4+rke2r1", map[string]any{
		"etcdSnapshotCreate": map[string]any{"generation": int64(4)},
	})
	cp := controlPlane(map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.5+rke2r1"}}, "")
	ops := RunningOperations(cluster, cp)
	if assert.Len(t, ops, 2) {
		assert.Equal(t, map[string]any{"generation": int64(4)}, ops[0].Request)
		assert.Equal(t, map[string]any{"kubernetesVersion": "v1.31.4+rke2r1"}, ops[1].Request)
		assert.Empty(t, ops[0].ReadyMessage)
	}
}

// TestRunningOperationsWithoutControlPlaneStatus asserts a control plane with
// no status reads as one that applied nothing: requests are running, an
// upgrade needs the applied spec.
func TestRunningOperationsWithoutControlPlaneStatus(t *testing.T) {
	t.Parallel()
	cluster := provisioningCluster("v1.31.4+rke2r1", map[string]any{"rotateCertificates": map[string]any{"generation": int64(1)}})
	assert.Equal(t, []OperationKind{OperationCertificateRotation}, kinds(RunningOperations(cluster, map[string]any{})))
	assert.Empty(t, RunningOperations(map[string]any{}, map[string]any{}))
}

// TestOperationKindWords asserts the string values are the phrases of the
// refusal text.
func TestOperationKindWords(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "etcd snapshot", string(OperationSnapshot))
	assert.Equal(t, "etcd snapshot restore", string(OperationRestore))
	assert.Equal(t, "certificate rotation", string(OperationCertificateRotation))
	assert.Equal(t, "encryption key rotation", string(OperationEncryptionKeyRotation))
	assert.Equal(t, "Kubernetes upgrade", string(OperationUpgrade))
}
