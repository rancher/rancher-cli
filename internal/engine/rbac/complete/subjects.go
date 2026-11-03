package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// SubjectSource is the shared --as source of the access commands: the
// management Users that are not system accounts, each by id and, when
// the username names exactly one user across usernames and display
// names and is no account's id, by username - the values the commands
// resolve a subject by without ambiguity. Each id is described by its
// username and display name; a username value carries no description.
// Users only: the completion protocol splits a candidate at its first ':',
// so a group principal cannot be offered, and under --type group, where a
// bare value is a group, nothing is. It needs a management token and one
// listing; every other site is declined.
func SubjectSource() completion.Source {
	return completion.Source{Claim: claimAs, Complete: completeSubjects}
}

func claimAs(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "as" || completion.TailFlag(req.Tail, "type") == "group" {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

// GrantSource is grant's source: the --role value, which leaves out the
// role templates marked locked at project and cluster scope and, at global
// scope, the global roles that inherit a locked role template or one that
// does not exist (Rancher does not allow a new binding to any of them), and
// the subject positional, the user subjects of SubjectSource unless --type
// group is typed, since no group collection can be listed. Every other site
// is declined.
func GrantSource() completion.Source {
	return completion.Source{Claim: claimGrant, Complete: completeGrant}
}

// RevokeSource is revoke's source: the --role value of RoleSource, locked
// role templates included, since a binding to one can still be removed,
// and the subject positional GrantSource answers. Every other site is
// declined.
func RevokeSource() completion.Source {
	return completion.Source{Claim: claimGrantRevoke, Complete: completeRevoke}
}

func claimGrantRevoke(req completion.Request) (completion.Requirements, bool) {
	switch {
	case req.Flag == "role":
		return claimRole(req)
	case req.Flag == "" && req.Index == 0 && completion.TailFlag(req.Tail, "type") != "group":
		return completion.ManagementListing(), true
	}
	return completion.Requirements{}, false
}

// claimGrant is claimGrantRevoke, except that grant's --role at global scope
// lists the role templates after the global roles and so claims two requests.
func claimGrant(req completion.Request) (completion.Requirements, bool) {
	reqs, ok := claimGrantRevoke(req)
	if ok && req.Flag == "role" {
		if scope, _ := ScopeOfTail(req.Tail); scope == ScopeGlobal {
			return completion.ManagementTwoListings(), true
		}
	}
	return reqs, ok
}

func completeGrant(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if req.Flag == "role" {
		return completeRole(ctx, conn, req, false)
	}
	return completeSubjects(ctx, conn, req)
}

func completeRevoke(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	if req.Flag == "role" {
		return RoleSource().Complete(ctx, conn, req)
	}
	return completeSubjects(ctx, conn, req)
}

// completeSubjects lists the management Users once and projects them
// into subject values: every non-system user's id, in list order, then
// its username when no other user carries that string as username or
// display name and no account, a system one included, carries it as id
// (an exact id wins in the commands' resolution). A user without
// principal ids is left out: grant and revoke refuse to bind one. A user
// whose display name equals its own username counts once.
func completeSubjects(ctx context.Context, conn *completion.Connection, _ completion.Request) completion.Result {
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
	type entry struct{ id, username string }
	var entries []entry
	descriptions := map[string]string{}
	ids := map[string]bool{}
	named := map[string]int{}
	for _, item := range list.Items {
		if id := completion.ItemName(item); id != "" {
			ids[id] = true
		}
	}
	seen := map[string]bool{}
	for _, item := range list.Items {
		if k.SystemFilter.Matches(item) || !hasPrincipals(item) {
			continue
		}
		id := completion.ItemName(item)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		username, _ := item["username"].(string)
		display, _ := item["displayName"].(string)
		entries = append(entries, entry{id: id, username: username})
		if d := completion.ItemDescription(item, paths); d != "" {
			descriptions[id] = d
		}
		if username != "" {
			named[username]++
		}
		if display != "" && display != username {
			named[display]++
		}
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.id)
		if e.username != "" && named[e.username] == 1 && (!ids[e.username] || e.username == e.id) {
			out = append(out, e.username)
		}
	}
	return completion.Result{Names: out, Descriptions: descriptions}
}

// hasPrincipals reports whether a User carries at least one principal
// id, the field a binding names it by.
func hasPrincipals(item map[string]any) bool {
	pids, _ := item["principalIds"].([]any)
	return len(pids) > 0
}
