package fidelity

import "net/http"

// catalogProbes records what the CLI's clusterrepo, app and operation rows
// read: the catalog.cattle.io/v1 discovery document a cluster serves, the
// admin list of each kind and the server's Table (the column names and
// types the CLI's own columns follow), the responses a standard user gets, and
// the shape of one installed app. Every cluster serves its own catalog
// objects, so the downstream cluster and the local cluster are recorded
// separately. The probes only read: they create no fixture. The first read
// of each probe waits until the server responds with 200; this stops the probe
// from recording the 404 of a catalog the server does not serve yet. If no 200
// arrives within the executor's status timeout, the probe fails; a drift run
// keeps the last response instead.
//
// Lists hold names, versions, URLs and timestamps that differ between
// instances. The recorded list keeps its kind and API version, replaces its
// items and metadata with "<scrubbed>" and drops the Table's rows and column
// descriptions. Items are replaced whole, because recording the keys of each
// item would make the golden depend on the number of repositories and apps the
// instance has. Operation items are never
// recorded, and the probes cannot make the operation list non-empty without
// a write.
//
//   - catalog/discovery-downstream: the group-version document of the
//     downstream cluster, without the storage version hashes.
//   - catalog/clusterrepos-downstream, catalog/apps-downstream and
//     catalog/operations-downstream: the admin list and Table of the
//     downstream cluster.
//   - catalog/clusterrepos-local: the same for the local cluster's path.
//   - catalog/standard-user-local: the standard user's list of repositories
//     (200) and of apps (an error status), each with its items, metadata and
//     message replaced.
//   - catalog/webhook-app-local: the rancher-webhook app the oracle installs,
//     recorded with only the keys its KeepKeys rules list under spec,
//     spec.chart and spec.chart.metadata.
func catalogProbes() []Probe {
	const (
		tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"
		downstream  = "/k8s/clusters/{downstream}/apis/catalog.cattle.io/v1"
		local       = "/k8s/clusters/local/apis/catalog.cattle.io/v1"
	)
	tableHeaders := map[string]string{"Accept": tableAccept}
	listRules := []NormalizeRule{
		{Path: "items", Replace: "<scrubbed>"},
		{Path: "metadata", Replace: "<scrubbed>"},
		{Path: "rows"},
		{Path: "columnDefinitions.description"},
	}
	listAndTable := func(base, resource string) []Observation {
		return []Observation{
			waitingFor(http.StatusOK, Observation{Name: "list", Request: RequestSpec{Method: http.MethodGet, Path: base + "/" + resource, Identity: IdentityAdmin}}),
			{Name: "table", Request: RequestSpec{Method: http.MethodGet, Path: base + "/" + resource, Headers: tableHeaders, Identity: IdentityAdmin}},
		}
	}
	return []Probe{
		{
			Name:       "catalog/discovery-downstream",
			Downstream: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "group-version", Request: RequestSpec{Method: http.MethodGet, Path: downstream, Identity: IdentityAdmin}}),
			},
			// The server lists the resources in no fixed order.
			Normalize: []NormalizeRule{
				{Path: "resources.storageVersionHash"},
				{Path: "resources", SortBy: "name"},
			},
		},
		{
			Name:         "catalog/clusterrepos-downstream",
			Downstream:   true,
			Observations: listAndTable(downstream, "clusterrepos"),
			Normalize:    listRules,
		},
		{
			Name:         "catalog/apps-downstream",
			Downstream:   true,
			Observations: listAndTable(downstream, "apps"),
			Normalize:    listRules,
		},
		{
			Name:         "catalog/operations-downstream",
			Downstream:   true,
			Observations: listAndTable(downstream, "operations"),
			Normalize:    listRules,
		},
		{
			// Covers the local cluster's path; the downstream probe above
			// covers the downstream cluster's.
			Name:         "catalog/clusterrepos-local",
			OracleOnly:   true,
			Observations: listAndTable(local, "clusterrepos"),
			Normalize:    listRules,
		},
		{
			Name:       "catalog/standard-user-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "standard-clusterrepos", Request: RequestSpec{Method: http.MethodGet, Path: local + "/clusterrepos", Identity: IdentityStandard}}),
				{Name: "standard-apps", Request: RequestSpec{Method: http.MethodGet, Path: local + "/apps", Identity: IdentityStandard}},
			},
			Normalize: []NormalizeRule{
				{Path: "items", Replace: "<scrubbed>"},
				{Path: "metadata", Replace: "<scrubbed>"},
				{Path: "message", Replace: "<scrubbed>"},
			},
		},
		{
			// The app Rancher installs on every cluster. KeepKeys limits the
			// keys recorded under spec, spec.chart and spec.chart.metadata to
			// the ones listed (the ones the CLI reads or hides, plus
			// helmVersion, namespace and resources), and every value under
			// them is replaced. The golden therefore asserts that
			// spec.chart.metadata.name and .version, spec.name, spec.version
			// and spec.info.status are served at those paths. A key the
			// server adds outside the lists is dropped; a listed key it stops
			// serving disappears from the response and fails verification.
			// The values the CLI hides (spec.values, spec.chart.values,
			// spec.info.notes) may be absent on this app; the tests of the cli
			// package assert that they are hidden, not this golden. Every
			// value is replaced by the same placeholder, which hides its JSON
			// type.
			Name:       "catalog/webhook-app-local",
			OracleOnly: true,
			Observations: []Observation{
				waitingFor(http.StatusOK, Observation{Name: "admin-webhook-app", Request: RequestSpec{Method: http.MethodGet, Path: local + "/namespaces/cattle-system/apps/rancher-webhook", Identity: IdentityAdmin}}),
			},
			Normalize: []NormalizeRule{
				{Path: "metadata", Replace: "<scrubbed>"},
				{Path: "spec.chart", KeepKeys: []string{"metadata", "values"}},
				{Path: "spec.chart.metadata", KeepKeys: []string{"name", "version"}},
				{Path: "spec.chart.metadata.*", Replace: "<value>"},
				{Path: "spec.chart.values", Replace: "<value>"},
				{Path: "spec.info.*", Replace: "<value>"},
				{Path: "spec", KeepKeys: []string{"chart", "info", "name", "version", "helmVersion", "namespace", "resources", "values"}},
				{Path: "spec.name", Replace: "<value>"},
				{Path: "spec.version", Replace: "<value>"},
				{Path: "spec.helmVersion", Replace: "<value>"},
				{Path: "spec.namespace", Replace: "<value>"},
				{Path: "spec.resources", Replace: "<value>"},
				{Path: "spec.values", Replace: "<value>"},
				{Path: "status.*", Replace: "<value>"},
			},
		},
	}
}
