package provisioning

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// IsImported reports whether a provisioning.cattle.io Cluster object is an
// imported cluster: spec.rkeConfig absent or null. nil, not {}, is the
// discriminator (rancher cluster/controller.go NormalizeCluster treats a
// nil RKEConfig as "not managed by RKE"); a custom cluster carries
// spec.rkeConfig: {} and a node-driver cluster a populated one. The create
// path (createcluster.go) writes these shapes; every reader-side test of
// "is this imported" goes through here.
func IsImported(prov *unstructured.Unstructured) bool {
	v, found, _ := unstructured.NestedFieldNoCopy(prov.Object, "spec", "rkeConfig")
	return !found || v == nil
}
