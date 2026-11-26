package complete

import (
	"context"
	"errors"
	"slices"

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
// management LIST. A downstream kind that sets SteveReadOnForbidden (the
// namespace kind) is two requests: the Kubernetes list and, after a 403, the
// Steve list.
func requirements(k kinds.Kind) (completion.Requirements, bool) {
	if !k.Completable || k.Policy == kinds.NSCluster {
		return completion.Requirements{}, false
	}
	if k.Scope == kinds.Downstream {
		allowance := completion.AllowanceOne
		if k.SteveReadOnForbidden {
			allowance = completion.AllowanceTwo
		}
		return completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: allowance}, true
	}
	return completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceOne}, true
}

// KindSource returns the generic live completion source for the kind whose
// Key is key: its objects' names, listed where the row says they live.
// Claim declines a key that names no Completable kind. The same source
// serves the kind's name positional on the generated verbs and, for the
// namespace and node kinds, the shared --namespace and --node flags. A kind
// with a Handle also describes each name by the fields of the row's
// Handle.Lookup (a display name); a kind without one gets no description. A
// management kind that sets SteveReadOnForbidden lists its objects through the
// Steve API for every caller, in one request. The downstream kind that sets it
// (namespace) lists through the Kubernetes API and reads the Steve API of the
// typed cluster only after a 403: an administrator makes one request.
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
// a path segment. A management kind that sets SteveReadOnForbidden is listed
// through the Steve API, which has the objects under data; the names are each
// object's metadata.name, never Steve's id, which is <namespace>/<name>. The
// downstream kind that sets it is listed through the Kubernetes API, and
// through the Steve API of the typed cluster only after the Kubernetes API
// responds with 403. The server's order is kept and a continue token is not followed.
func listNames(ctx context.Context, conn *completion.Connection, req completion.Request, k kinds.Kind) completion.Result {
	p, ok := listPath(req, k)
	if !ok {
		return completion.Result{}
	}
	list, ok := readItems(ctx, conn, req, k, p)
	if !ok {
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

// readItems reads the collection at p, the path listPath returned for k. A
// management kind that sets SteveReadOnForbidden is read through the Steve API
// only, and the others through the Kubernetes API only, except the downstream
// kind that sets it: after the Kubernetes API responds with 403, the Steve API
// of the typed cluster is read as the second request. Any other response, and a
// refusal from Steve, leaves nothing to offer. The Steve API lists a namespace
// the caller has no permission to get with a few fields only, and `get` of it
// by name fails with the Kubernetes API's error; the name positional therefore
// offers only the namespaces Steve lists in full, and a --namespace value
// offers every namespace Steve lists, because the caller has a role in each.
func readItems(ctx context.Context, conn *completion.Connection, req completion.Request, k kinds.Kind, p completion.Path) (completion.ItemList, bool) {
	if k.SteveReadOnForbidden && k.Scope == kinds.Management {
		return steveItems(ctx, conn, p)
	}
	var list completion.ItemList
	err := conn.GetJSON(ctx, p, "", &list)
	if err == nil {
		return list, true
	}
	if status, ok := errors.AsType[*completion.StatusError](err); !k.SteveReadOnForbidden || !ok || !status.Forbidden() {
		return completion.ItemList{}, false
	}
	steve, err := completion.SteveClusterCollectionPath(req.ClusterID, k.SteveType())
	if err != nil {
		return completion.ItemList{}, false
	}
	list, ok := steveItems(ctx, conn, steve)
	if ok && req.Flag == "" {
		list.Items = slices.DeleteFunc(list.Items, func(item map[string]any) bool { return !listedInFull(item) })
	}
	return list, ok
}

// listedInFull reports whether a namespace item of the Steve list is the whole
// object: every namespace the Kubernetes API returns in full carries the
// kubernetes.io/metadata.name label, and Steve's item of a namespace the
// caller cannot get does not.
func listedInFull(item map[string]any) bool {
	meta, _ := item["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	name, _ := labels[namespaceNameLabel].(string)
	return name != ""
}

// namespaceNameLabel is the label the Kubernetes API sets on every namespace
// with the namespace's name.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// steveItems reads the Steve collection at p, whose objects are under data.
func steveItems(ctx context.Context, conn *completion.Connection, p completion.Path) (completion.ItemList, bool) {
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if err := conn.GetJSON(ctx, p, "", &page); err != nil {
		return completion.ItemList{}, false
	}
	return completion.ItemList{Items: page.Data}, true
}

// listPath derives the collection path for k at the site, one arm per
// (scope, namespace policy) class, or reports false when a typed project
// scopes the kind out or the site gives it no namespace to list in. A
// management kind that sets SteveReadOnForbidden is listed at the Steve
// collection path of the typed --namespace, or of the row's default namespace
// when none is typed. Only a row with a workspace namespace has such a path;
// any other management row that sets SteveReadOnForbidden lists nothing. The
// downstream kind that sets it has the Kubernetes path: readItems reads the
// Steve API after a 403.
func listPath(req completion.Request, k kinds.Kind) (completion.Path, bool) {
	if k.Scope == kinds.Downstream && kinds.ProjectScopable(k) && req.Project != "" {
		return completion.Path{}, false
	}
	g, v, r := k.GVR.Group, k.GVR.Version, k.GVR.Resource
	ns := completion.TailFlag(req.Tail, "namespace")
	if k.SteveReadOnForbidden && k.Scope == kinds.Management {
		if k.Policy != kinds.NSWorkspace {
			return completion.Path{}, false
		}
		if ns == "" {
			ns = k.DefaultNamespace
		}
		return built(completion.SteveNamespacedCollectionPath(k.SteveType(), ns))
	}
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
// because both commands refuse a disabled ClusterRepo. It claims no other
// command's --repo and no other site, and a line with no --cluster typed
// completes nothing.
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
