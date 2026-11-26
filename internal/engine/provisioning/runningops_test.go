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

// TestRunningOperationRestoreHint asserts the refusal for an etcd snapshot
// restore in progress names it with the article "an" and the phase, is not
// retryable, advises no run with --apply --yes, and carries the preview of
// cluster restore-etcd-snapshot with the running snapshot and restore mode
// (none for an empty one, no restore mode for a value of any other kind), and
// the namespace only when it is not fleet-default.
func TestRunningOperationRestoreHint(t *testing.T) {
	t.Parallel()
	const wait = "to see its phase, run the command in the hint, which has --dry-run: while the restore is in progress it sends no write and shows the phase; once the restore has ended it previews a new restore, which --apply --yes would request"
	restore := func(mode any) map[string]any {
		return map[string]any{"name": "snap-1", "generation": int64(2), "restoreRKEConfig": mode}
	}
	command := func() clierr.Command {
		return clierr.NewCommand("cluster restore-etcd-snapshot").Arg("prod").FlagValue("--snapshot", "snap-1")
	}
	rows := []struct {
		name string
		ns   string
		op   clusterops.Operation
		want string
		hint clierr.Command
	}{
		{"all, with a message", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Restore", ReadyMessage: "waiting for etcd", Request: restore("all")},
			"cluster prod has an etcd snapshot restore in progress (phase Restore: waiting for etcd); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "all").Flag("--dry-run")},
		{"kubernetesVersion, not started", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Request: restore("kubernetesVersion")},
			"cluster prod has an etcd snapshot restore in progress (phase requested, not started); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "kubernetesVersion").Flag("--dry-run")},
		{"none", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Started", Request: restore("none")},
			"cluster prod has an etcd snapshot restore in progress (phase Started); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "none").Flag("--dry-run")},
		{"empty mode reads as none", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Started", Request: restore("")},
			"cluster prod has an etcd snapshot restore in progress (phase Started); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "none").Flag("--dry-run")},
		{"absent mode reads as none", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Started", Request: map[string]any{"name": "snap-1"}},
			"cluster prod has an etcd snapshot restore in progress (phase Started); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "none").Flag("--dry-run")},
		{"another value has no mode in the hint", "fleet-default", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "Started", Request: restore("everything")},
			"cluster prod has an etcd snapshot restore in progress (phase Started); Rancher runs one operation at a time; " + wait,
			command().Flag("--dry-run")},
		{"another namespace", "ws-two", clusterops.Operation{Kind: clusterops.OperationRestore, Phase: "RestartCluster", Request: restore("all")},
			"cluster prod has an etcd snapshot restore in progress (phase RestartCluster); Rancher runs one operation at a time; " + wait,
			command().FlagValue("--restore-mode", "all").FlagValue("--namespace", "ws-two").Flag("--dry-run")},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := runningOperationError("prod", tc.ns, tc.op)
			assert.Equal(t, tc.want, err.Message)
			assert.Equal(t, clierr.CodeGeneric, err.Code)
			assert.False(t, err.Retryable)
			assert.Equal(t, tc.hint.Field(), err.Hint)
			assert.NotContains(t, err.Message, "to wait for it")
		})
	}
}

// TestOnlyTheRestoreRefusalIsNotRetryable asserts the refusal for every kind
// of operation in progress is retryable except the one for an etcd snapshot
// restore: a run of the command in the hint after the restore ended previews a new
// restore.
func TestOnlyTheRestoreRefusalIsNotRetryable(t *testing.T) {
	t.Parallel()
	kinds := []clusterops.OperationKind{
		clusterops.OperationSnapshot, clusterops.OperationRestore, clusterops.OperationCertificateRotation,
		clusterops.OperationEncryptionKeyRotation, clusterops.OperationUpgrade,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			err := runningOperationError("prod", "fleet-default", clusterops.Operation{Kind: kind, Request: map[string]any{"name": "s1"}})
			assert.Equal(t, kind != clusterops.OperationRestore, err.Retryable)
		})
	}
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
