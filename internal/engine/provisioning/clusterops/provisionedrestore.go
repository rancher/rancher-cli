package clusterops

import "slices"

// clusterNameLabel is the label of an etcd snapshot object that names its
// cluster; the Rancher UI lists a provisioned cluster's snapshots by it.
const clusterNameLabel = "rke.cattle.io/cluster-name"

// ProvisionedRestoreProblem is the reason an etcd snapshot object cannot be
// restored to a provisioned cluster with the requested restore mode, in the
// order CheckProvisionedRestore finds them.
type ProvisionedRestoreProblem int

const (
	// ProvisionedRestoreAccepted: no problem.
	ProvisionedRestoreAccepted ProvisionedRestoreProblem = iota
	// ProvisionedRestoreNoClusterName: spec.clusterName is empty.
	ProvisionedRestoreNoClusterName
	// ProvisionedRestoreOtherCluster: spec.clusterName names another cluster.
	ProvisionedRestoreOtherCluster
	// ProvisionedRestoreOtherLabel: the label rke.cattle.io/cluster-name is
	// present and names another cluster.
	ProvisionedRestoreOtherLabel
	// ProvisionedRestoreNotSuccessful: snapshotFile.status is not successful.
	ProvisionedRestoreNotSuccessful
	// ProvisionedRestoreNoCreatedAt: snapshotFile.createdAt is absent or not an
	// RFC 3339 time.
	ProvisionedRestoreNoCreatedAt
	// ProvisionedRestoreBeforeCluster: snapshotFile.createdAt is at or before the
	// metadata.creationTimestamp of the provisioning Cluster.
	ProvisionedRestoreBeforeCluster
	// ProvisionedRestoreBadVersion: the snapshot's metadata decodes and its
	// Kubernetes version is empty or one Rancher cannot parse.
	ProvisionedRestoreBadVersion
	// ProvisionedRestoreModeNotOffered: the requested restore mode is not among
	// the modes offered.
	ProvisionedRestoreModeNotOffered
	// ProvisionedRestoreBelow125: restore mode none, a snapshot taken before
	// Kubernetes 1.25 and a cluster at 1.25 or newer, which Rancher refuses.
	ProvisionedRestoreBelow125
)

// ProvisionedRestore is the result of CheckProvisionedRestore.
type ProvisionedRestore struct {
	// Problem is the first problem found; the other fields are set up to the
	// step that found it.
	Problem ProvisionedRestoreProblem
	// Spec is what the snapshot's metadata has of the cluster spec.
	Spec SnapshotSpec
	// Modes are the restore modes offered for the snapshot, in the order none,
	// kubernetesVersion, all.
	Modes []string
	// Mode is the restore mode to write: the requested one, or the default among
	// Modes.
	Mode string
}

// CheckProvisionedRestore applies to an etcd snapshot object and a
// provisioning Cluster the rules the Rancher UI uses to offer a restore and
// the rule of Rancher's planner for a snapshot taken before Kubernetes 1.25.
// requestedMode is the restore mode the caller asked for, "" when it asked for
// no mode, which gives the Rancher UI's default among the modes offered. The
// problems are returned in the order of the ProvisionedRestoreProblem
// constants.
//
// The snapshot must name the cluster in spec.clusterName and, when it has the
// label rke.cattle.io/cluster-name, in the label; it must be successful and
// taken after the cluster was created. The restore modes offered: none always;
// kubernetesVersion and all as the snapshot's annotation
// etcdsnapshot.rke.io/restore-mode-options lists them (all three without
// one), and as the cluster spec in the snapshot's metadata allows: all needs
// an RKE configuration in it, and a snapshot whose metadata cannot be decoded
// offers none only. With a spec too large to read, the modes follow the annotation alone. A
// decoded spec whose Kubernetes version Rancher cannot parse is a problem for
// every mode, because Rancher fails such a restore on every attempt.
func CheckProvisionedRestore(snapshot, cluster map[string]any, requestedMode string) ProvisionedRestore {
	name := nestedString(cluster, "metadata", "name")
	problem := CheckRestorableSnapshot(snapshot, cluster, name)
	switch problem {
	case SnapshotNoClusterName:
		return ProvisionedRestore{Problem: ProvisionedRestoreNoClusterName}
	case SnapshotOtherCluster:
		return ProvisionedRestore{Problem: ProvisionedRestoreOtherCluster}
	}
	if label, found := nested(snapshot, "metadata", "labels", clusterNameLabel); found {
		if value, _ := label.(string); value != name {
			return ProvisionedRestore{Problem: ProvisionedRestoreOtherLabel}
		}
	}
	switch problem {
	case SnapshotNotSuccessful:
		return ProvisionedRestore{Problem: ProvisionedRestoreNotSuccessful}
	case SnapshotNoCreatedAt:
		return ProvisionedRestore{Problem: ProvisionedRestoreNoCreatedAt}
	case SnapshotBeforeImport:
		return ProvisionedRestore{Problem: ProvisionedRestoreBeforeCluster}
	}

	result := ProvisionedRestore{Spec: DecodeSnapshotSpec(snapshot)}
	result.Modes = RestoreModes(true, snapshot)
	switch result.Spec.State {
	case SnapshotSpecRead:
		if !KubernetesVersionParses(result.Spec.KubernetesVersion) {
			result.Problem = ProvisionedRestoreBadVersion
			return result
		}
		if !result.Spec.HasRKEConfig {
			result.Modes = slices.DeleteFunc(result.Modes, func(mode string) bool { return mode == RestoreModeAll })
		}
	case SnapshotSpecUnreadable:
		result.Modes = []string{RestoreModeNone}
	}
	if !slices.Contains(result.Modes, RestoreModeNone) {
		result.Modes = slices.Insert(result.Modes, 0, RestoreModeNone)
	}
	if requestedMode != "" && !slices.Contains(result.Modes, requestedMode) {
		result.Problem = ProvisionedRestoreModeNotOffered
		return result
	}
	result.Mode = requestedMode
	if result.Mode == "" {
		result.Mode = DefaultRestoreMode(result.Modes)
	}

	if result.Mode == RestoreModeNone && result.Spec.State == SnapshotSpecRead {
		below, snapshotParses := BelowKubernetes125(result.Spec.KubernetesVersion)
		clusterBelow, clusterParses := BelowKubernetes125(nestedString(cluster, "spec", "kubernetesVersion"))
		if snapshotParses && clusterParses && below && !clusterBelow {
			result.Problem = ProvisionedRestoreBelow125
		}
	}
	return result
}
