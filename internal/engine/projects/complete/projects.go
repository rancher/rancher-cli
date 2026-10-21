package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// ProjectSource is the shared --project source and inspect project's
// positional: the projects of the resolved cluster as steve lists them for
// the caller, every id plus each display name that is unique and is not
// itself an id, so a value offered is one the command resolves; each id
// is described by its display name. It needs the typed cluster resolved,
// a management token and one page walk.
func ProjectSource() completion.Source {
	return completion.Source{Claim: claimProjects, Complete: completeProjects}
}

// ProjectIDSource is the project kind's name source for the generated
// get, patch and delete verbs, which address a project by its id only:
// the same read, ids only, each described by its display name.
func ProjectIDSource() completion.Source {
	return completion.Source{Claim: claimProjects, Complete: completeProjectIDs}
}

func completeProjectIDs(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	list, err := conn.Projects(ctx, req.ClusterID)
	if err != nil {
		return completion.Result{}
	}
	names := make([]string, len(list))
	descriptions := map[string]string{}
	for i, p := range list {
		names[i] = p.ID
		if _, ok := descriptions[p.ID]; !ok && p.DisplayName != "" {
			descriptions[p.ID] = p.DisplayName
		}
	}
	return completion.Result{Names: names, Descriptions: descriptions}
}

func claimProjects(completion.Request) (completion.Requirements, bool) {
	r := completion.ClusterListing()
	r.Allowance = completion.AllowanceSteveProjectPages
	return r, true
}

func completeProjects(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	list, err := conn.Projects(ctx, req.ClusterID)
	if err != nil {
		return completion.Result{}
	}
	items := make([]map[string]any, len(list))
	for i, p := range list {
		items[i] = map[string]any{"metadata": map[string]any{"name": p.ID}, "spec": map[string]any{"displayName": p.DisplayName}}
	}
	names, displayNames := completion.IDsWithDisplayNames(items)
	return completion.Result{Names: names, Descriptions: displayNames}
}

// ProjectPositionalSource is inspect project's own source: the first
// positional only, which that command resolves by id or unique display
// name like --project.
func ProjectPositionalSource() completion.Source {
	return completion.Source{Claim: claimFirstPositional, Complete: completeProjects}
}

func claimFirstPositional(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "" || req.Index != 0 {
		return completion.Requirements{}, false
	}
	return claimProjects(req)
}
