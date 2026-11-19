package provisioning

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// rkeControlPlaneGVR is the RKEControlPlane Rancher creates beside a
// provisioning Cluster: the object whose status says which operation Rancher
// runs. It has no kind row.
var rkeControlPlaneGVR = schema.GroupVersionResource{Group: "rke.cattle.io", Version: "v1", Resource: "rkecontrolplanes"}

// fetchControlPlane reads the RKEControlPlane of the provisioning Cluster ns/name
// (it has the cluster's namespace and name). It returns the object or the
// read's error unchanged: each command decides what a missing object, a
// refused read or another failure means.
func fetchControlPlane(ctx context.Context, client engine.ClusterClient, ns, name string) (*unstructured.Unstructured, error) {
	return client.Get(ctx, rkeControlPlaneGVR, ns, name)
}
