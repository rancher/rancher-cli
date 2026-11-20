package clusterops

import (
	"github.com/rancher/rancher-cli/internal/clierr"
)

// StrategyCluster has the facts about a cluster that the upgrade strategy
// command needs, read from its objects by CheckStrategyCluster.
type StrategyCluster struct {
	// Name is the name of the provisioning Cluster; ID is the id of its
	// management Cluster, empty when the cluster is provisioned.
	Name string
	ID   string
	// Provisioned is true when Rancher provisions the cluster (it has
	// spec.rkeConfig), false for an imported one.
	Provisioned bool
	// Distro is the driver of an imported cluster, "rke2" or "k3s"; empty for a
	// provisioned cluster.
	Distro string
	// AddAnnotation is true when the write also sets the version management
	// annotation to system-default: an imported cluster whose annotation is
	// absent or empty.
	AddAnnotation bool
}

// CheckStrategyCluster checks from the objects of the cluster alone whether
// its upgrade strategy can be written. cluster is the provisioning Cluster,
// managementCluster its management Cluster (nil for a provisioned cluster). It
// returns the facts of the cluster, or the first refusal in this order: the
// cluster Rancher runs in; for a provisioned cluster nothing more; for an
// imported cluster the link to its management Cluster, hosted clusters, the
// driver, an annotation value Rancher refuses and a cluster that has not
// reported its version. The pause annotation, an annotation that is false and
// the version management Setting are not checked: Rancher's admission
// webhook accepts the write and the strategy applies once version management
// is on.
func CheckStrategyCluster(cluster, managementCluster map[string]any) (StrategyCluster, *clierr.Error) {
	name := nestedString(cluster, "metadata", "name")
	id := nestedString(cluster, "status", "clusterName")
	if err := refuseLocal(id); err != nil {
		return StrategyCluster{}, err
	}
	if HasRKEConfig(cluster) {
		return StrategyCluster{Name: name, Provisioned: true}, nil
	}

	driver, err := checkImportedDriver(name, id, managementCluster)
	if err != nil {
		return StrategyCluster{}, err
	}
	annotation, _ := nested(managementCluster, "metadata", "annotations", versionManagementAnnotation)
	value, _ := annotation.(string)
	if unknownAnnotationValue(value) {
		return StrategyCluster{}, unknownAnnotationError(name, value, versionManagementHint(id))
	}
	if _, err := checkImportedReported(name, driver, managementCluster); err != nil {
		return StrategyCluster{}, err
	}
	return StrategyCluster{Name: name, ID: id, Distro: driver, AddAnnotation: value == ""}, nil
}

// VersionManagement is whether Rancher manages the Kubernetes version of an
// imported cluster, which decides whether it applies the upgrade strategy.
type VersionManagement int

const (
	// VersionManagementOn is the state of a cluster whose annotation is true, or
	// leaves it to a Setting that is true.
	VersionManagementOn VersionManagement = iota
	// VersionManagementOff is the state of a cluster whose annotation is false,
	// or leaves it to a Setting that is not true.
	VersionManagementOff
	// VersionManagementUnknown is the state of a cluster that leaves it to the
	// Setting when the Setting could not be read, and of an annotation value
	// Rancher refuses.
	VersionManagementUnknown
)

// VersionManagementOf returns the version management state of an imported
// cluster from its management Cluster and the Setting as read. The Setting
// counts only when the annotation is system-default, empty or absent.
func VersionManagementOf(managementCluster map[string]any, setting SettingRead) VersionManagement {
	switch nestedString(managementCluster, "metadata", "annotations", versionManagementAnnotation) {
	case "true":
		return VersionManagementOn
	case "false":
		return VersionManagementOff
	case "", "system-default":
		switch {
		case !setting.Read:
			return VersionManagementUnknown
		case setting.Value == "true":
			return VersionManagementOn
		}
		return VersionManagementOff
	}
	return VersionManagementUnknown
}
