package complete

import (
	"context"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// UserIDSource is the user kind's name source on the generated verbs, on
// inspect user and on create password-change-request: the ids
// (metadata.name) of the management Users the kind table does not mark
// as system accounts. Ids only: those commands address a user by id and
// reject a bare username. It needs a management token and one listing.
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
	var ids []string
	for _, item := range list.Items {
		if isSystem(item, k.SystemFilter) {
			continue
		}
		if id := completion.ItemName(item); id != "" {
			ids = append(ids, id)
		}
	}
	return completion.Result{Names: ids}
}

// isSystem applies the kind table's system-account rule to one object:
// any element of the string array at the filter's path carries the
// prefix. A kind without a filter has no system accounts.
func isSystem(item map[string]any, f *kinds.PrefixFilter) bool {
	if f == nil {
		return false
	}
	var cur any = item
	for _, key := range f.Path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur = m[key]
	}
	values, ok := cur.([]any)
	if !ok {
		return false
	}
	for _, v := range values {
		if s, ok := v.(string); ok && strings.HasPrefix(s, f.Prefix) {
			return true
		}
	}
	return false
}
