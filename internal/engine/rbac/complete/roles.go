package complete

import (
	"context"
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

// RoleSource is the source grant and revoke hand the surface. It claims
// the --role value site alone (no cluster, a management credential, one
// listing) and answers the role names of the scope typed on the line;
// every other site of the two commands is declined.
func RoleSource() completion.Source {
	return completion.Source{Claim: claimRole, Complete: completeRole}
}

func claimRole(req completion.Request) (completion.Requirements, bool) {
	if req.Flag != "role" {
		return completion.Requirements{}, false
	}
	return completion.Requirements{
		Cluster:    completion.ClusterNone,
		Credential: completion.CredentialManagement,
		Allowance:  completion.AllowanceOne,
	}, true
}

// completeRole lists the scope's role collection once and keeps the names
// whose context is the scope's. The filter is client-side: the
// RoleTemplate CRD declares no selectable field for context, so a field
// selector cannot express it. No scope, or a conflicting one, answers
// nothing without a request.
func completeRole(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
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
	var names []string
	for _, item := range list.Items {
		if roleContext != "" {
			if c, _ := item["context"].(string); c != roleContext {
				continue
			}
		}
		if name := completion.ItemName(item); name != "" {
			names = append(names, name)
		}
	}
	return completion.Result{Names: completion.Dedupe(names)}
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
// typed.
func boolFlagSet(tail []string, name string) bool {
	long := "--" + name
	for _, a := range tail {
		if a == long {
			return true
		}
		if v, ok := strings.CutPrefix(a, long+"="); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				return b
			}
			return true
		}
	}
	return false
}
