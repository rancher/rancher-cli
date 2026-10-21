package complete

import (
	"context"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// ClusterSource is the shared --cluster source and the management cluster
// kind's name source: the management cluster list, every id plus each
// display name that is unique and is not itself an id, so a value offered
// is one the commands resolve; each id is described by its display name.
// It needs a management token and one listing; a typed cluster on the line
// never concerns it.
func ClusterSource() completion.Source {
	return completion.Source{Claim: claimManagementList, Complete: completeClusters}
}

func claimManagementList(completion.Request) (completion.Requirements, bool) {
	return completion.ManagementListing(), true
}

func completeClusters(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	k, ok := kinds.ByKey("managementcluster")
	if !ok {
		return completion.Result{}
	}
	p, err := completion.ManagementPath(k.GVR.Group, k.GVR.Version, k.GVR.Resource)
	if err != nil {
		return completion.Result{}
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	names, displayNames := completion.IDsWithDisplayNames(list.Items)
	return completion.Result{Names: names, Descriptions: displayNames}
}

// SteveClusterSource is the shared --cluster source and `inspect
// cluster`'s positional source: the clusters the caller can read, as the
// commands resolve the value against them, every id plus each display name
// that is unique and is not itself an id; each id is described by its
// display name. It needs a management token and one walk of the steve
// clusters collection; a typed cluster on the line never concerns it.
// Every other site is declined.
func SteveClusterSource() completion.Source {
	return completion.Source{Claim: claimSteveCluster, Complete: completeReadableClusters}
}

func claimSteveCluster(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "cluster" && (req.Flag != "" || req.Index != 0) {
		return completion.Requirements{}, false
	}
	return steveClusterRequirements(), true
}

func steveClusterRequirements() completion.Requirements {
	return completion.Requirements{
		Cluster:    completion.ClusterNone,
		Credential: completion.CredentialManagement,
		Allowance:  completion.AllowanceSteveClusterPages,
	}
}

func completeReadableClusters(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	list, err := conn.Clusters(ctx)
	if err != nil {
		return completion.Result{}
	}
	names, displayNames := completion.IDsWithDisplayNames(clusterItems(list))
	return completion.Result{Names: names, Descriptions: displayNames}
}

func clusterItems(list []completion.ClusterItem) []map[string]any {
	items := make([]map[string]any, 0, len(list))
	for _, c := range list {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": c.ID},
			"spec":     map[string]any{"displayName": c.DisplayName},
		})
	}
	return items
}

// localClusterRow is the steve id of the management cluster, the row the
// caller's cluster set always carries.
const localClusterRow = "local"

// ProjectsClusterSource is `access projects`' own --cluster source: the
// clusters the caller can read, as the command resolves the flag against
// them, plus the management cluster's row the command always carries.
// Every other site of the command is declined.
func ProjectsClusterSource() completion.Source {
	return completion.Source{Claim: claimProjectsCluster, Complete: completeSteveClusters}
}

func claimProjectsCluster(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "cluster" {
		return completion.Requirements{}, false
	}
	return steveClusterRequirements(), true
}

// completeSteveClusters reads the clusters through the connection's
// lister, in one walk: any disagreement in the collection answers nothing,
// since a collection that moves under a TAB is not worth a second walk.
// The management cluster is a row whether or not the collection listed it.
// Rows are ordered by id and projected to ids plus unique display names.
func completeSteveClusters(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	list, err := conn.Clusters(ctx)
	if err != nil {
		return completion.Result{}
	}
	if !slices.ContainsFunc(list, func(c completion.ClusterItem) bool { return c.ID == localClusterRow }) {
		list = append(list, completion.ClusterItem{ID: localClusterRow, DisplayName: localClusterRow})
	}
	slices.SortFunc(list, func(a, b completion.ClusterItem) int { return strings.Compare(a.ID, b.ID) })
	names, displayNames := completion.IDsWithDisplayNames(clusterItems(list))
	return completion.Result{Names: names, Descriptions: displayNames}
}
