package provisioning

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// importedDay2Rows are the keys of the kind rows for the objects through which
// Rancher takes etcd snapshots, restores and rotations of an imported RKE2 or
// K3s cluster.
var importedDay2Rows = []string{"etcdsnapshotsave", "etcdsnapshotrestore", "encryptionkeyrotation", "certificaterotation"}

// kindRegistered reports whether the kind table has a row with the key.
func kindRegistered(key string) bool {
	_, ok := kinds.ByKey(key)
	return ok
}

// unlinkedClusterError is the refusal for a provisioning Cluster whose
// status.clusterName is still empty: Rancher creates the management Cluster
// and links it by itself.
func unlinkedClusterError(name string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("Rancher has not linked cluster %s to its management cluster yet; try again later", name)).WithRetryable(true)
}

// classifyNoRKEConfig returns the refusal for a provisioning Cluster with no
// spec.rkeConfig, from its management Cluster: the local cluster, an imported
// RKE2 or K3s cluster, a cluster of another driver, or a cluster whose driver
// Rancher has not detected yet. Every outcome is an error: Rancher takes etcd
// snapshots and rotations of a provisioned cluster only through
// spec.rkeConfig. hasKind says whether the kind table has a row for a key; the
// message for an imported cluster names the kinds only when the kind table has
// a row for each of them.
func classifyNoRKEConfig(name, id string, mgmt map[string]any, hasKind func(key string) bool) *clierr.Error {
	return classifyNoRKEConfigFor(clusterops.OperationSnapshot, name, id, mgmt, hasKind)
}

// classifyNoRKEConfigFor is classifyNoRKEConfig for the operation kind: the
// texts for the local cluster and for another driver name the operation, the
// etcd snapshot restore or, for any other kind, the etcd snapshot.
func classifyNoRKEConfigFor(kind clusterops.OperationKind, name, id string, mgmt map[string]any, hasKind func(key string) bool) *clierr.Error {
	if id == engine.LocalCluster {
		if kind == clusterops.OperationRestore {
			return clierr.New(clierr.CodeValidation, "this CLI does not restore etcd snapshots of the local cluster")
		}
		return clierr.New(clierr.CodeValidation, "this CLI does not take etcd snapshots of the local cluster")
	}
	if _, imported := clusterops.ImportedDistribution(mgmt, true); imported {
		msg := fmt.Sprintf("cluster %s is an imported cluster; Rancher takes its etcd snapshots, restores and rotations through objects", name)
		known := true
		for _, key := range importedDay2Rows {
			known = known && hasKind(key)
		}
		if known {
			msg += " of the kinds ETCDSnapshotSave, ETCDSnapshotRestore, EncryptionKeyRotation and CertificateRotation"
		} else {
			msg += " this CLI does not create yet"
		}
		return clierr.New(clierr.CodeValidation, msg)
	}
	driver, _, _ := unstructured.NestedString(mgmt, "status", "driver")
	if driver == "" {
		return clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("Rancher has not detected the distribution of cluster %s yet; try again later", name)).WithRetryable(true)
	}
	if kind == clusterops.OperationRestore {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("Rancher restores etcd snapshots only on RKE2 and K3s clusters; cluster %s has driver %s", name, driver))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("Rancher takes etcd snapshots only for RKE2 and K3s clusters; cluster %s has driver %s", name, driver))
}
