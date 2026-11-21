package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

const (
	// versionManagementSetting is the Setting that turns version management on
	// or off for an imported cluster whose annotation leaves it to the Setting.
	versionManagementSetting = "imported-cluster-version-management"

	managementGroup   = "management.cattle.io"
	managementVersion = "v3"

	// steveClusters is the type of the management Clusters in Steve. A caller
	// whose role lets it read a cluster only through Steve (Cluster Owner)
	// reads them there, as the command does.
	steveClusters = "management.cattle.io.clusters"
)

// UpgradeSource is `cluster upgrade`'s own source. It claims the <cluster>
// positional and --k8s-version after it, and offers only values the command
// accepts: the clusters it takes, and the releases of the cluster's
// distribution that its version rules accept. Both need a management token;
// every other site is declined.
func UpgradeSource() completion.Source {
	return completion.Source{Claim: claimUpgrade, Complete: completeUpgrade}
}

func claimUpgrade(req completion.Request) (completion.Requirements, bool) {
	switch {
	case req.Flag == "" && req.Index == 0:
		return claimUpgradeCluster()
	case req.Flag == "k8s-version" && len(req.Positionals) > 0:
		return claimUpgradeVersion()
	}
	return completion.Requirements{}, false
}

// claimUpgradeCluster is the claim of the <cluster> site: one list of the
// provisioning Clusters of the namespace and one page of the management
// Clusters, both in Steve, and, when an imported cluster leaves version
// management to the Setting, the Setting: up to three requests.
func claimUpgradeCluster() (completion.Requirements, bool) {
	return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceThree}, true
}

// claimUpgradeVersion is the claim of the --k8s-version site: the provisioning
// Cluster, the management Cluster, the imported-cluster-version-management
// Setting and one release list: up to four requests.
func claimUpgradeVersion() (completion.Requirements, bool) {
	return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceFour}, true
}

func completeUpgrade(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if req.Flag == "k8s-version" {
		return completeUpgradeVersions(ctx, conn, req)
	}
	return completeUpgradeClusters(ctx, conn, req)
}

// settingCache keeps the imported-cluster-version-management Setting after it
// is read once, within the number of requests the claim allows.
type settingCache struct {
	setting clusterops.SettingRead
	asked   bool
}

// namespaceOf is the --namespace typed on the line, else the kind's default
// workspace; false when the kind row is missing.
func namespaceOf(req completion.Request) (string, bool) {
	k, ok := kinds.ByKey("provisioningcluster")
	if !ok {
		return "", false
	}
	if ns := completion.TailFlag(req.Tail, "namespace"); ns != "" {
		return ns, true
	}
	return k.DefaultNamespace, true
}

// settingFor reads the Setting once, when a cluster's annotation leaves the
// decision to it.
func (c *settingCache) settingFor(ctx context.Context, conn *completion.Connection, mgmt map[string]any) clusterops.SettingRead {
	if mgmt == nil || !clusterops.NeedsVersionManagementSetting(mgmt) {
		return clusterops.SettingRead{}
	}
	if c.asked {
		return c.setting
	}
	c.asked = true
	p, err := completion.ManagementNamedResourcePath(managementGroup, managementVersion, "", "settings", versionManagementSetting)
	if err != nil {
		c.setting = clusterops.SettingRead{Code: "GENERIC"}
		return c.setting
	}
	var obj struct {
		Value   string `json:"value"`
		Default string `json:"default"`
	}
	if err := conn.GetJSON(ctx, p, "", &obj); err != nil {
		c.setting = clusterops.SettingRead{Code: "GENERIC"}
		return c.setting
	}
	value := obj.Value
	if value == "" {
		value = obj.Default
	}
	c.setting = clusterops.SettingRead{Value: value, Read: true}
	return c.setting
}

// linkedManagementID is the id of the management Cluster of an imported
// cluster, "" for a provisioned one or one with no link.
func linkedManagementID(prov map[string]any) string {
	if clusterops.HasRKEConfig(prov) {
		return ""
	}
	status, _ := prov["status"].(map[string]any)
	id, _ := status["clusterName"].(string)
	return id
}

// readClusterLists reads the first page of the provisioning Clusters of the
// namespace and the first page of the management Clusters, each one request,
// and returns the provisioning Clusters and the management Clusters by id. A
// cluster beyond a page is not offered: a value is never offered that the
// command would refuse. ok is false when either read fails.
func readClusterLists(ctx context.Context, conn *completion.Connection, ns string) (provisioning []map[string]any, managementByID map[string]map[string]any, ok bool) {
	k, _ := kinds.ByKey("provisioningcluster")
	provPath, err := completion.SteveNamespacedCollectionPath(k.SteveType(), ns)
	if err != nil {
		return nil, nil, false
	}
	var provList struct {
		Items []map[string]any `json:"data"`
	}
	if err := conn.GetJSON(ctx, provPath, "", &provList); err != nil {
		return nil, nil, false
	}
	mgmtPath, err := completion.SteveCollectionPath(steveClusters)
	if err != nil {
		return nil, nil, false
	}
	var mgmtList struct {
		Items []map[string]any `json:"data"`
	}
	if err := conn.GetJSON(ctx, mgmtPath, "", &mgmtList); err != nil {
		return nil, nil, false
	}
	byID := map[string]map[string]any{}
	for _, m := range mgmtList.Items {
		meta, _ := m["metadata"].(map[string]any)
		if id, _ := meta["name"].(string); id != "" {
			byID[id] = m
		}
	}
	return provList.Items, byID, true
}

func completeUpgradeClusters(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
	var cache settingCache
	provItems, byID, ok := readClusterLists(ctx, conn, ns)
	if !ok {
		return completion.Result{}
	}
	var names []string
	for _, prov := range provItems {
		meta, _ := prov["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			continue
		}
		mgmt := byID[linkedManagementID(prov)]
		if _, refused := clusterops.CheckUpgradeCluster(prov, mgmt, cache.settingFor(ctx, conn, mgmt)); refused != nil {
			continue
		}
		names = append(names, name)
	}
	return completion.Result{Names: names}
}

func completeUpgradeVersions(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
	var cache settingCache
	k, _ := kinds.ByKey("provisioningcluster")
	provPath, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, ns, k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return completion.Result{}
	}
	var prov map[string]any
	if err := conn.GetJSON(ctx, provPath, "", &prov); err != nil {
		return completion.Result{}
	}
	var mgmt map[string]any
	if id := linkedManagementID(prov); id != "" && id != "local" {
		mgmtPath, err := completion.SteveObjectPath(steveClusters, id)
		if err != nil {
			return completion.Result{}
		}
		if err := conn.GetJSON(ctx, mgmtPath, "", &mgmt); err != nil {
			return completion.Result{}
		}
	}
	facts, refused := clusterops.CheckUpgradeCluster(prov, mgmt, cache.settingFor(ctx, conn, mgmt))
	if refused != nil {
		return completion.Result{}
	}
	releasePath, err := completion.ReleaseListPath(facts.Distro)
	if err != nil {
		return completion.Result{}
	}
	var list struct {
		Data []clusterops.Release `json:"data"`
	}
	if err := conn.GetJSON(ctx, releasePath, "", &list); err != nil {
		return completion.Result{}
	}
	return completion.Result{Names: clusterops.AcceptedTargets(facts, list.Data)}
}
