// pause.go implements the prime-gated `cluster pause-autoscaling` and
// `cluster resume-autoscaling` verbs: the cluster-wide switch on the
// provisioning Cluster's pause annotation.
package provisioning

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type pauseAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewPauseAutoscaling builds `cluster pause-autoscaling <cluster>`
// (mutating, prime): sets the pause annotation. No --pool flag - pause is
// cluster-wide only, since no per-pool pause exists server-side.
func NewPauseAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &pauseAutoscalingCmd{clients: clients}
}

func (pauseAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster pause-autoscaling",
		Usage: "Pause the cluster autoscaler cluster-wide",
		Desc: "Sets the provisioning.cattle.io/cluster-autoscaler-paused annotation on the provisioning cluster. " +
			"A pause applies to the whole cluster: the server has no per-pool pause, and this command has no " +
			"--pool flag. Pausing scales the cluster autoscaler Deployment in the downstream cluster to zero " +
			"and keeps every machine pool's autoscalingMinSize and autoscalingMaxSize; while paused, " +
			"`rancher cluster scale` can change the number of nodes of an autoscaled machine pool. If autoscaling is " +
			"already paused, the command succeeds and changes nothing. Like every change to a provisioning " +
			"cluster, it requires --yes with --apply.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       []runtime.ArgSpec{clusterNamespaceFlag()},
		Examples:    []string{"rancher cluster pause-autoscaling my-cluster --apply --yes"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
		Capability:  clusterAutoscalingCapability,
	}
}

func (c pauseAutoscalingCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return planAutoscalerPause(ctx, c.clients, in, true)
}

var _ runtime.MutatingCommand = (*pauseAutoscalingCmd)(nil)

type resumeAutoscalingCmd struct {
	clients engine.ClusterClientFactory
}

// NewResumeAutoscaling builds `cluster resume-autoscaling <cluster>`
// (mutating, prime): clears the pause annotation.
func NewResumeAutoscaling(clients engine.ClusterClientFactory) runtime.Command {
	return &resumeAutoscalingCmd{clients: clients}
}

func (resumeAutoscalingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "cluster resume-autoscaling",
		Usage: "Resume the cluster autoscaler cluster-wide",
		Desc: "Removes the provisioning.cattle.io/cluster-autoscaler-paused annotation from the provisioning " +
			"cluster. The cluster autoscaler then manages again every machine pool that has " +
			"autoscalingMinSize and autoscalingMaxSize set. If autoscaling is not paused, the command " +
			"succeeds and changes nothing. Like every change to a provisioning cluster, it requires " +
			"--yes with --apply.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true, Desc: "provisioning cluster `name`"}},
		Flags:       []runtime.ArgSpec{clusterNamespaceFlag()},
		Examples:    []string{"rancher cluster resume-autoscaling my-cluster --apply --yes"},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
		Capability:  clusterAutoscalingCapability,
	}
}

func (c resumeAutoscalingCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return planAutoscalerPause(ctx, c.clients, in, false)
}

var _ runtime.MutatingCommand = (*resumeAutoscalingCmd)(nil)

// planAutoscalerPause implements both pause and resume: set or clear the
// pause annotation, succeeding and changing nothing when the cluster is
// already in the target state - the same retry contract clear-autoscaling
// gives an agent that finds nothing left to do.
func planAutoscalerPause(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input, pause bool) (safety.Effect, runtime.ExecuteFunc, error) {
	client, obj, ns, name, err := fetchClusterFamilyTarget(ctx, clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	already := autoscalingPaused(obj)
	after := obj.DeepCopy()
	if pause {
		if err := unstructured.SetNestedField(after.Object, "true", "metadata", "annotations", clusterAutoscalerPausedAnnotation); err != nil {
			return safety.Effect{}, nil, err
		}
	} else {
		unstructured.RemoveNestedField(after.Object, "metadata", "annotations", clusterAutoscalerPausedAnnotation)
	}
	after.SetResourceVersion(obj.GetResourceVersion())

	diff := engine.DiffObjects(obj.Object, after.Object)
	eff, exec, err := engine.PlanUpdate(ctx, client, provisioningClusterGVR, ns, after, provisioningClusterRedaction(), engine.LocalCluster, provisioningClusterDisplayKind, "Cluster", diff, provisioningClusterSensitive())
	if err != nil {
		return eff, exec, err
	}

	var contextLine string
	switch {
	case pause && already:
		contextLine = fmt.Sprintf("cluster autoscaler already paused for %s", name)
	case !pause && !already:
		contextLine = fmt.Sprintf("cluster autoscaler not paused for %s; nothing to resume", name)
	case pause:
		contextLine = fmt.Sprintf("cluster autoscaler paused for %s", name)
	default:
		contextLine = fmt.Sprintf("cluster autoscaler resumed for %s", name)
	}
	return eff, wrapWithContextLine(exec, contextLine), nil
}
