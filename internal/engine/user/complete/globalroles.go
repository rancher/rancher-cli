package complete

import (
	"context"
	"errors"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// GlobalRoleSource is create user's source: the --global-role value, the ids
// (metadata.name) of the GlobalRoles Rancher accepts in a new binding, each
// described by its display name. A global role that inherits a locked
// RoleTemplate, or one that does not exist, is left out: Rancher refuses a
// binding to it. The RoleTemplates are listed for that in a second request,
// made only when some global role names an inherited RoleTemplate; when that
// listing fails, only the global roles that name none are offered, and when
// the context ends during it nothing is offered. A role the caller may not
// bind is offered, as grant offers it: the server refuses it at the preview.
// Every other site is declined. It needs a management token and, at most, two
// listings.
func GlobalRoleSource() completion.Source {
	return completion.Source{Claim: claimGlobalRole, Complete: completeGlobalRoles}
}

func claimGlobalRole(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "global-role" {
		return completion.Requirements{}, false
	}
	return completion.ManagementTwoListings(), true
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
	roles, err := kinds.BindableGlobalRoles(list.Items, func() ([]map[string]any, error) {
		return listRoleTemplates(ctx, conn)
	})
	if err != nil && ctx.Err() != nil {
		return completion.Result{}
	}
	paths := k.DescriptionPaths()
	var names []string
	descriptions := map[string]string{}
	for _, item := range roles {
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

// listRoleTemplates lists every RoleTemplate, whatever its context: Rancher
// reads the role templates a global role inherits by name alone.
func listRoleTemplates(ctx context.Context, conn *completion.Connection) ([]map[string]any, error) {
	k, ok := kinds.Lookup("roletemplate")
	if !ok {
		return nil, errors.New("roletemplate kind is not registered")
	}
	p, err := completion.ManagementPath(k.GVR.Group, k.GVR.Version, k.GVR.Resource)
	if err != nil {
		return nil, err
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}
