package fidelity

import "net/http"

// serverTableProbes records the server-side Table the CLI asks for when it
// prints a built-in kind as text: kubectl's Accept value sent through
// Rancher's cluster proxy returns a meta.k8s.io/v1 Table whose column
// definitions (name, type, format, priority) the CLI prints. The recorded
// content type keeps no media-type parameters, so the body's
// `kind: Table` and `apiVersion: meta.k8s.io/v1` are what prove a Table
// answer and are never dropped. Rows and the
// column descriptions are dropped: rows carry per-boot names, ages and
// addresses, and descriptions are long upstream prose that changes with
// the Kubernetes version without changing the table.
func serverTableProbes() []Probe {
	const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"
	headers := map[string]string{"Accept": tableAccept}
	rules := []NormalizeRule{
		{Path: "rows"},
		{Path: "columnDefinitions.description"},
	}
	return []Probe{
		{
			Name:       "server-table-downstream/nodes-and-pods",
			Downstream: true,
			Observations: []Observation{
				{Name: "nodes", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/api/v1/nodes", Headers: headers, Identity: IdentityAdmin}},
				{Name: "pods", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/{downstream}/api/v1/pods", Headers: headers, Identity: IdentityAdmin}},
			},
			Normalize: rules,
		},
		{
			Name:       "server-table/local-namespaces",
			OracleOnly: true,
			Observations: []Observation{
				{Name: "namespaces", Request: RequestSpec{Method: http.MethodGet, Path: "/k8s/clusters/local/api/v1/namespaces", Headers: headers, Identity: IdentityAdmin}},
			},
			Normalize: rules,
		},
	}
}
