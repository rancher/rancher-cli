package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// SaveEtcdSnapshotSource completes the <cluster> argument of cluster
// save-etcd-snapshot with the provisioning clusters that have spec.rkeConfig,
// the clusters the command accepts: those of the --namespace typed on the
// line, or of the kind's default Fleet workspace. It needs a management token
// and one listing, the first page of the Steve collection; every other site,
// and a listing that fails or is refused, offers no candidates.
func SaveEtcdSnapshotSource() completion.Source {
	return completion.Source{Claim: claimSaveEtcdSnapshot, Complete: completeSaveEtcdSnapshot}
}

func claimSaveEtcdSnapshot(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "" || req.Index != 0 {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completeSaveEtcdSnapshot(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	k, ok := kinds.ByKey("provisioningcluster")
	if !ok {
		return completion.Result{}
	}
	ns := completion.TailFlag(req.Tail, "namespace")
	if ns == "" {
		ns = k.DefaultNamespace
	}
	p, err := completion.SteveNamespacedCollectionPath(k.SteveType(), ns)
	if err != nil {
		return completion.Result{}
	}
	var list struct {
		Items []map[string]any `json:"data"`
	}
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	var names []string
	for _, item := range list.Items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name != "" && clusterops.HasRKEConfig(item) {
			names = append(names, name)
		}
	}
	return completion.Result{Names: names}
}
