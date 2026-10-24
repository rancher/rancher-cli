package inspect

import (
	"context"
	"fmt"
	"slices"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
		Name:    "inspect deployment",
		Aliases: k.Aliases,
		Usage:   "Inspect a deployment and the pods it selects",
		Desc: degradedContractDesc +
			"Finding the cluster by id or name, giving --namespace, and reading the deployment are required: " +
			"the command fails when any of them fails. " +
			"Section that can degrade: pods (unavailable - the list of pods the deployment's selector matches; " +
			"the pods field is null).",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, true),
			engine.NamespaceFlag(true),
		},
		Auth:  true,
		Shape: runtime.ShapeText,
	}
}

func (c *deploymentCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	depKind, _ := kinds.Lookup("deployment")
	clusterID, ns, _, _, err := resources.Route(ctx, c.clients, depKind, in, resources.OpNamed, in.Args()[0])
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
	sec := newSections(ctx, sectionPods)
	podKind, _ := kinds.Lookup("pod")

	// The deployment itself is readable; a denied pod list nulls that
	// section rather than losing the whole document.
	var pods []map[string]any
	list, lerr := client.List(ctx, podKind.GVR, ns, selector)
	if lerr != nil {
		if herr := sec.Unavailable(sectionPods, lerr); herr != nil {
			return runtime.Result{}, herr
		}
	} else {
		pods = make([]map[string]any, 0, len(list.Items))
		for i := range list.Items {
			pods = append(pods, list.Items[i].Object)
		}
	}

	degraded, warnings := sec.Finalize()
	res := renderDeploymentText(name, ns, clusterID, dep.Object, pods, degraded)
	res.Kind = "DeploymentInspection"
	res.Data = deploymentResult{Deployment: dep.Object, Pods: pods, Degraded: degraded}
	res.Warnings = warnings
	return res, nil
}

type deploymentResult struct {
	Deployment map[string]any   `json:"deployment"`
	Pods       []map[string]any `json:"pods"`
	// Degraded names the sections this run could not read in full. Absent
	// when every collector succeeded.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
}

// renderDeploymentText builds `inspect deployment`'s default-format text
// (sibling of inspect cluster/pod's own text renderers): the deployment
// identity, the replica summary, one line per matched pod in list order -
// "pods: unavailable" when the pods section is degraded (the list could not
// be read; "none" would be indistinguishable from a selector matching zero
// pods), "pods: none" only for a genuine empty match - and one line per
// degraded finding (the block is omitted entirely when there are none).
func renderDeploymentText(name, ns, cluster string, dep map[string]any, pods []map[string]any, degraded []sectionOutcome) runtime.Result {
	ready, _, _ := unstructured.NestedInt64(dep, "status", "readyReplicas")
	desired, _, _ := unstructured.NestedInt64(dep, "spec", "replicas")

	podsUnavailable := slices.ContainsFunc(degraded, func(f sectionOutcome) bool {
		return f.Section == sectionPods && f.State == stateUnavailable
	})

	var lines []string
	lines = append(lines, fmt.Sprintf("replicas: %d/%d ready", ready, desired))
	switch {
	case podsUnavailable:
		lines = append(lines, "pods: unavailable")
	case len(pods) == 0:
		lines = append(lines, "pods: none")
	default:
		for _, p := range pods {
			podName, _, _ := unstructured.NestedString(p, "metadata", "name")
			phase, _, _ := unstructured.NestedString(p, "status", "phase")
			lines = append(lines, fmt.Sprintf("pod %s: %s", podName, phase))
		}
	}
	for _, f := range degraded {
		finding := f.Section + " " + f.State
		if f.Target != "" {
			finding += " for " + f.Target
		}
		finding += " (" + f.Code + ")"
		lines = append(lines, "degraded: "+finding)
	}
	return runtime.TextLines(fmt.Sprintf("deployment/%s (namespace %s, cluster %s)", name, ns, cluster), lines...)
}
