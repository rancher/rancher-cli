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
// is one the commands resolve. It needs a management token and one
// listing; a typed cluster on the line never concerns it.
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
	return completion.Result{Names: completion.IDsWithUniqueDisplayNames(list.Items)}
}

// steveClusters is the steve collection `access projects` lists.
const steveClusters = "management.cattle.io.clusters"

// localClusterRow is the steve id of the management cluster, the row the
// caller's cluster set always carries.
const localClusterRow = "local"

// ProjectsClusterSource is `access projects`' own --cluster source: the
// steve cluster collection the command resolves the flag against, walked
// page by page through the continuation tokens under the surface's
// page-walk allowance, so the values offered are the ones the command
// accepts even for a caller who may not list the management clusters.
// Every other site of the command is declined.
func ProjectsClusterSource() completion.Source {
	return completion.Source{Claim: claimProjectsCluster, Complete: completeSteveClusters}
}

func claimProjectsCluster(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "cluster" {
		return completion.Requirements{}, false
	}
	return completion.Requirements{
		Cluster:    completion.ClusterNone,
		Credential: completion.CredentialManagement,
		Allowance:  completion.AllowanceSteveClusterPages,
	}, true
}

// stevePage is one page of a steve collection as the source reads it.
type stevePage struct {
	Data     []steveClusterItem `json:"data"`
	Count    *int               `json:"count"`
	Continue string             `json:"continue"`
	Revision string             `json:"revision"`
}

type steveClusterItem struct {
	ID       string `json:"id"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

// completeSteveClusters walks the steve cluster collection once, under
// the same protocol the command reads it with: every page carries a
// revision and a count and they do not move between pages, every item has
// an id its metadata.name agrees with, a repeated item agrees with itself,
// a continuation token never repeats, and the count equals what was read.
// The management cluster is a row whether or not the collection listed
// it. Any disagreement answers nothing: a collection that moves under a
// TAB is not worth a second walk. Rows are ordered by id and projected to
// ids plus unique display names.
func completeSteveClusters(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
	base, err := completion.SteveCollectionPath(steveClusters)
	if err != nil {
		return completion.Result{}
	}
	type row struct{ id, display string }
	var (
		rows     []row
		byID     = map[string]row{}
		read     int
		total    *int
		revision string
		seen     = map[string]bool{}
		token    string
	)
	for page := 0; ; page++ {
		p := base
		if token != "" {
			p = base.WithQuery("continue", token)
		}
		var col stevePage
		if err := conn.GetJSON(ctx, p, "", &col); err != nil {
			return completion.Result{}
		}
		if col.Revision == "" || col.Count == nil || *col.Count < 0 {
			return completion.Result{}
		}
		if page == 0 {
			revision, total = col.Revision, col.Count
		} else if col.Revision != revision || *col.Count != *total {
			return completion.Result{}
		}
		for _, item := range col.Data {
			if item.ID == "" || (item.Metadata.Name != "" && item.Metadata.Name != item.ID) {
				return completion.Result{}
			}
			r := row{id: item.ID, display: item.Spec.DisplayName}
			if prev, dup := byID[item.ID]; dup {
				if prev != r {
					return completion.Result{}
				}
				continue
			}
			byID[item.ID] = r
			rows = append(rows, r)
		}
		read += len(col.Data)
		if col.Continue == "" {
			break
		}
		if seen[col.Continue] {
			return completion.Result{}
		}
		seen[col.Continue] = true
		token = col.Continue
	}
	if *total != read {
		return completion.Result{}
	}
	if _, ok := byID[localClusterRow]; !ok {
		rows = append(rows, row{id: localClusterRow, display: localClusterRow})
	}
	slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.id, b.id) })
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": r.id},
			"spec":     map[string]any{"displayName": r.display},
		})
	}
	return completion.Result{Names: completion.IDsWithUniqueDisplayNames(items)}
}
