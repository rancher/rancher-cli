package fidelity

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The annotations app install sends in its chart entry, and the value of the
// source repository type for a ClusterRepo.
const (
	catalogRequestIDAnnotation      = "cli.rancher.io/request-id"
	catalogSourceRepoTypeAnnotation = "catalog.cattle.io/ui-source-repo-type"
	catalogSourceRepoAnnotation     = "catalog.cattle.io/ui-source-repo"
	catalogSourceRepoTypeCluster    = "cluster"
)

// What app install and app uninstall send with every action request: the
// field manager query parameter and its value, and the Accept header. The
// fidelity package does not import the CLI's client; a guard test compares
// these with the CLI's source.
const (
	catalogFieldManagerParam = "fieldManager"
	catalogFieldManager      = "cli.rancher.io"
	catalogActionAccept      = "application/json"
)

// catalogActionQuery is the query of an action request for action.
func catalogActionQuery(action string) url.Values {
	return url.Values{"action": {action}, catalogFieldManagerParam: {catalogFieldManager}}
}

// catalogActionHeaders are the headers of an action request besides the
// bearer and the Content-Type the executor sets.
func catalogActionHeaders() map[string]string {
	return map[string]string{"Accept": catalogActionAccept}
}

// catalogWriteProbes records what app install and app uninstall --wait
// decide on: the 201 answer of each action, the end state of each
// Operation and the App of the release. Both install the harvester-rbac
// chart from rancher-charts as the release harvester-rbac in the namespace
// default, with the request id and the cluster values app install sends.
//
//   - catalog/install-uninstall-local: the install on the local cluster,
//     its Operation once Helm's run has ended (Reconciling False and
//     Stalled False), the App at deployed with this request's id, the
//     uninstall, its Operation and the App's 404.
//   - catalog/failing-install-downstream: the install on the downstream,
//     where the cluster serves no RoleTemplate kind, so Helm stores no
//     release: its Operation once it is Stalled, and the App's 404.
//
// Recording: each Operation and App is recorded when its wait accepts the
// answer, the end state, never the progression to it. A golden holds no
// generated name, uid, timestamp or read count: the Operation names, the
// request id and the downstream id are written as their placeholders, the
// keys the CLI does not read are dropped, and the values are replaced.
// The probes run only on the pair scripts/e2e-fidelity booted or kept,
// and only up to Rancher 2.15, the last minor this chart version declares.
// Their setup and teardown come from catalogCleanup: a run deletes only
// the objects its own Operations created.
func catalogWriteProbes() []Probe {
	return catalogWriteProbesWith(func(*catalogCleanup) {})
}

// catalogWriteProbesWith is catalogWriteProbes with tune applied to each
// probe's cleanup before its steps are built; tests shorten the bounds.
func catalogWriteProbesWith(tune func(c *catalogCleanup)) []Probe {
	local := newCatalogCleanup("local", "installop", "uninstallop")
	tune(&local)
	downstream := newCatalogCleanup("{downstream}", "failop", "")
	tune(&downstream)
	localSetup, localTeardown := local.steps()
	downstreamSetup, downstreamTeardown := downstream.steps()
	const (
		localKube      = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/" + catalogReleaseNamespace
		downstreamKube = "/k8s/clusters/{downstream}/apis/catalog.cattle.io/v1/namespaces/" + catalogReleaseNamespace
		app            = "/apps/harvester-rbac"
	)
	return []Probe{
		{
			Name:               "catalog/install-uninstall-local",
			OracleOnly:         true,
			OwnPairOnly:        true,
			MaxMinor:           "2.15",
			SubstituteCaptures: true,
			Setup:              localSetup,
			Observations: []Observation{
				catalogInstall("/v1/catalog.cattle.io.clusterrepos/rancher-charts", "installop"),
				catalogOperation("install-operation", localKube+"/operations/{installop}", operationSucceeded),
				{
					Name:      "install-app",
					Request:   RequestSpec{Method: http.MethodGet, Path: localKube + app, Identity: IdentityAdmin},
					Until:     appDeployed,
					Normalize: appRules(),
				},
				{
					Name:      "uninstall",
					Request:   RequestSpec{Method: http.MethodPost, Path: "/v1/catalog.cattle.io.apps/" + catalogReleaseNamespace + "/harvester-rbac", Query: catalogActionQuery("uninstall"), Headers: catalogActionHeaders(), Body: `{}`, Identity: IdentityAdmin},
					Captures:  map[string]Capture{"uninstallop": {Path: "operationName"}},
					Normalize: actionAnswerRules(),
				},
				catalogOperation("uninstall-operation", localKube+"/operations/{uninstallop}", operationSucceeded),
				{
					Name:    "app-gone",
					Request: RequestSpec{Method: http.MethodGet, Path: localKube + app, Identity: IdentityAdmin},
					Until:   appGone,
				},
			},
			Teardown: localTeardown,
		},
		{
			Name:               "catalog/failing-install-downstream",
			Downstream:         true,
			OwnPairOnly:        true,
			MaxMinor:           "2.15",
			SubstituteCaptures: true,
			Setup:              downstreamSetup,
			Observations: []Observation{
				catalogInstall("/k8s/clusters/{downstream}/v1/catalog.cattle.io.clusterrepos/rancher-charts", "failop"),
				catalogOperation("install-operation", downstreamKube+"/operations/{failop}", operationStalled),
				{
					Name:    "app",
					Request: RequestSpec{Method: http.MethodGet, Path: downstreamKube + app, Identity: IdentityAdmin},
				},
			},
			Teardown: downstreamTeardown,
		},
	}
}

// catalogInstallBody is the install action's body as app install sends it
// for harvester-rbac: its defaults, the chart entry with the request id and
// the source annotations, and the cluster values, each written in place
// from its stored JSON encoding.
func catalogInstallBody() string {
	values := make([]string, len(catalogValueKeys))
	for i, key := range catalogValueKeys {
		values[i] = strconv.Quote(key) + ":{" + catalogValuePrefix + key + "}"
	}
	return `{"timeout":"10m0s","wait":true,"namespace":"` + catalogReleaseNamespace + `","charts":[{"chartName":"harvester-rbac",` +
		`"version":"110.0.0+up0.1.1","releaseName":"harvester-rbac","values":{"global":{"cattle":{` + strings.Join(values, ",") + `}}},` +
		`"annotations":{"` + catalogRequestIDAnnotation + `":"{` + requestIDCapture + `}","` +
		catalogSourceRepoTypeAnnotation + `":"` + catalogSourceRepoTypeCluster + `","` +
		catalogSourceRepoAnnotation + `":"rancher-charts"}}]}`
}

// catalogInstall is the install action on the ClusterRepo at path, its
// Operation name captured as capture.
func catalogInstall(path, capture string) Observation {
	return Observation{
		Name:      "install",
		Request:   RequestSpec{Method: http.MethodPost, Path: path, Query: catalogActionQuery("install"), Headers: catalogActionHeaders(), Body: catalogInstallBody(), Identity: IdentityAdmin},
		Captures:  map[string]Capture{capture: {Path: "operationName"}},
		Normalize: actionAnswerRules(),
	}
}

// catalogOperation reads the Operation at path until done accepts it.
func catalogOperation(name, path string, done func(decoded map[string]any, status int, captured map[string]string) (bool, string, error)) Observation {
	return Observation{
		Name:      name,
		Request:   RequestSpec{Method: http.MethodGet, Path: path, Identity: IdentityAdmin},
		Until:     done,
		Normalize: operationRules(),
	}
}

// actionAnswerRules keep an action's 201 answer without its links, which
// carry the server's address.
func actionAnswerRules() []NormalizeRule {
	return []NormalizeRule{{Path: "links", Replace: "<links>"}}
}

// operationRules keep the keys of an Operation the waits read, without
// the owner's generated name and uid and the condition times.
func operationRules() []NormalizeRule {
	return []NormalizeRule{
		{Path: "metadata", KeepKeys: []string{"name", "namespace", "generation", "ownerReferences"}},
		{Path: "metadata.ownerReferences.name"},
		{Path: "metadata.ownerReferences.uid"},
		{Path: "status", KeepKeys: []string{"action", "conditions", "namespace", "observedGeneration", "podCreated", "podName", "podNamespace", "releaseName"}},
		{Path: "status.conditions.lastUpdateTime", Replace: "<ts>"},
		{Path: "status.conditions.lastTransitionTime", Replace: "<ts>"},
		{Path: "status.conditions", SortBy: "type"},
	}
}

// appRules keep the keys of an App the waits and app get read, without
// the owner's generated name and uid, and replace the values. The App's
// metadata.generation and status.observedGeneration are not kept: the CLI
// reads neither, and their value depends on whether Rancher's app
// controller handled Helm's first release write before its second.
func appRules() []NormalizeRule {
	return []NormalizeRule{
		{Path: "metadata", KeepKeys: []string{"name", "namespace", "ownerReferences"}},
		{Path: "metadata.ownerReferences.name"},
		{Path: "metadata.ownerReferences.uid"},
		{Path: "spec", KeepKeys: []string{"chart", "info", "name", "namespace", "version", "helmVersion", "values"}},
		{Path: "spec.chart", KeepKeys: []string{"metadata"}},
		{Path: "spec.chart.metadata", KeepKeys: []string{"name", "version", "annotations"}},
		{Path: "spec.chart.metadata.annotations", KeepKeys: []string{catalogRequestIDAnnotation, catalogSourceRepoTypeAnnotation, catalogSourceRepoAnnotation}},
		{Path: "spec.info", KeepKeys: []string{"status", "description"}},
		{Path: "spec.values", Replace: "<value>"},
		{Path: "status", KeepKeys: []string{"summary"}},
	}
}

// operationSucceeded accepts an Operation whose Helm run ended without a
// stall: the generation observed, Reconciling False and Stalled False, both
// present. It is stricter than app install --wait, which also accepts a
// missing Stalled, so that a record and a verify accept the same
// conditions. A Stalled True fails the wait.
func operationSucceeded(decoded map[string]any, status int, _ map[string]string) (bool, string, error) {
	if status != http.StatusOK {
		return false, fmt.Sprintf("Operation answering status %d", status), nil
	}
	reconciling, _ := conditionStatus(decoded, "Reconciling")
	stalled, _ := conditionStatus(decoded, "Stalled")
	if stalled == "True" {
		return false, "", errors.New("the Operation is Stalled")
	}
	return generationObserved(decoded) && reconciling == "False" && stalled == "False", operationState(decoded), nil
}

// operationStalled accepts an Operation whose Helm run ended with a stall:
// the generation observed, Stalled True and Reconciling False. An
// Operation that ended without a stall fails the wait.
func operationStalled(decoded map[string]any, status int, _ map[string]string) (bool, string, error) {
	if status != http.StatusOK {
		return false, fmt.Sprintf("Operation answering status %d", status), nil
	}
	reconciling, _ := conditionStatus(decoded, "Reconciling")
	stalled, _ := conditionStatus(decoded, "Stalled")
	observed := generationObserved(decoded)
	if observed && reconciling == "False" && stalled != "True" {
		return false, "", errors.New("the Operation ended without a stall")
	}
	return observed && reconciling == "False" && stalled == "True", operationState(decoded), nil
}

// appDeployed accepts the App at deployed with this run's request id and its
// generation observed; a failed revision fails the wait, and a 404 reads
// again.
func appDeployed(decoded map[string]any, status int, captured map[string]string) (bool, string, error) {
	if status != http.StatusOK {
		return false, fmt.Sprintf("App answering status %d", status), nil
	}
	annotations, _ := nestedValue(decoded, "spec.chart.metadata.annotations").(map[string]any)
	id, _ := annotations[catalogRequestIDAnnotation].(string)
	if id == "" || id != captured[requestIDCapture] {
		return false, "App without this request's id", nil
	}
	info := nestedString(decoded, "spec.info.status")
	if info == "failed" {
		return false, "", errors.New("the App's revision failed")
	}
	summary := nestedString(decoded, "status.summary.state")
	meta, _ := decoded["metadata"].(map[string]any)
	st, _ := decoded["status"].(map[string]any)
	generation, _ := meta["generation"].(float64)
	observed, ok := st["observedGeneration"].(float64)
	done := info == "deployed" && summary == "deployed" && ok && observed == generation
	return done, "App with spec.info.status " + appState(info) + " and status.summary.state " + appState(summary), nil
}

// appState names an App status for a wait message; a status outside the
// ones Helm and Rancher write is not shown.
func appState(s string) string {
	switch s {
	case "":
		return "absent"
	case "deployed", "failed", "uninstalled", "superseded", "uninstalling", "pending-install", "pending-upgrade", "pending-rollback", "unknown":
		return s
	}
	return "of another value"
}

// appGone accepts the App's 404.
func appGone(_ map[string]any, status int, _ map[string]string) (bool, string, error) {
	return status == http.StatusNotFound, fmt.Sprintf("App answering status %d", status), nil
}
