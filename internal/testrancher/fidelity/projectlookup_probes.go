package fidelity

import "net/http"

// projectLookupProbes records what the --project lookup reads: the steve
// project collection of one cluster, for a member (exactly its project,
// steve id <cluster>/<project>) and for an admin (every project on the
// cluster; an unknown cluster is an empty collection with no count), and
// the member's GET of a project by id through the local cluster, the
// lookup's answer when steve's cache has not listed a project yet.
func projectLookupProbes() []Probe {
	const (
		steveProjects = "/v1/management.cattle.io.projects"
		mgmtProjects  = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/{downstream}/projects"
		localProjects = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/local/projects"
	)
	projectRules := []NormalizeRule{
		{Path: "links", Replace: ""},
		{Path: "actions", Replace: ""},
		{Path: "pagination", Replace: ""},
		{Path: "sort", Replace: ""},
		{Path: "filters", Replace: ""},
		{Path: "createTypes", Replace: ""},
		{Path: "resourceType", Replace: ""},
		{Path: "revision", Replace: "<revision>"},
		{Path: "data._id", Replace: ""},
		{Path: "data.links", Replace: ""},
		{Path: "data.actions", Replace: ""},
		{Path: "data.status", Replace: ""},
		{Path: "data.metadata.annotations", Replace: ""},
		{Path: "data.metadata.labels", Replace: ""},
		{Path: "data.metadata.managedFields", Replace: ""},
		{Path: "data.metadata.fields", Replace: ""},
		{Path: "data.metadata.finalizers", Replace: ""},
		{Path: "data.metadata.generateName", Replace: ""},
		{Path: "data.metadata.creationTimestamp", Replace: ""},
		{Path: "data.metadata.resourceVersion", Replace: ""},
		{Path: "data.metadata.uid", Replace: ""},
		{Path: "data.metadata.relationships", Replace: ""},
		{Path: "data.metadata.state", Replace: ""},
		{Path: "data.metadata.generation", Replace: ""},
		{Path: "data.spec.resourceQuota", Replace: ""},
		{Path: "data.spec.namespaceDefaultResourceQuota", Replace: ""},
		{Path: "data.spec.containerDefaultResourceLimit", Replace: ""},
		{Path: "data.spec.description", Replace: ""},
		// The kube GET of one project and the refusal of another.
		{Path: "metadata.annotations", Replace: ""},
		{Path: "metadata.labels", Replace: ""},
		{Path: "metadata.managedFields", Replace: ""},
		{Path: "metadata.finalizers", Replace: ""},
		{Path: "metadata.generateName", Replace: ""},
		{Path: "metadata.creationTimestamp", Replace: ""},
		{Path: "metadata.resourceVersion", Replace: ""},
		{Path: "metadata.uid", Replace: ""},
		{Path: "metadata.generation", Replace: ""},
		{Path: "spec.resourceQuota", Replace: ""},
		{Path: "spec.namespaceDefaultResourceQuota", Replace: ""},
		{Path: "spec.containerDefaultResourceLimit", Replace: ""},
		{Path: "spec.description", Replace: ""},
		{Path: "status", Replace: ""},
		{Path: "message", Replace: "<message>"},
		{Path: "details", Replace: ""},
	}
	// An admin's ids are generated per cluster; the display names and the
	// cluster stay, sorted by display name.
	adminRules := append([]NormalizeRule{
		// A project another probe created, still being deleted, is not
		// recorded.
		probeFixtures("metadata.name", "p-"+fixturePrefix),
		{Path: "data.id", Replace: "<id>"},
		{Path: "data.metadata.name", Replace: "<name>"},
		{Path: "data", SortBy: "spec.displayName"},
	}, projectRules...)
	return []Probe{
		{
			Name:               "access-downstream/member-steve-projects-by-cluster",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects + "/{downstream}", Identity: IdentityMember},
					Captures: map[string]Capture{"project": {Path: "data.0.metadata.name"}}},
				{Name: "get-own-project", Request: RequestSpec{Method: http.MethodGet, Path: mgmtProjects + "/{project}", Identity: IdentityMember}},
				{Name: "get-missing-project", Request: RequestSpec{Method: http.MethodGet, Path: mgmtProjects + "/p-nosuchproject", Identity: IdentityMember}},
			},
			Normalize: projectRules,
		},
		{
			Name:               "access-downstream/admin-steve-projects-by-cluster",
			Downstream:         true,
			SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects + "/{downstream}", Identity: IdentityAdmin}},
				{Name: "unknown-cluster", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects + "/c-nosuchcluster", Identity: IdentityAdmin}},
			},
			Normalize: adminRules,
		},
		{
			// The same reads against the local cluster, which the composite
			// facade also serves: recorded on the oracle and compared with
			// the facade by the conformance test, so the facade's
			// per-cluster collection, its empty collection and its GET by id
			// are checked in make e2e. Ids are generated per boot; the
			// display names stay.
			Name:               "project-lookup/admin-local-projects",
			SubstituteCaptures: true,
			Observations: []Observation{
				{Name: "projects", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects + "/local", Identity: IdentityAdmin},
					Captures: map[string]Capture{"project": {Path: "data.0.metadata.name"}}},
				{Name: "get-project", Request: RequestSpec{Method: http.MethodGet, Path: localProjects + "/{project}", Identity: IdentityAdmin}},
				{Name: "get-missing-project", Request: RequestSpec{Method: http.MethodGet, Path: localProjects + "/p-nosuchproject", Identity: IdentityAdmin}},
				{Name: "unknown-cluster", Request: RequestSpec{Method: http.MethodGet, Path: steveProjects + "/c-nosuchcluster", Identity: IdentityAdmin}},
			},
			Normalize: append([]NormalizeRule{
				{Path: "data.id", Replace: "<id>"},
				{Path: "data.metadata.name", Replace: "<name>"},
				{Path: "data", SortBy: "spec.displayName"},
				// Which project data.0 was is boot order, so the one read
				// by id keeps no display name.
				{Path: "spec.displayName", Replace: "<display-name>"},
			}, projectRules...),
		},
	}
}
