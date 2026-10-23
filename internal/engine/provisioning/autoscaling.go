// autoscaling.go implements the prime-gated `cluster set-autoscaling` and
// `cluster clear-autoscaling` verbs: turning autoscaling of a machine pool
// on and off.
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

// clusterAutoscalingCapability is the static Prime requirement every
// autoscaling verb's Descriptor carries directly, with no backing kind row
// underneath: the provisioningcluster row stays community (community
// clusters use it constantly), so only these four verbs - never the kind
// row, never `cluster scale` - gate on it.
var clusterAutoscalingCapability = runtime.CapabilityRequirement{
	Tier:    runtime.CapabilityPrime,
	Feature: "cluster-autoscaling",
}

type setAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewSetAutoscaling builds `cluster set-autoscaling <cluster> --pool <name>
// --min <n> --max <m>` (mutating, prime): sets autoscalingMinSize and
// autoscalingMaxSize on the named machine pool, checking the CRD's CEL rules
// before it sends the change to the server.
func NewSetAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &setAutoscalingCmd{clients: clients}
}

func (setAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster set-autoscaling",
		Usage: "Turn on autoscaling for a machine pool",
		Desc: "Sets autoscalingMinSize and autoscalingMaxSize (spec.rkeConfig.machinePools[]) on the machine pool " +
			"named by --pool. Before it sends the change to the server, the command checks the CRD's validation " +
			"rules: --min and --max are both required, --min must be <= --max, and --min must be > 0 on a " +
			"control-plane or etcd machine pool. It also requires both to be whole numbers >= 0. An invalid " +
			"value is rejected with an error that names the flag. The server still validates the request. " +
			"Like every change to a provisioning cluster, it requires --yes with --apply. From then on the " +
			"cluster autoscaler sets the number of nodes of the machine pool, not you.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			{Name: "min", Type: "string", Required: true, Desc: "minimum `number` of nodes"},
			{Name: "max", Type: "string", Required: true, Desc: "maximum `number` of nodes"},
			clusterNamespaceFlag(),
		},
		Examples:    []string{"rancher cluster set-autoscaling my-cluster --pool pool1 --min 2 --max 5 --apply --yes"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
		Capability:  clusterAutoscalingCapability,
	}
}

func (c setAutoscalingCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	minStr, maxStr := in.String("min"), in.String("max")
	min, minErr := strconv.Atoi(minStr)
	max, maxErr := strconv.Atoi(maxStr)
	if minErr != nil || maxErr != nil {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--min %q and --max %q must both be integers", minStr, maxStr))
	}
	if min < 0 || max < 0 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--min %d and --max %d must both be >= 0", min, max))
	}
	if min > max {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--min %d must be <= --max %d", min, max))
	}

	return planClusterFamilyMutation(ctx, c.clients, in, "cluster set-autoscaling",
		func(cluster *unstructured.Unstructured, pool map[string]any, clusterName, poolName string) ([]string, error) {
			if min == 0 && poolRequiresPositiveMin(pool) {
				return nil, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--min must be > 0 for machine pool %q: it has a control-plane or etcd role", poolName))
			}
			pool["autoscalingMinSize"] = int64(min)
			pool["autoscalingMaxSize"] = int64(max)
			return []string{fmt.Sprintf("the cluster autoscaler now manages the number of nodes of machine pool %q", poolName)}, nil
		})
}

var _ runtime.MutatingCommand = (*setAutoscalingCmd)(nil)

type clearAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewClearAutoscaling builds `cluster clear-autoscaling <cluster> --pool
// <name>` (mutating, prime): turns off autoscaling for the named machine
// pool. The patch is built from the fetched object - remove operations only
// for the fields present - so a machine pool with only one of the two fields
// set is cleared too, and a machine pool with neither field succeeds and
// changes nothing rather than surfacing a raw path-not-found error.
func NewClearAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &clearAutoscalingCmd{clients: clients}
}

func (clearAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster clear-autoscaling",
		Usage: "Turn off autoscaling for a machine pool",
		Desc: "Removes the autoscalingMinSize and autoscalingMaxSize fields (spec.rkeConfig.machinePools[]) from the " +
			"machine pool named by --pool, also when only one of them is set. If the machine pool has neither " +
			"field, the command succeeds and changes nothing; running it again is safe. Like every change to a " +
			"provisioning cluster, it requires --yes with --apply.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			clusterNamespaceFlag(),
		},
		Examples:    []string{"rancher cluster clear-autoscaling my-cluster --pool pool1 --apply --yes"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
		Capability:  clusterAutoscalingCapability,
	}
}

func (c clearAutoscalingCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	var contextLine string
	eff, exec, err := planClusterFamilyMutation(ctx, c.clients, in, "cluster clear-autoscaling",
		func(cluster *unstructured.Unstructured, pool map[string]any, clusterName, poolName string) ([]string, error) {
			_, hadMin := pool["autoscalingMinSize"]
			_, hadMax := pool["autoscalingMaxSize"]
			delete(pool, "autoscalingMinSize")
			delete(pool, "autoscalingMaxSize")
			if !hadMin && !hadMax {
				contextLine = fmt.Sprintf("machine pool %s has no autoscaling; nothing to remove", poolName)
				return nil, nil
			}
			quantity, _, _ := unstructured.NestedInt64(pool, "quantity")
			contextLine = fmt.Sprintf("machine pool %s is no longer autoscaled; its number of nodes stays at %d", poolName, quantity)
			return nil, nil
		})
	if err != nil {
		return eff, exec, err
	}
	return eff, wrapWithContextLine(exec, contextLine), nil
}

var _ runtime.MutatingCommand = (*clearAutoscalingCmd)(nil)
