package fidelity

import "net/http"

// steveNamespacesPath is the namespace collection of the Steve API of the
// downstream cluster: the list `get namespaces` reads after the Kubernetes API
// refuses the list of every namespace.
const steveNamespacesPath = "/k8s/clusters/{downstream}/v1/namespaces"

// namespaceProbes records the Steve namespace list and one Steve namespace of
// the downstream cluster, read as the project member of its Default project:
// the name, the labels (the label kubernetes.io/metadata.name shows that a
// namespace is returned in full), the project annotation, status.phase, and
// whether metadata.creationTimestamp and metadata.fields are present, without
// their values. The project's name is generated on each boot: the probe reads
// the member's project first and records it as {project}. A namespace Steve
// returns with a few fields only is not recorded.
func namespaceProbes() []Probe {
	const steveProjects = "/v1/management.cattle.io.projects"
	// These rules replace the envelope and the fields that differ between
	// instances or Steve versions; the id, the name, the labels, the project
	// annotation, the phase and the presence of the creation time and of the
	// Steve fields stay. The rules for an item in a collection and for one item
	// returned alone are both listed: a rule whose path is not in the body
	// changes nothing.
	rules := []NormalizeRule{
		{Path: "links", Replace: ""},
		{Path: "actions", Replace: ""},
		{Path: "pagination", Replace: ""},
		{Path: "sort", Replace: ""},
		{Path: "filters", Replace: ""},
		{Path: "createTypes", Replace: ""},
		{Path: "resourceType", Replace: ""},
		{Path: "revision", Replace: "<revision>"},
		{Path: "_id", Replace: ""},
		{Path: "data._id", Replace: ""},
		{Path: "data.links", Replace: ""},
		{Path: "data.actions", Replace: ""},
		{Path: "data.spec", Replace: "<spec>"},
		{Path: "spec", Replace: "<spec>"},
	}
	for _, prefix := range []string{"", "data."} {
		rules = append(rules,
			NormalizeRule{Path: prefix + "metadata.creationTimestamp", Replace: "<ts>"},
			NormalizeRule{Path: prefix + "metadata.fields", Replace: "<fields>"},
			NormalizeRule{Path: prefix + "metadata.uid", Replace: "<uid>"},
			NormalizeRule{Path: prefix + "metadata.resourceVersion", Replace: "<rv>"},
			NormalizeRule{Path: prefix + "metadata.annotations", KeepKeys: []string{"field.cattle.io/projectId"}},
			NormalizeRule{Path: prefix + "metadata.managedFields", Replace: ""},
			NormalizeRule{Path: prefix + "metadata.relationships", Replace: ""},
			NormalizeRule{Path: prefix + "metadata.state", Replace: ""},
			NormalizeRule{Path: prefix + "metadata.generation", Replace: ""},
		)
	}
	return []Probe{
		{
			Name:               "namespaces-downstream/member-steve-namespaces",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				// The member's project, for its generated name: only the id, the
				// count and the envelope stay.
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects, Identity: IdentityMember},
					Captures: map[string]Capture{"project": {Path: "data.0.metadata.name"}},
					Normalize: []NormalizeRule{
						{Path: "data.metadata", Replace: ""},
						{Path: "data.status", Replace: ""},
					}},
				{Name: "list", Request: RequestSpec{Method: http.MethodGet, Path: steveNamespacesPath, Identity: IdentityMember}},
				{Name: "get-own-namespace", Request: RequestSpec{Method: http.MethodGet, Path: steveNamespacesPath + "/default", Identity: IdentityMember}},
			},
			Normalize: rules,
		},
	}
}
