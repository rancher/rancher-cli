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
// (mutating): sets the named machine pool's quantity, refusing before the
// plan when the machine pool is autoscaled and autoscaling is not paused
// (the cluster autoscaler would set the number of nodes back) or when the
// cluster has no machine pools.
func NewScale(clients engine.ClusterClientFactory) runtime.Command {
	return &scaleCmd{clients: clients}
}

func (scaleCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster scale",
		Usage: "Change the number of nodes in a machine pool",
		Desc: "Sets spec.rkeConfig.machinePools[].quantity on the machine pool named by --pool. Refuses when " +
			"the machine pool has autoscalingMinSize or autoscalingMaxSize set and autoscaling is not " +
			"paused: the server accepts the change, but the cluster autoscaler sets the number of nodes " +
			"back to its own value minutes later. Run `rancher cluster pause-autoscaling` first. Refuses on a " +
			"cluster with no machine pools (imported, custom or local). Every scale, up or down, requires " +
			"--yes with --apply, like any change to a provisioning cluster. --nodes 0 is allowed on every " +
			"machine pool, including a control-plane or etcd machine pool; the CLI sets no minimum of its own.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			{Name: "nodes", Type: "string", Required: true, Desc: "target `number` of nodes"},
			clusterNamespaceFlag(),
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
			fmt.Sprintf("--nodes %q is not valid; it must be a whole number, 0 or more", nodesStr))
	}

	return planClusterFamilyMutation(ctx, c.clients, in, "cluster scale",
		func(cluster *unstructured.Unstructured, pool map[string]any, clusterName, poolName string) ([]string, error) {
			if poolAutoscaled(pool) && !autoscalingPaused(cluster) {
				return nil, autoscaledPoolGuardError(clusterName, poolName)
			}
			_, hadQuantity, _ := unstructured.NestedInt64(pool, "quantity")
			pool["quantity"] = int64(nodes)
			if !hadQuantity {
				return []string{fmt.Sprintf("machine pool %q has no quantity set; its current number of nodes is unknown", poolName)}, nil
			}
			return nil, nil
		})
}

var _ runtime.MutatingCommand = (*scaleCmd)(nil)
