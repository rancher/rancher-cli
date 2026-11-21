package fidelity

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// The Rancher extension repository the probes add, with the three fields
// extension add-repository sends, and the paths of the reads of extension
// list.
const (
	extensionRepoName    = "partner-extensions"
	extensionRepoPath    = "/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos"
	extensionUIPlugins   = "/k8s/clusters/local/apis/catalog.cattle.io/v1/uiplugins"
	extensionApps        = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/cattle-ui-plugin-system/apps"
	extensionSteveRepos  = "/v1/catalog.cattle.io.clusterrepos"
	extensionRepoPayload = `{"apiVersion":"catalog.cattle.io/v1","kind":"ClusterRepo","metadata":{"name":"` + extensionRepoName +
		`"},"spec":{"gitRepo":"https://github.com/rancher/partner-extensions","gitBranch":"main"}}`
)

// extensionProbes records the responses that extension add-repository and
// extension list read. Neither probe reaches github.com itself: the oracle
// downloads the repository it is told to add.
//
//   - extension/add-repository-dry-run: the ClusterRepo list the command
//     reads first and the server-side dry-run of the create. Nothing is
//     stored.
//   - extension/add-and-list: the real create, the ClusterRepo read until
//     Rancher has downloaded it, the reads of extension list (the list of
//     ClusterRepos from the Steve API, the repository's index, the local
//     management Cluster, the UIPlugins of every namespace and the Apps of
//     the extension namespace) and the delete by name.
//
// Both refuse to run when a ClusterRepo named partner-extensions exists, and
// delete only that name. The index and the object lists record their shape,
// never their entries: the content of the repository comes from github.com
// and changes without a change in Rancher.
func extensionProbes() []Probe {
	// The local management Cluster, read as Rancher responds, not as a lookup the CLI makes.
	const extensionLocalSteve = "/v1/management.cattle.io.clusters/local"
	create := func(dryRun bool) RequestSpec {
		query := url.Values{"fieldManager": {catalogFieldManager}}
		if dryRun {
			query.Set("dryRun", "All")
		}
		return RequestSpec{Method: http.MethodPost, Path: extensionRepoPath, Query: query, Body: extensionRepoPayload, Identity: IdentityAdmin}
	}
	repoRules := []NormalizeRule{
		{Path: "metadata", KeepKeys: []string{"name"}},
		{Path: "status"},
	}
	// The ClusterRepos whose name starts with rancher- (rancher-charts and the
	// other Rancher repositories) are dropped: a list that holds one
	// ClusterRepo after a dry run would show that the dry run stored it.
	listRules := []NormalizeRule{
		{Path: "metadata", Replace: "<scrubbed>"},
		{Path: "items", DropPrefix: "rancher-", DropBy: "metadata.name"},
		{Path: "items.metadata", KeepKeys: []string{"name"}},
		{Path: "items.spec"},
		{Path: "items.status"},
	}
	objectsRules := []NormalizeRule{
		{Path: "metadata", Replace: "<scrubbed>"},
		{Path: "items", Replace: "<scrubbed>"},
	}
	absent := deleteGuard("check that no ClusterRepo " + extensionRepoName + " exists")
	remove := deleteFixture("ClusterRepo "+extensionRepoName, extensionRepoPath+"/"+extensionRepoName)
	return []Probe{
		{
			Name:       "extension/add-repository-dry-run",
			OracleOnly: true,
			Setup:      []Step{absent},
			Observations: []Observation{
				{Name: "list", Request: RequestSpec{Method: http.MethodGet, Path: extensionRepoPath, Identity: IdentityAdmin}, Normalize: listRules},
				{Name: "dry-run-create", Request: create(true), Normalize: repoRules},
				{Name: "list-after", Request: RequestSpec{Method: http.MethodGet, Path: extensionRepoPath, Identity: IdentityAdmin}, Normalize: listRules},
			},
		},
		{
			Name:        "extension/add-and-list",
			OracleOnly:  true,
			OwnPairOnly: true,
			Setup:       []Step{absent},
			Observations: []Observation{
				{Name: "create", Request: create(false), Normalize: repoRules},
				{
					Name:    "repository",
					Request: RequestSpec{Method: http.MethodGet, Path: extensionSteveRepos + "/" + extensionRepoName, Identity: IdentityAdmin},
					Until:   extensionRepoActive,
					Normalize: []NormalizeRule{
						{Path: "metadata", KeepKeys: []string{"name", "state"}},
						{Path: "metadata.state", KeepKeys: []string{"name"}},
						{Path: "spec", KeepKeys: []string{"gitBranch", "gitRepo"}},
						{Path: "status"},
						{Path: "links"},
						{Path: "actions"},
					},
				},
				{
					Name:    "collection",
					Request: RequestSpec{Method: http.MethodGet, Path: extensionSteveRepos, Identity: IdentityAdmin},
					Normalize: []NormalizeRule{
						{Path: "data", Replace: "<scrubbed>"},
						{Path: "links"}, {Path: "actions"}, {Path: "createTypes"}, {Path: "revision"}, {Path: "count"},
					},
				},
				{
					Name:      "index",
					Request:   RequestSpec{Method: http.MethodGet, Path: extensionSteveRepos + "/" + extensionRepoName, Query: url.Values{"link": {"index"}}, Identity: IdentityAdmin},
					Normalize: []NormalizeRule{{Path: "entries", Replace: "<scrubbed>"}, {Path: "generated", Replace: "<ts>"}},
				},
				{
					Name:    "local-cluster",
					Request: RequestSpec{Method: http.MethodGet, Path: extensionLocalSteve, Identity: IdentityAdmin},
					Normalize: []NormalizeRule{
						{Path: "metadata"}, {Path: "spec"}, {Path: "links"}, {Path: "actions"},
						{Path: "status", KeepKeys: []string{"info", "version"}},
						{Path: "status.info", KeepKeys: []string{"kubernetesVersion"}},
						{Path: "status.info.kubernetesVersion", Replace: "<version>"},
						{Path: "status.version", KeepKeys: []string{"gitVersion"}},
						{Path: "status.version.gitVersion", Replace: "<version>"},
					},
				},
				{Name: "uiplugins", Request: RequestSpec{Method: http.MethodGet, Path: extensionUIPlugins, Identity: IdentityAdmin}, Normalize: objectsRules},
				{Name: "apps", Request: RequestSpec{Method: http.MethodGet, Path: extensionApps, Identity: IdentityAdmin}, Normalize: objectsRules},
			},
			Teardown: []Step{remove},
		},
	}
}

// extensionRepoActive accepts the ClusterRepo from the Steve API once Rancher
// has downloaded the repository: state active and status.indexConfigMapName
// set. Rancher reports the repository active before it has stored the index,
// and the index read responds with 500 until then. An error state ends the
// wait, for it does not recover on its own.
func extensionRepoActive(decoded map[string]any, status int, _ map[string]string) (bool, string, error) {
	if status != http.StatusOK {
		return false, fmt.Sprintf("ClusterRepo responded with status %d", status), nil
	}
	switch nestedString(decoded, "metadata.state.name") {
	case "active":
		if nestedString(decoded, "status.indexConfigMapName") == "" {
			return false, "ClusterRepo active, index not stored yet", nil
		}
		return true, "ClusterRepo active", nil
	case "error":
		return false, "", fmt.Errorf("the ClusterRepo is in state error: the server may have no access to github.com")
	}
	return false, "ClusterRepo not active yet", nil
}

// deleteGuard fails when a ClusterRepo named partner-extensions exists, to keep
// a run from deleting an object it did not create.
func deleteGuard(name string) Step {
	return Step{
		Name: name,
		Do: func(ctx context.Context, tgt Target, _ map[string]string) error {
			status, err := adminDo(ctx, tgt, http.MethodGet, extensionRepoPath+"/"+extensionRepoName, "")
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			switch status {
			case http.StatusNotFound:
				return nil
			case http.StatusOK:
				return fmt.Errorf("%s: a ClusterRepo named %s exists; delete it first", name, extensionRepoName)
			}
			return fmt.Errorf("%s: unexpected status %d", name, status)
		},
	}
}
