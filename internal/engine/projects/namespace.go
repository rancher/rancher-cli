package projects

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type createNamespaceCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateNamespace builds `create namespace <name> [--project <p>]
// [--cluster <c>]` (mutating): today namespaces are otherwise creatable only
// via `create resource -f`. With --project, the namespace is born a member
// (label + annotation set from birth) rather than needing a follow-up
// add-namespace.
func NewCreateNamespace(clients engine.ClusterClientFactory) runtime.Command {
	return &createNamespaceCmd{clients: clients}
}

func (createNamespaceCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create namespace",
		Usage:       "Create a namespace, optionally as a project member",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "project", Type: "string", Desc: "make the namespace a member of this project from birth (label + annotation set on create)"},
			{Name: "cluster", Type: "string"},
		},
		Examples:    []string{"rancher create namespace team-a --project p-x --cluster c-x --apply"},
		Destructive: "false",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c createNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	name := args[0]

	cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name},
	}}
	if projectFlag := in.String("project"); projectFlag != "" {
		projectID, perr := ResolveProject(ctx, c.clients, cluster, projectFlag)
		if perr != nil {
			return safety.Effect{}, nil, perr
		}
		SetMembership(obj, cluster, projectID)
	}

	client, err := c.clients.Cluster(ctx, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	nsKind, _ := kinds.Lookup("namespace")
	return engine.PlanCreate(ctx, client, nsKind.GVR, "", obj, cluster, "Namespace", nsKind.Sensitive)
}
