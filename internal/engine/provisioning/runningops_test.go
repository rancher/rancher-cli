package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/stretchr/testify/assert"
)

// TestRunningOperationErrorWithoutAHint asserts the refusal for every kind of
// operation is GENERIC and retryable, names the operation with its article and
// its phase, and carries no hint for an operation whose command does not exist in this build.
func TestRunningOperationErrorWithoutAHint(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		op   clusterops.Operation
		want string
	}{
		{"restore with a message", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Restore", ReadyMessage: "waiting for etcd"},
			"cluster prod has an etcd snapshot restore in progress (phase Restore: waiting for etcd); Rancher runs one operation at a time; try again after it ends"},
		{"certificate rotation", clusterops.Operation{Kind: clusterops.OperationCertificateRotation, ReadyMessage: "rotating"},
			"cluster prod has a certificate rotation in progress (rotating); Rancher runs one operation at a time; try again after it ends"},
		{"certificate rotation, no message", clusterops.Operation{Kind: clusterops.OperationCertificateRotation},
			"cluster prod has a certificate rotation in progress; Rancher runs one operation at a time; try again after it ends"},
		{"encryption key rotation", clusterops.Operation{Kind: clusterops.OperationEncryptionKeyRotation, Phase: "Rotate", ReadyMessage: "keys"},
			"cluster prod has an encryption key rotation in progress (phase Rotate: keys); Rancher runs one operation at a time; try again after it ends"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runningOperationError("prod", "fleet-default", tc.op)
			assert.Equal(t, tc.want, err.Message)
			assert.Equal(t, clierr.CodeGeneric, err.Code)
			assert.True(t, err.Retryable)
			assert.Empty(t, err.Hint)
		})
	}
}

// TestRunningOperationUpgradeHint asserts the refusal for a running upgrade
// carries the preview of cluster upgrade with the running version, and the
// namespace only when it is not fleet-default.
func TestRunningOperationUpgradeHint(t *testing.T) {
	t.Parallel()
	op := clusterops.Operation{Kind: clusterops.OperationUpgrade, ReadyMessage: "waiting", Request: map[string]any{"kubernetesVersion": "v1.31.4+rke2r1"}}
	err := runningOperationError("prod", "fleet-default", op)
	assert.Equal(t, "cluster prod has a Kubernetes upgrade in progress (waiting); Rancher runs one operation at a time; to wait for it, run the command in the hint with --apply --yes and without --dry-run; while the operation runs, that command changes nothing and waits", err.Message)
	assert.Equal(t, clierr.NewCommand("cluster upgrade").Arg("prod").FlagValue("--k8s-version", "v1.31.4+rke2r1").Flag("--dry-run").Field(), err.Hint)
	err = runningOperationError("prod", "ws-two", op)
	assert.True(t, err.Retryable)
	assert.Equal(t, clierr.CodeGeneric, err.Code)
	assert.Equal(t, clierr.NewCommand("cluster upgrade").Arg("prod").FlagValue("--k8s-version", "v1.31.4+rke2r1").FlagValue("--namespace", "ws-two").Flag("--dry-run").Field(), err.Hint)
}

// TestRunningOperationSnapshotHint asserts the refusal for an etcd snapshot
// in progress names it with the article "an" and the phase, and carries the
// preview of cluster save-etcd-snapshot, with the namespace only when it is
// not fleet-default.
func TestRunningOperationSnapshotHint(t *testing.T) {
	t.Parallel()
	const wait = "to wait for it, run the command in the hint with --apply --yes and without --dry-run; while the operation runs, that command changes nothing and waits"
	rows := []struct {
		name string
		ns   string
		op   clusterops.Operation
		want string
		hint clierr.Command
	}{
		{"started", "fleet-default", clusterops.Operation{Kind: clusterops.OperationSnapshot, Phase: "Started"},
			"cluster prod has an etcd snapshot in progress (phase Started); Rancher runs one operation at a time; " + wait,
			clierr.NewCommand("cluster save-etcd-snapshot").Arg("prod").Flag("--dry-run")},
		{"not started, with a message", "fleet-default", clusterops.Operation{Kind: clusterops.OperationSnapshot, ReadyMessage: "waiting for agent"},
			"cluster prod has an etcd snapshot in progress (phase requested, not started: waiting for agent); Rancher runs one operation at a time; " + wait,
			clierr.NewCommand("cluster save-etcd-snapshot").Arg("prod").Flag("--dry-run")},
		{"another namespace", "ws-two", clusterops.Operation{Kind: clusterops.OperationSnapshot, Phase: "RestartCluster"},
			"cluster prod has an etcd snapshot in progress (phase RestartCluster); Rancher runs one operation at a time; " + wait,
			clierr.NewCommand("cluster save-etcd-snapshot").Arg("prod").FlagValue("--namespace", "ws-two").Flag("--dry-run")},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runningOperationError("prod", tc.ns, tc.op)
			assert.Equal(t, tc.want, err.Message)
			assert.Equal(t, clierr.CodeGeneric, err.Code)
			assert.True(t, err.Retryable)
			assert.Equal(t, tc.hint.Field(), err.Hint)
		})
	}
}
