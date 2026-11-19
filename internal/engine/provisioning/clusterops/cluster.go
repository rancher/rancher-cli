package clusterops

// HasRKEConfig reports whether a provisioning.cattle.io Cluster has
// spec.rkeConfig: present and not null. An empty object counts, because a
// custom cluster carries spec.rkeConfig: {} and Rancher manages it as a
// provisioned cluster; only an absent or null rkeConfig marks an imported
// (imported) cluster.
func HasRKEConfig(cluster map[string]any) bool {
	v, found := nested(cluster, "spec", "rkeConfig")
	return found && v != nil
}
