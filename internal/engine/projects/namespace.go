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
		Usage:       "Create a namespace, optionally inside a project",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			runtime.ProjectFlag(false, "add the namespace to this project when it is created"),
			engine.ClusterFlag(true, false),
		},
		Examples:    []string{"rancher create namespace team-a --project p-x --cluster c-x --apply"},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c createNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required")
	}
	name := args[0]

	cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, false)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name},
	}}
	p, scoped, err := runtime.TypedProjectOn(ctx, in, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if scoped {
		SetMembership(obj, cluster, p.ID)
	}

	client, err := c.clients.Cluster(ctx, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	nsKind, _ := kinds.Lookup("namespace")
	return engine.PlanCreate(ctx, client, nsKind.GVR, "", obj, cluster, nsKind.Name, "Namespace", nsKind.Sensitive)
}
