package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// ProjectSource is the shared --project source and the project kind's
// name source: the management Projects in the resolved cluster's
// namespace, every id plus each display name that is unique and is not
// itself an id, so a value offered is one the commands resolve. It needs
// the typed cluster resolved, a management token and one listing.
func ProjectSource() completion.Source {
	return completion.Source{Claim: claimProjects, Complete: completeProjects}
}

func claimProjects(completion.Request) (completion.Requirements, bool) {
	return completion.ClusterListing(), true
}

func completeProjects(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	k, ok := kinds.ByKey("project")
	if !ok {
		return completion.Result{}
	}
	p, err := completion.ManagementNamespacedPath(k.GVR.Group, k.GVR.Version, req.ClusterID, k.GVR.Resource)
	if err != nil {
		return completion.Result{}
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	return completion.Result{Names: completion.IDsWithUniqueDisplayNames(list.Items)}
}
