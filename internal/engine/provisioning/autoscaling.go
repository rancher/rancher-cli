// autoscaling.go implements the prime-gated `cluster set-autoscaling` and
// `cluster clear-autoscaling` verbs: turning cluster-autoscaler management
// of a machine pool's quantity on and off.
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
// clusters use it constantly), so only these four verbs — never the kind
// row, never `cluster scale` — gate on it.
var clusterAutoscalingCapability = runtime.CapabilityRequirement{
	Tier:    runtime.CapabilityPrime,
	Feature: "cluster-autoscaling",
}

type setAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewSetAutoscaling builds `cluster set-autoscaling <cluster> --pool <name>
// --min <n> --max <m>` (mutating, prime): sets autoscalingMinSize/MaxSize on
// the named pool, mirroring the CRD's CEL client-side before the server
// round-trip.
func NewSetAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &setAutoscalingCmd{clients: clients}
}

func (setAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster set-autoscaling",
		Usage: "Turn on cluster-autoscaler management for a machine pool",
		Desc: "Sets spec.rkeConfig.machinePools[].autoscalingMinSize/autoscalingMaxSize on the named pool. " +
			"Mirrors the CRD's validation client-side — --min and --max are both required and must both be " +
			">= 0, min must be <= max, and min must be > 0 on a control-plane or etcd pool — so a bad " +
			"value is rejected with flag names before the server round-trip; the server remains the " +
			"authority. provisioningcluster is a Sensitive kind, so this is Destructive like every other " +
			"mutation on the same object — dry-run/apply --yes. From this point the pool's quantity is " +
			"managed by the cluster autoscaler, not the operator.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			{Name: "min", Type: "string", Required: true, Desc: "minimum node `count`"},
			{Name: "max", Type: "string", Required: true, Desc: "maximum node `count`"},
			clusterNamespaceFlag,
		},
		Examples:    []string{"rancher cluster set-autoscaling my-cluster --pool pool1 --min 2 --max 5 --apply --yes"},
		Destructive: "true",
		Network:     true,
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
					fmt.Sprintf("--min must be > 0 for pool %q: it carries a control-plane or etcd role", poolName))
			}
			pool["autoscalingMinSize"] = int64(min)
			pool["autoscalingMaxSize"] = int64(max)
			return []string{fmt.Sprintf("pool %q quantity is now managed by the cluster autoscaler", poolName)}, nil
		})
}

var _ runtime.MutatingCommand = (*setAutoscalingCmd)(nil)

type clearAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewClearAutoscaling builds `cluster clear-autoscaling <cluster> --pool
// <name>` (mutating, prime): returns the named pool to manual quantity. The
// patch is built from the fetched object — remove ops only for the fields
// actually present — so a half-set pool still clears cleanly, and a pool
// that already carries neither field succeeds as a no-op rather than
// surfacing a raw path-not-found error.
func NewClearAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &clearAutoscalingCmd{clients: clients}
}

func (clearAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster clear-autoscaling",
		Usage: "Return a machine pool to manual quantity",
		Desc: "Removes spec.rkeConfig.machinePools[].autoscalingMinSize/autoscalingMaxSize from the named " +
			"pool, built from the fetched object so a half-set pool still clears cleanly. A pool that " +
			"already carries neither field succeeds as a no-op, so a retry always converges. " +
			"provisioningcluster is a Sensitive kind, so this is Destructive like every other mutation on " +
			"the same object — dry-run/apply --yes.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "pool", Type: "string", Required: true, Desc: "machine pool `name`"},
			clusterNamespaceFlag,
		},
		Examples:    []string{"rancher cluster clear-autoscaling my-cluster --pool pool1 --apply --yes"},
		Destructive: "true",
		Network:     true,
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
				contextLine = fmt.Sprintf("pool %s was already manual; nothing to clear", poolName)
				return nil, nil
			}
			quantity, _, _ := unstructured.NestedInt64(pool, "quantity")
			contextLine = fmt.Sprintf("pool %s left at manual quantity %d", poolName, quantity)
			return nil, nil
		})
	if err != nil {
		return eff, exec, err
	}
	return eff, wrapWithContextLine(exec, contextLine), nil
}

var _ runtime.MutatingCommand = (*clearAutoscalingCmd)(nil)
