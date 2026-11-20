// Package clusterops has the pure checks of operations on a Rancher
// cluster that a command and its completion both call: which operation
// Rancher is running on a provisioned cluster, which clusters are imported
// RKE2 or K3s clusters, which etcd snapshots belong to a cluster, and the rules
// of the day-2 operations of an imported cluster: the restore modes the Rancher
// UI offers, the certificate services of a distribution, and the age of the
// newest etcd snapshot. Its functions read objects as decoded JSON
// (map[string]any) and make no request.
//
// The package imports the standard library packages it needs and
// internal/clierr, and nothing else: a completion package may import it
// because it can never reach a client through it. A test in test/conformance
// asserts the import list.
package clusterops
