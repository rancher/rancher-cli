package clusterops

import (
	"slices"
	"strings"
	"time"
)

// The restore modes of an etcd snapshot restore, from the most basic to the
// most complete, and the annotation that lists the modes a snapshot offers.
const (
	RestoreModeNone              = "none"
	RestoreModeKubernetesVersion = "kubernetesVersion"
	RestoreModeAll               = "all"

	// restoreModeOptionsAnnotation is the annotation of an etcd snapshot
	// object that lists, comma separated, the restore modes it offers.
	restoreModeOptionsAnnotation = "etcdsnapshot.rke.io/restore-mode-options"
)

// CommandDistribution is the distribution Rancher runs the commands of a
// day-2 operation for: rke2 when status.provider of the management Cluster is
// rke2, else k3s. It can differ from the distribution ImportedDistribution
// finds, which reads status.driver.
func CommandDistribution(managementCluster map[string]any) string {
	if nestedString(managementCluster, "status", "provider") == distroRKE2 {
		return distroRKE2
	}
	return distroK3s
}

// CertificateServices are the services of a cluster whose certificates a
// certificate rotation can name, in the order the Rancher UI lists them, for an
// imported cluster of the distribution (rke2 or k3s; any other value is read as
// rke2, as DistroName does): the services every cluster has, the controller and
// server service of the distribution, and supervisor.
func CertificateServices(distro string) []string {
	services := []string{"admin", "api-server", "controller-manager", "scheduler", "cloud-controller", "etcd", "auth-proxy", "kubelet", "kube-proxy"}
	if distro == distroK3s {
		services = append(services, "k3s-controller", "k3s-server")
	} else {
		services = append(services, "rke2-controller", "rke2-server")
	}
	return append(services, "supervisor")
}

// FirstUnknownService returns the first of services that is not a service of
// the distribution (CertificateServices), and false when every one is.
func FirstUnknownService(distro string, services []string) (string, bool) {
	known := CertificateServices(distro)
	for _, s := range services {
		if !slices.Contains(known, s) {
			return s, true
		}
	}
	return "", false
}

// VersionManagementOn reports whether Rancher manages the Kubernetes version of
// an imported cluster, as the Rancher UI reads it: the annotation
// rancher.io/imported-cluster-version-management is "true", or it leaves the
// decision to the setting of the same name (NeedsVersionManagementSetting) and
// the value of the Setting, which the caller reads only then, is "true".
func VersionManagementOn(managementCluster map[string]any, settingValue string) bool {
	if nestedString(managementCluster, "metadata", "annotations", versionManagementAnnotation) == "true" {
		return true
	}
	return NeedsVersionManagementSetting(managementCluster) && settingValue == "true"
}

// RestoreModes are the restore modes the Rancher UI offers for an etcd
// snapshot object of a cluster, in the order none, kubernetesVersion, all:
// none always; kubernetesVersion and all only while version management is on;
// of those, only the modes the snapshot's annotation
// etcdsnapshot.rke.io/restore-mode-options lists, when it lists any; none when
// no mode is left.
func RestoreModes(versionManagement bool, snapshot map[string]any) []string {
	modes := []string{RestoreModeNone}
	if versionManagement {
		modes = append(modes, RestoreModeKubernetesVersion, RestoreModeAll)
	}
	var listed []string
	for mode := range strings.SplitSeq(nestedString(snapshot, "metadata", "annotations", restoreModeOptionsAnnotation), ",") {
		if mode = strings.TrimSpace(mode); mode != "" {
			listed = append(listed, mode)
		}
	}
	if len(listed) == 0 {
		return modes
	}
	modes = slices.DeleteFunc(modes, func(mode string) bool { return !slices.Contains(listed, mode) })
	if len(modes) == 0 {
		return []string{RestoreModeNone}
	}
	return modes
}

// DefaultRestoreMode is the mode the Rancher UI preselects among the offered
// ones: the most complete, all, then kubernetesVersion, then none.
func DefaultRestoreMode(offered []string) string {
	for _, mode := range []string{RestoreModeAll, RestoreModeKubernetesVersion} {
		if slices.Contains(offered, mode) {
			return mode
		}
	}
	return RestoreModeNone
}

// SnapshotProblem is the reason the Rancher UI does not restore an etcd
// snapshot object to a cluster; SnapshotRestorable is no problem.
type SnapshotProblem int

const (
	// SnapshotRestorable: the snapshot belongs to the cluster, is successful and
	// was taken after the cluster was created.
	SnapshotRestorable SnapshotProblem = iota
	// SnapshotNoClusterName: spec.clusterName is empty.
	SnapshotNoClusterName
	// SnapshotOtherCluster: spec.clusterName names another cluster.
	SnapshotOtherCluster
	// SnapshotNotSuccessful: snapshotFile.status is not successful.
	SnapshotNotSuccessful
	// SnapshotNoCreatedAt: snapshotFile.createdAt is absent or not an RFC 3339
	// time.
	SnapshotNoCreatedAt
	// SnapshotBeforeImport: snapshotFile.createdAt is at or before the
	// metadata.creationTimestamp of the cluster's management Cluster.
	SnapshotBeforeImport
)

// CheckRestorableSnapshot applies the rule the Rancher UI uses to offer a
// restore from an etcd snapshot object to the cluster with id clusterID, whose
// management Cluster is managementCluster: the snapshot's spec.clusterName
// names the cluster, snapshotFile.status is successful, and snapshotFile.createdAt
// is strictly after the management Cluster's metadata.creationTimestamp. A
// creationTimestamp that is absent or not an RFC 3339 time leaves the time
// comparison out. The problems are returned in that order. The cluster object
// may be the management Cluster of an imported cluster or the provisioning
// Cluster of a provisioned one, with the name of the provisioning Cluster as
// clusterID; both carry metadata.creationTimestamp.
func CheckRestorableSnapshot(snapshot, managementCluster map[string]any, clusterID string) SnapshotProblem {
	switch {
	case nestedString(snapshot, "spec", "clusterName") == "":
		return SnapshotNoClusterName
	case !SnapshotOfCluster(snapshot, clusterID):
		return SnapshotOtherCluster
	case nestedString(snapshot, "snapshotFile", "status") != "successful":
		return SnapshotNotSuccessful
	}
	taken, ok := snapshotTime(snapshot)
	if !ok {
		return SnapshotNoCreatedAt
	}
	created, err := time.Parse(time.RFC3339, nestedString(managementCluster, "metadata", "creationTimestamp"))
	if err == nil && !taken.After(created) {
		return SnapshotBeforeImport
	}
	return SnapshotRestorable
}

// SnapshotAge describes the newest etcd snapshot object of a cluster.
type SnapshotAge struct {
	// Count is the number of the cluster's etcd snapshot objects, of any status.
	Count int
	// Days is the whole days from the newest one's snapshotFile.createdAt to
	// now, 0 for a time in the future. Dated is false, and Days 0, when the
	// newest has no createdAt that parses.
	Days  int
	Dated bool
}

// NewestSnapshotAge returns the age of the newest of the etcd snapshot
// objects that belong to the cluster (SnapshotOfCluster), whatever their
// status, as the Rancher UI counts them before it rotates the encryption keys.
func NewestSnapshotAge(items []map[string]any, clusterID string, now time.Time) SnapshotAge {
	var own []map[string]any
	for _, item := range items {
		if SnapshotOfCluster(item, clusterID) {
			own = append(own, item)
		}
	}
	age := SnapshotAge{Count: len(own)}
	if len(own) == 0 {
		return age
	}
	SortSnapshotsNewestFirst(own)
	taken, ok := snapshotTime(own[0])
	if !ok {
		return age
	}
	age.Dated = true
	age.Days = max(0, int(now.Sub(taken)/(24*time.Hour)))
	return age
}
