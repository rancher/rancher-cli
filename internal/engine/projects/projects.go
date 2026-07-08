package projects

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var gvr = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

type createCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreate builds `create project <name> --cluster <id>` (mutating). The Project
// is a management.cattle.io/v3 resource in the cluster-id namespace on the
// management cluster (proxy "local"). Uses an explicit metadata.name (not
// generateName) so Plan's dry-run and Execute address the same object.
func NewCreate(clients engine.ClusterClientFactory) runtime.Command {
	return &createCmd{clients: clients}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create project",
		Usage:       "Create a project in a cluster",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true},
			{Name: "description", Type: "string"},
		},
		Examples: []string{"rancher create project web --cluster c-x --dry-run", "rancher create project web --cluster c-x --apply"},
	}
}

// build validates input and constructs the Project object + namespace.
func (createCmd) build(in runtime.Input) (*unstructured.Unstructured, string, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, "", clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	cluster := in.String("cluster")
	if cluster == "" {
		return nil, "", clierr.New(clierr.CodeValidation, "--cluster is required").WithHint("pass --cluster <id>")
	}
	name := args[0]
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Project",
		"metadata":   map[string]any{"name": name, "namespace": cluster},
		"spec":       map[string]any{"clusterName": cluster, "displayName": name},
	}}
	if desc := in.String("description"); desc != "" {
		_ = unstructured.SetNestedField(obj.Object, desc, "spec", "description")
	}
	return obj, cluster, nil
}

func (c createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, cluster, err := c.build(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// The write goes through the management proxy (LocalCluster) because
	// Project is a management.cattle.io resource namespaced by cluster ID,
	// but the Effect targets the downstream cluster the project belongs to.
	return engine.PlanCreate(ctx, client, gvr, cluster, obj, cluster, "Project")
}
