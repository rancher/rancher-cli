// scale.go implements `cluster scale <cluster> --pool <name> --nodes <n>`
// (community): a direct, named-pool alternative to the array-index
// JSON-Patch a positional machinePools list otherwise forces on any caller
// that wants to change spec.rkeConfig.machinePools[].quantity.
package provisioning

import (
	"context"
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type scaleCmd struct {
	clients engine.ClusterClientFactory
}

// NewScale builds `cluster scale <cluster> --pool <name> --nodes <n>`
// (mutating): patches the named pool's quantity, refusing pre-Plan when the
// pool is autoscaled and not paused (the autoscaler would silently revert
// the edit) or when the cluster carries no machine pools at all.
func NewScale(clients engine.ClusterClientFactory) runtime.Command {
	return &scaleCmd{clients: clients}
}

func (scaleCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster scale",
		Usage: "Scale a machine pool's node count",
		Desc: "Patches spec.rkeConfig.machinePools[].quantity on the named pool, resolved by name to its " +
			"array index at patch-build time. Refuses when the pool carries autoscalingMinSize/MaxSize and " +
			"autoscaling is not paused: the server would accept the edit and apply it, but the cluster " +
			"autoscaler re-asserts its own count within its loop, silently reverting the change minutes " +
			"later - pause-autoscaling is the sanctioned override. Refuses on a cluster with no machine " +
			"pools (imported, custom, or local). provisioningcluster is a Sensitive kind, so every scale " +
			"is Destructive regardless of direction, the same as a generic patch on the same object - " +
			"dry-run/apply --yes. --nodes 0 is schema-legal on every pool, including a control-plane/etcd " +
			"pool; no invented floor.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			{Name: "nodes", Type: "string", Required: true, Desc: "target node `count`"},
			clusterNamespaceFlag,
		},
		Examples:    []string{"rancher cluster scale my-cluster --pool pool1 --nodes 3 --apply --yes"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c scaleCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	nodesStr := in.String("nodes")
	nodes, nerr := strconv.Atoi(nodesStr)
	if nerr != nil || nodes < 0 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--nodes %q is not a non-negative integer", nodesStr))
	}

	return planClusterFamilyMutation(ctx, c.clients, in, "cluster scale",
		func(cluster *unstructured.Unstructured, pool map[string]any, clusterName, poolName string) ([]string, error) {
			if poolAutoscaled(pool) && !autoscalingPaused(cluster) {
				return nil, autoscaledPoolGuardError(clusterName, poolName)
			}
			_, hadQuantity, _ := unstructured.NestedInt64(pool, "quantity")
			pool["quantity"] = int64(nodes)
			if !hadQuantity {
				return []string{fmt.Sprintf("pool %q had no recorded quantity; current count is unknown", poolName)}, nil
			}
			return nil, nil
		})
}

var _ runtime.MutatingCommand = (*scaleCmd)(nil)
