package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// GlobalRoleSource is create user's source: the --global-role value, the ids
// (metadata.name) of every GlobalRole, each described by its display name.
// A role the caller may not bind is offered too, as grant offers it: the
// server refuses it at the preview. Every other site is declined. It needs a
// management token and one listing.
func GlobalRoleSource() completion.Source {
	return completion.Source{Claim: claimGlobalRole, Complete: completeGlobalRoles}
}

func claimGlobalRole(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "global-role" {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

func completeGlobalRoles(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	k, ok := kinds.Lookup("globalrole")
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
	var names []string
	descriptions := map[string]string{}
	for _, item := range list.Items {
		if name := completion.ItemName(item); name != "" {
			names = append(names, name)
			if _, ok := descriptions[name]; !ok {
				if d := completion.ItemDescription(item, paths); d != "" {
					descriptions[name] = d
				}
			}
		}
	}
	return completion.Result{Names: completion.Dedupe(names), Descriptions: descriptions}
}
