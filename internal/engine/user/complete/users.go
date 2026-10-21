package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// UserIDSource is the user kind's name source on the generated verbs, on
// inspect user and on create password-change-request: the ids
// (metadata.name) of the management Users the kind table does not mark
// as system accounts, each described by its username and display name.
// The values are ids only; the descriptions are display only, and inspect
// user also takes a username, a display name or a principal. It needs a
// management token and one listing.
func UserIDSource() completion.Source {
	return completion.Source{Claim: claimUsers, Complete: completeUserIDs}
}

func claimUsers(completion.Request) (completion.Requirements, bool) {
	return completion.ManagementListing(), true
}

func completeUserIDs(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	k, ok := kinds.ByKey("user")
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
	paths := k.DescriptionPaths()
	var ids []string
	descriptions := map[string]string{}
	for _, item := range list.Items {
		if k.SystemFilter.Matches(item) {
			continue
		}
		if id := completion.ItemName(item); id != "" {
			ids = append(ids, id)
			if _, ok := descriptions[id]; !ok {
				if d := completion.ItemDescription(item, paths); d != "" {
					descriptions[id] = d
				}
			}
		}
	}
	return completion.Result{Names: ids, Descriptions: descriptions}
}
