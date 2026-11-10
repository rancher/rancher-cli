package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// requirements is the declaration a kind's generic derivation makes: a
// downstream kind lists through the typed cluster's proxy with whatever
// credential the caller has; a management kind lists through the
// management cluster's proxy with a management token. The second value is
// false for a kind that does not complete, and for a kind whose namespace
// is the cluster (a project): its names come from the projects the caller
// can read, which its own source lists (projects/complete), never from the
// management LIST.
func requirements(k kinds.Kind) (completion.Requirements, bool) {
	if !k.Completable || k.Policy == kinds.NSCluster {
		return completion.Requirements{}, false
	}
	if k.Scope == kinds.Downstream {
		return completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}, true
	}
	return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}, true
}

// KindSource returns the generic live completion source for the kind whose
// Key is key: its objects' names, listed where the row says they live.
// Claim declines a key that names no Completable kind. The same source
// serves the kind's name positional on the generated verbs and, for the
// namespace and node kinds, the shared --namespace and --node flags. A kind
// with a Handle also describes each name by the fields of the row's
// Handle.Lookup (a display name); a kind without one gets no description.
func KindSource(key string) completion.Source {
	k, known := kinds.ByKey(key)
	return completion.Source{
		Claim: func(completion.Request) (completion.Requirements, bool) {
			if !known {
				return completion.Requirements{}, false
			}
			return requirements(k)
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			if !known {
				return completion.Result{}
			}
			return listNames(ctx, conn, req, k)
		},
	}
}

// NamespaceSource is the shared --namespace source: the typed cluster's
// namespaces, and nothing under a typed --project, whose scope already
// names the namespaces a command may reach.
func NamespaceSource() completion.Source { return KindSource("namespace") }

// NodeSource is the shared --node source: the typed cluster's nodes.
func NodeSource() completion.Source { return KindSource("node") }

// listNames lists k's objects for the site and answers their names. A
// downstream kind under a typed --project answers nothing when the project
// already scopes it; a namespaced kind needs --namespace typed (or the
// row's default namespace for a workspace kind) and the namespace must be
// a path segment.
func listNames(ctx context.Context, conn *completion.Connection, req completion.Request, k kinds.Kind) completion.Result {
	p, ok := listPath(req, k)
	if !ok {
		return completion.Result{}
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	paths := k.DescriptionPaths()
	if len(paths) == 0 {
		return completion.Result{Names: list.Names()}
	}
	var names []string
	descriptions := map[string]string{}
	for _, item := range list.Items {
		name := completion.ItemName(item)
		if name == "" {
			continue
		}
		names = append(names, name)
		if _, ok := descriptions[name]; !ok {
			if d := completion.ItemDescription(item, paths); d != "" {
				descriptions[name] = d
			}
		}
	}
	return completion.Result{Names: names, Descriptions: descriptions}
}

// listPath derives the collection path for k at the site, one arm per
// (scope, namespace policy) class, or reports false when a typed project
// scopes the kind out or the site gives it no namespace to list in.
func listPath(req completion.Request, k kinds.Kind) (completion.Path, bool) {
	if k.Scope == kinds.Downstream && kinds.ProjectScopable(k) && req.Project != "" {
		return completion.Path{}, false
	}
	g, v, r := k.GVR.Group, k.GVR.Version, k.GVR.Resource
	ns := completion.TailFlag(req.Tail, "namespace")
	switch {
	case k.Scope == kinds.Downstream && k.Policy == kinds.NSNone:
		return built(completion.ClusterPath(req.ClusterID, g, v, r))
	case k.Scope == kinds.Downstream:
		if ns == "" {
			return completion.Path{}, false
		}
		return built(completion.NamespacedPath(req.ClusterID, g, v, ns, r))
	case k.Policy == kinds.NSNone:
		return built(completion.ManagementPath(g, v, r))
	case k.Policy == kinds.NSWorkspace && ns == "":
		return built(completion.ManagementNamespacedPath(g, v, k.DefaultNamespace, r))
	default:
		if ns == "" {
			return completion.Path{}, false
		}
		return built(completion.ManagementNamespacedPath(g, v, ns, r))
	}
}

// built turns a builder's result into listPath's answer: a rejected
// segment is a site that lists nothing.
func built(p completion.Path, err error) (completion.Path, bool) {
	return p, err == nil
}

// ChartsCommand is the registered name of the command ClusterRepoSource
// answers for.
const ChartsCommand = "charts"

// ClusterRepoSource is ClusterRepoSourceFor the charts command.
func ClusterRepoSource() completion.Source { return ClusterRepoSourceFor(ChartsCommand) }

// ClusterRepoSourceFor is the live source of the --repo flag of command, a
// registered command name (charts, app install): the names of the typed cluster's ClusterRepos whose
// spec.enabled is not false (a missing spec.enabled counts as enabled),
// because Rancher answers a read of a disabled ClusterRepo's index as not
// found. It claims no other command's --repo and no other site, and a line
// with no --cluster typed completes nothing.
func ClusterRepoSourceFor(command string) completion.Source {
	k, known := kinds.ByKey("clusterrepo")
	return completion.Source{
		Claim: func(req completion.Request) (completion.Requirements, bool) {
			if !known || req.Command != command || req.Flag != "repo" {
				return completion.Requirements{}, false
			}
			return requirements(k)
		},
		Complete: func(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
			if !known || req.ClusterID == "" {
				return completion.Result{}
			}
			p, ok := listPath(req, k)
			if !ok {
				return completion.Result{}
			}
			var list completion.ItemList
			if err := conn.GetJSON(ctx, p, "", &list); err != nil {
				return completion.Result{}
			}
			var names []string
			for _, item := range list.Items {
				spec, _ := item["spec"].(map[string]any)
				if enabled, ok := spec["enabled"].(bool); ok && !enabled {
					continue
				}
				if name := completion.ItemName(item); name != "" {
					names = append(names, name)
				}
			}
			return completion.Result{Names: names}
		},
	}
}
