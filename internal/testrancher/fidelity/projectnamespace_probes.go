package fidelity

import "net/http"

// projectNamespaceProbes records how Rancher maps a downstream namespace to
// its project and what a project delete does to the project's namespaces,
// the facts delete project lists before it acts. A namespace names its
// project in the field.cattle.io/projectId annotation
// ("<cluster>:<project>"), and Rancher writes the field.cattle.io/projectId
// label ("<project>") from it. When the project is deleted, Rancher deletes
// each namespace that carries a field.cattle.io/creatorId annotation and
// keeps every other one: a kept namespace loses the project annotation and
// still carries the project label.
//
// The fixtures are a project created through the management cluster's
// Kubernetes API and two namespaces created on the downstream with the
// project annotation, one of them also with a creatorId annotation. The
// second probe deletes the project in its setup and waits for Rancher to
// finish with both namespaces, because an observation cannot wait for a
// field to change; its observations read the end state by name.
func projectNamespaceProbes() []Probe {
	const (
		mgmtProjects   = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/{downstream}/projects/"
		localNS        = "/k8s/clusters/local/api/v1/namespaces/"
		downstreamNS   = "/k8s/clusters/{downstream}/api/v1/namespaces/"
		projectLabel   = "field.cattle.io/projectId"
		projectAnno    = "field.cattle.io/projectId"
		creatorAnno    = "field.cattle.io/creatorId"
		creatorValue   = "e2e-fid-creator"
		mappedProject  = "p-e2e-fid-nsmap"
		mappedCreated  = "e2e-fid-nsmap-created"
		mappedKept     = "e2e-fid-nsmap-kept"
		deletedProject = "p-e2e-fid-nsdel"
		deletedCreated = "e2e-fid-nsdel-created"
		deletedKept    = "e2e-fid-nsdel-kept"
	)
	projectBody := func(name string) string {
		return `{"apiVersion":"management.cattle.io/v3","kind":"Project","metadata":{"name":"` + name + `","namespace":"{downstream}"},"spec":{"clusterName":"{downstream}","displayName":"` + name + `"}}`
	}
	namespaceBody := func(name, project string, withCreator bool) string {
		annotations := `"` + projectAnno + `":"{downstream}:` + project + `"`
		if withCreator {
			annotations += `,"` + creatorAnno + `":"` + creatorValue + `"`
		}
		return `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"` + name + `","annotations":{` + annotations + `}}}`
	}
	gone := func(_ map[string]any, status int) bool { return status == http.StatusNotFound }
	present := func(_ map[string]any, status int) bool { return status == http.StatusOK }
	labeled := func(project string) func(map[string]any, int) bool {
		return func(decoded map[string]any, status int) bool {
			md, _ := decoded["metadata"].(map[string]any)
			labels, _ := md["labels"].(map[string]any)
			return status == http.StatusOK && labels[projectLabel] == project
		}
	}
	annotationRemoved := func(decoded map[string]any, status int) bool {
		md, _ := decoded["metadata"].(map[string]any)
		annotations, _ := md["annotations"].(map[string]any)
		_, has := annotations[projectAnno]
		return status == http.StatusOK && !has
	}
	// setup removes leftovers of an earlier run on a kept pair and waits
	// until they are gone (Rancher's project webhook refuses a project
	// whose backing namespace still exists), then creates the project and
	// its two namespaces and waits for Rancher to label both.
	setup := func(project, created, kept string) []Step {
		return []Step{
			deleteFixture("stale namespace "+created, downstreamNS+created),
			deleteFixture("stale namespace "+kept, downstreamNS+kept),
			deleteFixture("stale project "+project, mgmtProjects+project),
			waitFixture("stale namespace "+created+" gone", http.MethodGet, downstreamNS+created, "", gone),
			waitFixture("stale namespace "+kept+" gone", http.MethodGet, downstreamNS+kept, "", gone),
			waitFixture("stale project "+project+" gone", http.MethodGet, mgmtProjects+project, "", gone),
			waitFixture("stale backing namespace gone", http.MethodGet, localNS+"{downstream}-"+project, "", gone),
			createFixture("project "+project, mgmtProjects, projectBody(project)),
			waitFixture("backing namespace", http.MethodGet, localNS+"{downstream}-"+project, "", present),
			createFixture("namespace "+created, downstreamNS, namespaceBody(created, project, true)),
			createFixture("namespace "+kept, downstreamNS, namespaceBody(kept, project, false)),
			waitFixture("namespace "+created+" labeled", http.MethodGet, downstreamNS+created, "", labeled(project)),
			waitFixture("namespace "+kept+" labeled", http.MethodGet, downstreamNS+kept, "", labeled(project)),
		}
	}
	teardown := func(project, created, kept string) []Step {
		return []Step{
			deleteFixture("namespace "+created, downstreamNS+created),
			deleteFixture("namespace "+kept, downstreamNS+kept),
			deleteFixture("project "+project, mgmtProjects+project),
		}
	}
	// Each namespace keeps only the project label and the two annotations
	// the outcome depends on; Rancher's other annotations carry timestamps
	// and controller state.
	rules := []NormalizeRule{
		{Path: "metadata.labels", KeepKeys: []string{projectLabel}},
		{Path: "metadata.annotations", KeepKeys: []string{projectAnno, creatorAnno}},
	}
	deleteSetup := append(setup(deletedProject, deletedCreated, deletedKept),
		deleteFixture("project "+deletedProject, mgmtProjects+deletedProject),
		waitFixture("namespace "+deletedCreated+" deleted", http.MethodGet, downstreamNS+deletedCreated, "", gone),
		waitFixture("namespace "+deletedKept+" annotation removed", http.MethodGet, downstreamNS+deletedKept, "", annotationRemoved),
		waitFixture("project "+deletedProject+" gone", http.MethodGet, mgmtProjects+deletedProject, "", gone),
	)
	return []Probe{
		{
			Name:               "project-namespaces-downstream/annotation-and-label",
			Downstream:         true,
			SubstituteCaptures: true,
			Setup:              setup(mappedProject, mappedCreated, mappedKept),
			Observations: []Observation{
				{Name: "namespace-with-creator", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNS + mappedCreated, Identity: IdentityAdmin}},
				{Name: "namespace-without-creator", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNS + mappedKept, Identity: IdentityAdmin}},
			},
			Normalize: rules,
			Teardown:  teardown(mappedProject, mappedCreated, mappedKept),
		},
		{
			Name:               "project-namespaces-downstream/after-project-delete",
			Downstream:         true,
			SubstituteCaptures: true,
			Setup:              deleteSetup,
			Observations: []Observation{
				{Name: "namespace-with-creator", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNS + deletedCreated, Identity: IdentityAdmin}},
				{Name: "namespace-without-creator", Request: RequestSpec{Method: http.MethodGet, Path: downstreamNS + deletedKept, Identity: IdentityAdmin}},
				{Name: "project", Request: RequestSpec{Method: http.MethodGet, Path: mgmtProjects + deletedProject, Identity: IdentityAdmin}},
			},
			Normalize: rules,
			Teardown:  teardown(deletedProject, deletedCreated, deletedKept),
		},
	}
}
