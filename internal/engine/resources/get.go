// Package resources holds generic verbs (get, delete) driven by the kind
// table: one command implementation per verb, parameterized by a kinds.Kind.
package resources

import (
	"context"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type getCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
}

// NewGet builds `get <kind> [name]` (readOnly) from a Kind: the Descriptor,
// scope-based flags, and cluster/namespace routing all derive from the table
// row.
func NewGet(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return &getCmd{k: k, clients: clients}
}

func (c getCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "get " + c.k.Name,
		Aliases:     c.k.Aliases,
		Usage:       "List " + c.k.Name + "s or show one",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:       getFlags(c.k),
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// getFlags declares the routing flags a kind's namespace policy needs:
//   - NSCluster: --cluster (it becomes the namespace).
//   - NSWorkspace: --namespace only (client is always management).
//   - NSPlain: --namespace, plus --cluster when the kind is Downstream.
//   - NSNone: --cluster only when the kind is Downstream; nothing otherwise.
func getFlags(k kinds.Kind) []runtime.ArgSpec {
	switch k.Policy {
	case kinds.NSCluster:
		return []runtime.ArgSpec{{Name: "cluster", Type: "string"}}
	case kinds.NSWorkspace:
		return []runtime.ArgSpec{{Name: "namespace", Type: "string"}}
	case kinds.NSPlain:
		if k.Scope == kinds.Downstream {
			return []runtime.ArgSpec{{Name: "cluster", Type: "string"}, {Name: "namespace", Type: "string"}}
		}
		return []runtime.ArgSpec{{Name: "namespace", Type: "string"}}
	default: // NSNone
		if k.Scope == kinds.Downstream {
			return []runtime.ArgSpec{{Name: "cluster", Type: "string"}}
		}
		return nil
	}
}

func (c getCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, ns, _, err := route(ctx, c.clients, c.k, in, hasName(in))
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	title := kindTitle(c.k.Name)
	if hasName(in) {
		one, gerr := client.Get(ctx, c.k.GVR, ns, in.Args()[0])
		if gerr != nil {
			return runtime.Result{}, gerr
		}
		return runtime.Result{Kind: title, Data: newTable(c.k, []unstructured.Unstructured{*one})}, nil
	}
	list, lerr := client.List(ctx, c.k.GVR, ns)
	if lerr != nil {
		return runtime.Result{}, lerr
	}
	return runtime.Result{Kind: title + "List", Data: newTable(c.k, list.Items)}, nil
}

func hasName(in runtime.Input) bool {
	args := in.Args()
	return len(args) > 0 && args[0] != ""
}

// route maps a kind's NamespacePolicy + input to the client cluster id, the
// namespace, and the Effect's target cluster (the identity a mutation lands on,
// which differs from the client cluster for management namespaced kinds):
//
//	NSNone:      client per scope,     ns "",                 effect = client
//	NSCluster:   client "local",       ns = resolved --cluster, effect that cluster
//	NSWorkspace: client "local",       ns = --namespace|Default, effect "local"
//	NSPlain:     client per scope,     ns = --namespace,      effect = client
//
// "per scope" (NSNone, NSPlain): a Management kind is served by the local proxy
// ("local"); a Downstream kind resolves --cluster (name-or-id) via
// ResolveCluster, falling back to the context cluster, else a VALIDATION error.
//
// SAFETY (review F3) — for NSCluster kinds an empty --cluster must NEVER
// silently become "local" (ResolveCluster("") returns LocalCluster, which as a
// namespace targets the local cluster's own resources): an empty --cluster is
// allowed only for a nameless list (namespace "" = across all clusters);
// addressing a resource by name requires --cluster. NSPlain applies the same
// fail-fast rule to --namespace before any network resolution.
//
// route is shared by get and delete: delete always passes named=true and uses
// the effect cluster to stamp Effect.Cluster.
func route(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, named bool) (clientCluster, ns, effectCluster string, err error) {
	switch k.Policy {
	case kinds.NSCluster:
		clusterFlag := in.String("cluster")
		if clusterFlag == "" {
			if named {
				return "", "", "", clierr.New(clierr.CodeValidation,
					"--cluster is required to address a "+k.Name+" by name").
					WithHint("pass --cluster <id|name>")
			}
			return engine.LocalCluster, "", "", nil // list across all clusters
		}
		id, rerr := clients.ResolveCluster(ctx, clusterFlag)
		if rerr != nil {
			return "", "", "", rerr
		}
		// The namespace IS the cluster id; the Effect targets that cluster.
		return engine.LocalCluster, id, id, nil

	case kinds.NSWorkspace:
		// A management-plane namespace with a default; --cluster is never
		// consulted and a missing --namespace falls back to the default, so a
		// named op never fails validation here.
		nspace := in.String("namespace")
		if nspace == "" {
			nspace = k.DefaultNamespace
		}
		return engine.LocalCluster, nspace, engine.LocalCluster, nil

	case kinds.NSPlain:
		// Namespace is a pure input requirement — check it before any network
		// resolution so a missing --namespace fails fast. A named op with an
		// empty --namespace would pass ns="" to the API and 404 opaquely; a
		// nameless list keeps ns="" (all-namespaces), which is valid and useful.
		nspace := in.String("namespace")
		if named && nspace == "" {
			return "", "", "", clierr.New(clierr.CodeValidation,
				"--namespace is required to address a "+k.Name+" by name").
				WithHint("pass --namespace <namespace>")
		}
		return routeCluster(ctx, clients, k, in, nspace)

	default: // NSNone — cluster-scoped, no namespace
		return routeCluster(ctx, clients, k, in, "")
	}
}

// routeCluster picks the serving cluster for the scope-driven policies (NSNone,
// NSPlain): a Management kind is served by the local proxy; a Downstream kind
// resolves --cluster (falling back to the context cluster, else VALIDATION).
// ns passes through unchanged and the Effect targets the serving cluster.
func routeCluster(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, ns string) (clientCluster, nsOut, effectCluster string, err error) {
	if k.Scope == kinds.Management {
		return engine.LocalCluster, ns, engine.LocalCluster, nil
	}
	target := in.String("cluster")
	if target == "" {
		cc, cerr := clients.ContextCluster()
		if cerr != nil {
			return "", "", "", cerr
		}
		target = cc
	}
	if target == "" {
		return "", "", "", clierr.New(clierr.CodeValidation,
			"--cluster is required (no cluster set in the current context)").
			WithHint("pass --cluster <id|name>")
	}
	id, rerr := clients.ResolveCluster(ctx, target)
	if rerr != nil {
		return "", "", "", rerr
	}
	return id, ns, id, nil
}

// kindTitle upper-cases the kind's first rune for the Result envelope Kind
// ("cluster" → "Cluster", "pod" → "Pod").
func kindTitle(name string) string {
	if name == "" {
		return ""
	}
	r := []rune(name)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}
