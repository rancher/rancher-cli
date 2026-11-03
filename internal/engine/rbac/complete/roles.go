package complete

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// Scope is a binding scope of grant and revoke, spelled as the commands'
// own scope kind values so the two packages can convert freely.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeCluster Scope = "cluster"
	ScopeGlobal  Scope = "global"
)

// RoleKind is the role vocabulary rule grant, revoke and their completion
// share: the kind key of the collection a scope's roles come from, and
// the RoleTemplate context those roles carry. Global scope draws from
// GlobalRoles, which have no context; project and cluster scope draw from
// RoleTemplates whose context names the scope.
func RoleKind(s Scope) (kindKey, roleContext string) {
	if s == ScopeGlobal {
		return "globalrole", ""
	}
	return "roletemplate", string(s)
}

// ScopeOfTail derives the scope from the flags present on the typed line:
// --global (a boolean; a false spelling counts as absent), --project or
// --cluster (string flags; presence is enough, since a line being
// completed is unfinished and "--project= <TAB>" is about to receive a
// value). --global beside --project or --cluster is the combination the
// commands reject, so it selects nothing. Nothing outside the line is
// consulted: grant and revoke refuse an implicit scope, so completion must
// not suggest a vocabulary the command would reject.
func ScopeOfTail(tail []string) (Scope, bool) {
	global := boolFlagSet(tail, "global")
	project := flagPresent(tail, "project")
	cluster := flagPresent(tail, "cluster")
	switch {
	case global && (project || cluster):
		return "", false
	case global:
		return ScopeGlobal, true
	case project:
		return ScopeProject, true
	case cluster:
		return ScopeCluster, true
	}
	return "", false
}

// RoleSource claims the --role value site alone (no cluster, a management
// credential, one listing) and answers every role name of the scope typed
// on the line, locked role templates included, each described by its
// display name; every other site is declined.
func RoleSource() completion.Source {
	return completion.Source{Claim: claimRole, Complete: completeAllRoles}
}

func completeAllRoles(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	return completeRole(ctx, conn, req, true)
}

func claimRole(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "role" {
		return completion.Requirements{}, false
	}
	return completion.ManagementListing(), true
}

// completeRole lists the scope's role collection and keeps the names whose
// context is the scope's. The filter is client-side: the RoleTemplate CRD
// declares no selectable field for context, so a field selector cannot
// express it. Unless offerLocked is set, a role template whose locked field
// is the JSON boolean true is left out at project and cluster scope, and at
// global scope a global role is left out when Rancher refuses a binding to
// it: it inherits a locked role template or one that does not exist. Global
// scope then lists the role templates too, a second request made only when
// some global role names an inherited role template; when that listing
// fails, only the global roles that name none are kept; when the context
// ends during that listing, nothing is answered. No scope, or a conflicting
// one, answers nothing without a request.
func completeRole(ctx context.Context, conn *completion.Connection, req completion.Request, offerLocked bool) completion.Result {
	scope, ok := ScopeOfTail(req.Tail)
	if !ok {
		return completion.Result{}
	}
	kindKey, roleContext := RoleKind(scope)
	k, ok := kinds.Lookup(kindKey)
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
	roles := list.Items
	if scope == ScopeGlobal && !offerLocked {
		var err error
		roles, err = kinds.BindableGlobalRoles(roles, func() ([]map[string]any, error) {
			return listRoleTemplates(ctx, conn)
		})
		if err != nil && ctx.Err() != nil {
			return completion.Result{}
		}
	}
	paths := k.DescriptionPaths()
	var names []string
	descriptions := map[string]string{}
	for _, item := range roles {
		if roleContext != "" {
			if c, _ := item["context"].(string); c != roleContext {
				continue
			}
			if kinds.RoleTemplateLocked(item) && !offerLocked {
				continue
			}
		}
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

// flagPresent reports whether --name or --name=... is on the tail.
func flagPresent(tail []string, name string) bool {
	long := "--" + name
	for _, a := range tail {
		if a == long || strings.HasPrefix(a, long+"=") {
			return true
		}
	}
	return false
}

// boolFlagSet reports whether a boolean flag is set on the tail: a bare
// --name is true; --name=<value> is parsed as a boolean, and a value that
// does not parse counts as present, since it is a token still being
// typed. A flag typed more than once takes its last spelling, as the
// command's parser reads it.
func boolFlagSet(tail []string, name string) bool {
	long := "--" + name
	set := false
	for _, a := range tail {
		if a == long {
			set = true
			continue
		}
		if v, ok := strings.CutPrefix(a, long+"="); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				set = b
			} else {
				set = true
			}
		}
	}
	return set
}
