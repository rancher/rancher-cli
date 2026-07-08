package clusters

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var gvr = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}

type getCmd struct {
	clients engine.ClusterClientFactory
}

// NewGet builds `get cluster` (readOnly). Clusters are cluster-scoped on the
// management cluster.
func NewGet(clients engine.ClusterClientFactory) runtime.Command {
	return &getCmd{clients: clients}
}

func (getCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "get cluster",
		Usage:       "List clusters or show one",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Examples:    []string{"rancher get clusters", "rancher get cluster prod -o yaml"},
	}
}

func (c getCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	var items []unstructured.Unstructured
	kind := "ClusterList"
	if args := in.Args(); len(args) > 0 && args[0] != "" {
		one, gerr := client.Get(ctx, gvr, "", args[0])
		if gerr != nil {
			return runtime.Result{}, gerr
		}
		items = []unstructured.Unstructured{*one}
		kind = "Cluster"
	} else {
		list, lerr := client.List(ctx, gvr, "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		items = list.Items
	}
	return runtime.Result{Kind: kind, Data: table(items)}, nil
}

// table projects clusters to NAME/DISPLAY/STATE for -o table and -o name.
type table []unstructured.Unstructured

func (table) Columns() []string { return []string{"NAME", "DISPLAY", "STATE"} }
func (t table) Rows() [][]string {
	rows := make([][]string, 0, len(t))
	for i := range t {
		o := t[i].Object
		display, _, _ := unstructured.NestedString(o, "spec", "displayName")
		state, _, _ := unstructured.NestedString(o, "status", "state")
		rows = append(rows, []string{t[i].GetName(), display, state})
	}
	return rows
}
