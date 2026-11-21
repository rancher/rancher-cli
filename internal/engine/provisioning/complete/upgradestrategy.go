package complete

import (
	"context"
	"slices"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// strategyValueFlags are the flags of `cluster set-upgrade-strategy` that
// complete from a list of values, with the group each belongs to and the list.
var strategyValueFlags = map[string]struct {
	group  string
	values []string
}{
	"control-plane-grace-period":  {"control-plane", []string{"30", "60", "120", "300"}},
	"worker-grace-period":         {"worker", []string{"30", "60", "120", "300"}},
	"control-plane-drain-timeout": {"control-plane", []string{"30s", "1m", "2m", "5m", "10m"}},
	"worker-drain-timeout":        {"worker", []string{"30s", "1m", "2m", "5m", "10m"}},
}

// UpgradeStrategySource is `cluster set-upgrade-strategy`'s own source. It
// claims the <cluster> positional, and the grace period and drain timeout
// flags of both groups after it, and offers only values the command accepts:
// the clusters it takes, and the values of those flags for a provisioned
// cluster whose drain of the flag's group is not turned off on the line. Both
// need a management token; every other site is declined.
func UpgradeStrategySource() completion.Source {
	return completion.Source{Claim: claimUpgradeStrategy, Complete: completeUpgradeStrategy}
}

func claimUpgradeStrategy(req completion.Request) (completion.Requirements, bool) {
	if req.Flag == "" && req.Index == 0 {
		// One list of the provisioning Clusters of the namespace and one page of
		// the management Clusters, both in Steve: two requests.
		return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceTwo}, true
	}
	if _, ok := strategyValueFlags[req.Flag]; ok && len(req.Positionals) > 0 {
		// The provisioning Cluster: one request.
		return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}, true
	}
	return completion.Requirements{}, false
}

func completeUpgradeStrategy(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if flag, ok := strategyValueFlags[req.Flag]; ok {
		return completeStrategyValues(ctx, conn, req, flag.group, flag.values)
	}
	return completeStrategyClusters(ctx, conn, req)
}

// completeStrategyClusters offers the clusters CheckStrategyCluster accepts.
// It reads no Setting: a cluster is offered in every state of version
// management.
func completeStrategyClusters(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
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
		if _, refused := clusterops.CheckStrategyCluster(prov, byID[linkedManagementID(prov)]); refused != nil {
			continue
		}
		names = append(names, name)
	}
	return completion.Result{Names: names}
}

// completeStrategyValues offers the values of a drain option flag of a group
// for a provisioned cluster the command accepts: an imported cluster refuses
// the flag, and so does a line that turns the group's drain off with the exact
// word --<group>-drain=false.
func completeStrategyValues(ctx context.Context, conn *completion.Connection, req completion.Request, group string, values []string) completion.Result {
	if len(req.Positionals) == 0 || slices.Contains(req.Tail, "--"+group+"-drain=false") {
		return completion.Result{}
	}
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
	k, _ := kinds.ByKey("provisioningcluster")
	provPath, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, ns, k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return completion.Result{}
	}
	var prov map[string]any
	if err := conn.GetJSON(ctx, provPath, "", &prov); err != nil {
		return completion.Result{}
	}
	// The flags belong to a provisioned cluster, which has spec.rkeConfig. With
	// no management Cluster, CheckStrategyCluster also refuses an imported
	// cluster and the cluster Rancher runs in; each check states the rule alone.
	if !clusterops.HasRKEConfig(prov) {
		return completion.Result{}
	}
	if _, refused := clusterops.CheckStrategyCluster(prov, nil); refused != nil {
		return completion.Result{}
	}
	return completion.Result{Names: slices.Clone(values)}
}
