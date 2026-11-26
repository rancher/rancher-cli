package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/clusterops"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// restoreSnapshotFlag and restoreModeFlag are the flags of `cluster
// restore-etcd-snapshot` this source completes.
const (
	restoreSnapshotFlag = "snapshot"
	restoreModeFlag     = "restore-mode"
)

// RestoreEtcdSnapshotSource is `cluster restore-etcd-snapshot`'s own source.
// It claims the <cluster> argument, --snapshot and --restore-mode, and offers
// only values the command accepts for the cluster typed on the line: the
// clusters that have spec.rkeConfig; the etcd snapshot objects the snapshot
// checks accept for the restore mode typed on the line (or its default); and
// the restore modes the checks accept for the etcd snapshot object typed on the
// line. The two flags need the cluster typed first, and --restore-mode the
// snapshot. They read the provisioning Cluster by name and either one page of
// the Steve collection of etcd snapshot objects (the first page; a
// continuation is not followed) or the etcd snapshot object by name. Every
// read that fails or is refused offers nothing, and every other site is
// declined. It checks neither the readiness of the cluster nor an operation in
// progress.
func RestoreEtcdSnapshotSource() completion.Source {
	return completion.Source{Claim: claimRestoreEtcdSnapshot, Complete: completeRestoreEtcdSnapshot}
}

func claimRestoreEtcdSnapshot(req completion.Request) (completion.Requirements, bool) {
	switch {
	case req.Flag == "" && req.Index == 0:
		return completion.ManagementListing(), true
	case req.Flag == restoreSnapshotFlag || req.Flag == restoreModeFlag:
		return completion.ManagementTwoListings(), true
	}
	return completion.Requirements{}, false
}

func completeRestoreEtcdSnapshot(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	switch req.Flag {
	case restoreSnapshotFlag:
		return completeRestoreSnapshots(ctx, conn, req)
	case restoreModeFlag:
		return completeRestoreModes(ctx, conn, req)
	}
	return completeSaveEtcdSnapshot(ctx, conn, req)
}

// readRestoreCluster reads the provisioning Cluster typed on the line and
// returns it when it has spec.rkeConfig, the clusters the command accepts.
func readRestoreCluster(ctx context.Context, conn *completion.Connection, req completion.Request, ns string) (map[string]any, bool) {
	if len(req.Positionals) == 0 {
		return nil, false
	}
	k, ok := kinds.ByKey("provisioningcluster")
	if !ok {
		return nil, false
	}
	p, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, ns, k.GVR.Resource, req.Positionals[0])
	if err != nil {
		return nil, false
	}
	var prov map[string]any
	if err := conn.GetJSON(ctx, p, "", &prov); err != nil || !clusterops.HasRKEConfig(prov) {
		return nil, false
	}
	return prov, true
}

// completeRestoreSnapshots offers the etcd snapshot objects of the typed
// cluster that CheckProvisionedRestore accepts with the restore mode typed on
// the line, or its default.
func completeRestoreSnapshots(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
	prov, ok := readRestoreCluster(ctx, conn, req, ns)
	if !ok {
		return completion.Result{}
	}
	k, ok := kinds.ByKey("etcdsnapshot")
	if !ok {
		return completion.Result{}
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
	mode := completion.TailFlag(req.Tail, restoreModeFlag)
	var names []string
	for _, item := range list.Items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name != "" && clusterops.CheckProvisionedRestore(item, prov, mode).Problem == clusterops.ProvisionedRestoreAccepted {
			names = append(names, name)
		}
	}
	return completion.Result{Names: names}
}

// completeRestoreModes offers the restore modes CheckProvisionedRestore
// accepts for the etcd snapshot object typed on the line; the answer is
// always a subset of the flag's values.
func completeRestoreModes(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	snapshot := completion.TailFlag(req.Tail, restoreSnapshotFlag)
	if snapshot == "" {
		return completion.Result{}
	}
	ns, ok := namespaceOf(req)
	if !ok {
		return completion.Result{}
	}
	prov, ok := readRestoreCluster(ctx, conn, req, ns)
	if !ok {
		return completion.Result{}
	}
	k, ok := kinds.ByKey("etcdsnapshot")
	if !ok {
		return completion.Result{}
	}
	p, err := completion.ManagementNamedResourcePath(k.GVR.Group, k.GVR.Version, ns, k.GVR.Resource, snapshot)
	if err != nil {
		return completion.Result{}
	}
	var object map[string]any
	if err := conn.GetJSON(ctx, p, "", &object); err != nil {
		return completion.Result{}
	}
	var names []string
	for _, mode := range []string{clusterops.RestoreModeNone, clusterops.RestoreModeKubernetesVersion, clusterops.RestoreModeAll} {
		if clusterops.CheckProvisionedRestore(object, prov, mode).Problem == clusterops.ProvisionedRestoreAccepted {
			names = append(names, mode)
		}
	}
	return completion.Result{Names: names}
}
