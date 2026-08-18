package inspect

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type deploymentCmd struct {
	clients engine.ClusterClientFactory
}

// NewDeployment builds `inspect deployment <name>`: the deployment plus the
// pods matched by its full spec.selector (matchLabels AND matchExpressions).
func NewDeployment(clients engine.ClusterClientFactory) runtime.Command {
	return &deploymentCmd{clients: clients}
}

func (c *deploymentCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("deployment")
	return runtime.Descriptor{
		Name:        "inspect deployment",
		Aliases:     k.Aliases,
		Usage:       "Inspect a deployment and the pods it selects",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string", Required: true, Desc: "target cluster `id|name`"},
			{Name: resources.FlagNamespace, Type: "string", Required: true, Desc: "`namespace` to operate in"},
		},
		Network: true,
		Auth:    true,
	}
}

func (c *deploymentCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(in, "inspect deployment"); err != nil {
		return runtime.Result{}, err
	}
	depKind, _ := kinds.Lookup("deployment")
	clusterID, ns, _, _, err := resources.Route(ctx, c.clients, depKind, in, true)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	name := in.Args()[0]
	dep, err := client.Get(ctx, depKind.GVR, ns, name)
	if err != nil {
		return runtime.Result{}, err
	}
	selector, err := engine.SelectorFrom(dep)
	if err != nil {
		return runtime.Result{}, err
	}
	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, ns, selector)
	if err != nil {
		return runtime.Result{}, err
	}
	pods := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, list.Items[i].Object)
	}
	return runtime.Result{
		Kind: "DeploymentInspection",
		Data: deploymentResult{Deployment: dep.Object, Pods: pods},
	}, nil
}

type deploymentResult struct {
	Deployment map[string]any   `json:"deployment"`
	Pods       []map[string]any `json:"pods"`
}
