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
	}
}

// getFlags declares the routing flags a kind's scope needs.
func getFlags(k kinds.Kind) []runtime.ArgSpec {
	switch {
	case k.Scope == kinds.Downstream:
		flags := []runtime.ArgSpec{{Name: "cluster", Type: "string"}}
		if k.Namespaced {
			flags = append(flags, runtime.ArgSpec{Name: "namespace", Type: "string"})
		}
		return flags
	case k.Scope == kinds.Management && k.Namespaced:
		return []runtime.ArgSpec{{Name: "cluster", Type: "string"}}
	default:
		return nil
	}
}

func (c getCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, ns, err := c.route(ctx, in, hasName(in))
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

// route maps the kind's scope + input to the (client cluster id, namespace):
//
//	Management, cluster-scoped:  ("local", "")
//	Management, namespaced:      ("local", resolved --cluster)   // project: namespace IS the cluster id
//	Downstream, cluster-scoped:  (resolve(--cluster or context), "")
//	Downstream, namespaced:      (resolve(--cluster or context), --namespace)
//
// --cluster values pass through ResolveCluster (name-or-id). For Downstream
// kinds an empty --cluster falls back to the context cluster; if that is also
// empty it is a VALIDATION error.
//
// SAFETY (review F3) — for Management+Namespaced kinds an empty --cluster must
// NEVER silently become "local" (ResolveCluster("") returns LocalCluster,
// which as a namespace targets the local cluster's own resources): an empty
// --cluster is allowed only for a nameless list (namespace "" = across all
// clusters); getting by name requires --cluster.
func (c getCmd) route(ctx context.Context, in runtime.Input, named bool) (string, string, error) {
	clusterFlag := in.String("cluster")
	switch {
	case c.k.Scope == kinds.Management && !c.k.Namespaced:
		return engine.LocalCluster, "", nil

	case c.k.Scope == kinds.Management && c.k.Namespaced:
		if clusterFlag == "" {
			if named {
				return "", "", clierr.New(clierr.CodeValidation,
					"--cluster is required to address a "+c.k.Name+" by name").
					WithHint("pass --cluster <id|name>")
			}
			return engine.LocalCluster, "", nil // list across all clusters
		}
		id, err := c.clients.ResolveCluster(ctx, clusterFlag)
		if err != nil {
			return "", "", err
		}
		return engine.LocalCluster, id, nil

	default: // Downstream
		target := clusterFlag
		if target == "" {
			cc, err := c.clients.ContextCluster()
			if err != nil {
				return "", "", err
			}
			target = cc
		}
		if target == "" {
			return "", "", clierr.New(clierr.CodeValidation,
				"--cluster is required (no cluster set in the current context)").
				WithHint("pass --cluster <id|name>")
		}
		id, err := c.clients.ResolveCluster(ctx, target)
		if err != nil {
			return "", "", err
		}
		ns := ""
		if c.k.Namespaced {
			ns = in.String("namespace")
		}
		return id, ns, nil
	}
}

// kindTitle upper-cases the kind's first rune for the Result envelope Kind
// ("cluster" → "Cluster", "pod" → "Pod").
func kindTitle(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
