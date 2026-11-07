package fidelity

import "net/http"

// etcdSnapshotProbes records what the CLI's etcd-snapshot row reads: the
// rke.cattle.io/v1 discovery document of the local cluster, the admin list of
// etcdsnapshots in the namespace fleet-default and the server's Table (its
// columns are only Name and Created At, because the CRD declares no printer
// columns), the CRD itself, and the response a standard user gets. The probes
// only read: they create no fixture.
//
// On the oracle the admin list is empty: the oracle has no RKE2 provisioning
// cluster (its downstream is an imported k3s with no etcd snapshots), so the
// golden records the kind, the API version, the status and the Table's column
// definitions, not a snapshot. The probes cannot record the field paths of an
// object as served (snapshotFile.* and spec.clusterName) or the annotation
// etcdsnapshot.rke.io/storage, because no snapshot exists. The CRD read shows
// that the schema the server serves keeps the fields the row reads. The values
// of those fields are known only from Rancher's Go type definition and
// controller source and from the CRD of the e2e scenario, not from a recorded
// response.
//
// The reads that depend on the CRD being served wait until the server responds
// with 200: the discovery read, the admin list, the CRD read and the admin list
// that comes first in the standard user's probe. This stops the probe from
// recording the 404 of a CRD the server does not serve yet. If no 200 arrives
// within the executor's status timeout, the probe fails; a drift run keeps the
// last response instead. The standard user's request does not wait; it records
// whatever status the server returns.
//
//   - etcdsnapshot/discovery-local: the rke.cattle.io/v1 group-version
//     document of the local cluster, without the storage version hashes. It
//     lists every resource of the group.
//   - etcdsnapshot/list-local: the admin list and Table of etcdsnapshots in
//     fleet-default, with items and metadata replaced and the Table's rows and
//     column descriptions dropped.
//   - etcdsnapshot/crd-local: the etcdsnapshots.rke.cattle.io CRD, reduced to
//     the group, scope, version names and the schema paths the row reads.
//   - etcdsnapshot/standard-user-local: an admin list that waits for the CRD
//     to be served, then the standard user's request for the list that
//     etcdsnapshot/list-local reads as admin.
func etcdSnapshotProbes() []Probe {
	const (
		tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"
		local       = "/k8s/clusters/local/apis/rke.cattle.io/v1"
		listPath    = local + "/namespaces/fleet-default/etcdsnapshots"
		crdPath     = "/k8s/clusters/local/apis/apiextensions.k8s.io/v1/customresourcedefinitions/etcdsnapshots.rke.cattle.io"
		props       = "spec.versions.schema.openAPIV3Schema.properties"
	)
	return []Probe{
		{
			Name:       "etcdsnapshot/discovery-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "group-version", Request: RequestSpec{Method: http.MethodGet, Path: local, Identity: IdentityAdmin}}),
			},
			// The server lists the resources in no fixed order.
			Normalize: []NormalizeRule{
				{Path: "resources.storageVersionHash"},
				{Path: "resources", SortBy: "name"},
			},
		},
		{
			Name:       "etcdsnapshot/list-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "list", Request: RequestSpec{Method: http.MethodGet, Path: listPath, Identity: IdentityAdmin}}),
				{Name: "table", Request: RequestSpec{Method: http.MethodGet, Path: listPath, Headers: map[string]string{"Accept": tableAccept}, Identity: IdentityAdmin}},
			},
			Normalize: []NormalizeRule{
				{Path: "items", Replace: "<scrubbed>"},
				{Path: "metadata", Replace: "<scrubbed>"},
				{Path: "rows"},
				{Path: "columnDefinitions.description"},
			},
		},
		{
			// KeepKeys cannot address one element of an array, so the version
			// element's served, storage and subresources fields are dropped
			// and any other key Rancher adds there, for example
			// additionalPrinterColumns, stays in the golden. Below
			// spec.versions.schema the rules keep only the listed keys, so a
			// key Rancher adds there does not show in the golden. The golden
			// shows the type and format of each field the row reads (size:
			// integer, int64; createdAt: string, date-time).
			Name:       "etcdsnapshot/crd-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "crd", Request: RequestSpec{Method: http.MethodGet, Path: crdPath, Identity: IdentityAdmin}}),
			},
			Normalize: []NormalizeRule{
				{Path: "metadata", Replace: "<scrubbed>"},
				{Path: "status"},
				{Path: "spec", KeepKeys: []string{"group", "scope", "versions"}},
				{Path: "spec.versions.served"},
				{Path: "spec.versions.storage"},
				{Path: "spec.versions.subresources"},
				{Path: "spec.versions.schema", KeepKeys: []string{"openAPIV3Schema"}},
				{Path: "spec.versions.schema.openAPIV3Schema", KeepKeys: []string{"properties"}},
				{Path: props, KeepKeys: []string{"spec", "snapshotFile"}},
				{Path: props + ".spec", KeepKeys: []string{"properties"}},
				{Path: props + ".spec.properties", KeepKeys: []string{"clusterName"}},
				{Path: props + ".spec.properties.*", KeepKeys: []string{"type", "format"}},
				{Path: props + ".snapshotFile", KeepKeys: []string{"properties"}},
				{Path: props + ".snapshotFile.properties", KeepKeys: []string{"name", "nodeName", "location", "createdAt", "size", "status", "message"}},
				{Path: props + ".snapshotFile.properties.*", KeepKeys: []string{"type", "format"}},
			},
		},
		{
			Name:       "etcdsnapshot/standard-user-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "admin-list-ready", Request: RequestSpec{Method: http.MethodGet, Path: listPath, Identity: IdentityAdmin}}),
				{Name: "standard-list", Request: RequestSpec{Method: http.MethodGet, Path: listPath, Identity: IdentityStandard}},
			},
			Normalize: []NormalizeRule{
				{Path: "items", Replace: "<scrubbed>"},
				{Path: "metadata", Replace: "<scrubbed>"},
				{Path: "message", Replace: "<scrubbed>"},
			},
		},
	}
}
