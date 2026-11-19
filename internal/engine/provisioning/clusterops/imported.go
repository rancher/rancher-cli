package clusterops

const (
	// distroRKE2 and distroK3s name the two distributions Rancher manages
	// the Kubernetes version of.
	distroRKE2 = "rke2"
	distroK3s  = "k3s"

	// localCluster is the id of the local cluster's management Cluster.
	localCluster = "local"

	// providerLabel is the label Rancher puts on a management Cluster to
	// name its Kubernetes distribution.
	providerLabel = "provider.cattle.io"
)

// ImportedDistribution returns the distribution of an imported RKE2 or K3s
// management Cluster. It is rke2 or k3s from status.driver; only when the
// driver is neither, and byLabel is true, it is rke2 or k3s from the label
// provider.cattle.io. The driver takes precedence over a label that names the
// other distribution. The local cluster and any other cluster return ok false.
func ImportedDistribution(managementCluster map[string]any, byLabel bool) (distro string, ok bool) {
	if nestedString(managementCluster, "metadata", "name") == localCluster {
		return "", false
	}
	switch driver := nestedString(managementCluster, "status", "driver"); driver {
	case distroRKE2, distroK3s:
		return driver, true
	}
	if !byLabel {
		return "", false
	}
	switch label := nestedString(managementCluster, "metadata", "labels", providerLabel); label {
	case distroRKE2, distroK3s:
		return label, true
	}
	return "", false
}
