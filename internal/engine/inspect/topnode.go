package inspect

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// nodeMetricsGVR is podMetricsGVR's sibling (metrics.k8s.io/v1beta1 nodes),
// the sole consumer of which is this file.
var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

type topNodeCmd struct {
	clients engine.ClusterClientFactory
}

// NewTopNode builds `top node [name]`: cluster nodes joined with
// metrics.k8s.io node metrics. A metrics-server absence degrades every row's
// usage to nil plus one warning, never a command error.
func NewTopNode(clients engine.ClusterClientFactory) runtime.Command {
	return &topNodeCmd{clients: clients}
}

func (c *topNodeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "top node",
		Usage:       "Show node resource usage from metrics.k8s.io",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:       []runtime.ArgSpec{{Name: resources.FlagCluster, Type: "string"}},
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c *topNodeCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(c.clients, in, "top node"); err != nil {
		return runtime.Result{}, err
	}
	nodeKind, _ := kinds.Lookup("node")
	named := len(in.Args()) > 0 && in.Args()[0] != ""
	clusterID, _, _, _, err := resources.Route(ctx, c.clients, nodeKind, in, named)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}

	if named {
		return c.single(ctx, client, clusterID, nodeKind.GVR, in.Args()[0])
	}
	return c.list(ctx, client, clusterID, nodeKind.GVR)
}

func (c *topNodeCmd) single(ctx context.Context, client engine.ClusterClient, clusterID string, nodeGVR schema.GroupVersionResource, name string) (runtime.Result, error) {
	node, err := client.Get(ctx, nodeGVR, "", name)
	if err != nil {
		return runtime.Result{}, err
	}
	var warnings []string
	var usage map[string]any
	if m, merr := client.Get(ctx, nodeMetricsGVR, "", name); merr != nil {
		warnings = append(warnings, metricsUnavailableWarning(clusterID))
	} else {
		usage, _, _ = unstructured.NestedMap(m.Object, "usage")
	}
	return runtime.Result{
		Kind:     "NodeMetrics",
		Data:     nodeRows{{"node": node.Object, "usage": usage}},
		Warnings: warnings,
	}, nil
}

func (c *topNodeCmd) list(ctx context.Context, client engine.ClusterClient, clusterID string, nodeGVR schema.GroupVersionResource) (runtime.Result, error) {
	nodes, err := client.List(ctx, nodeGVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	var warnings []string
	usageByName := map[string]map[string]any{}
	if metrics, merr := client.List(ctx, nodeMetricsGVR, "", ""); merr != nil {
		warnings = append(warnings, metricsUnavailableWarning(clusterID))
	} else {
		for i := range metrics.Items {
			usage, _, _ := unstructured.NestedMap(metrics.Items[i].Object, "usage")
			usageByName[metrics.Items[i].GetName()] = usage
		}
	}
	rows := make(nodeRows, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := nodes.Items[i]
		rows = append(rows, map[string]any{"node": n.Object, "usage": usageByName[n.GetName()]})
	}
	return runtime.Result{Kind: "NodeMetrics", Data: rows, Warnings: warnings}, nil
}

// nodeRows is `top node`'s payload: one {"node", "usage"} map per row. It
// implements output.Tabular so `-o table` shows NAME/CPU/MEMORY, rendering
// "<none>" for a node with no metrics.
type nodeRows []map[string]any

func (n nodeRows) Columns() []string { return []string{"NAME", "CPU", "MEMORY"} }

func (n nodeRows) Rows() [][]string {
	out := make([][]string, 0, len(n))
	for _, row := range n {
		node, _ := row["node"].(map[string]any)
		name, _, _ := unstructured.NestedString(node, "metadata", "name")
		cpu, memory := "<none>", "<none>"
		if usage, ok := row["usage"].(map[string]any); ok && usage != nil {
			if v, _, _ := unstructured.NestedString(usage, "cpu"); v != "" {
				cpu = v
			}
			if v, _, _ := unstructured.NestedString(usage, "memory"); v != "" {
				memory = v
			}
		}
		out = append(out, []string{name, cpu, memory})
	}
	return out
}
