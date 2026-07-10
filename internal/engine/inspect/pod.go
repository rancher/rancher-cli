package inspect

import (
	"context"
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const flagTail = "tail"

const defaultTailLines = "50"

type podCmd struct {
	clients engine.ClusterClientFactory
}

// NewPod builds `inspect pod <name>`: the pod, its owner chain, its metrics,
// and the last --tail lines of every container's log, joined into one
// document. Metrics/log fetch failures degrade to warnings; the pod fetch
// itself is a command error.
func NewPod(clients engine.ClusterClientFactory) runtime.Command {
	return &podCmd{clients: clients}
}

func (c *podCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "inspect pod",
		Usage:       "Inspect a pod: owners, metrics, and container logs",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string"},
			{Name: resources.FlagNamespace, Type: "string", Required: true},
			{Name: flagTail, Type: "string"},
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

func (c *podCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if err := rejectProject(in, "inspect pod"); err != nil {
		return runtime.Result{}, err
	}
	podKind, _ := kinds.Lookup("pod")
	tailLines, terr := parseTail(in.String(flagTail))
	if terr != nil {
		return runtime.Result{}, terr
	}
	clusterID, ns, _, _, err := resources.Route(ctx, c.clients, podKind, in, true)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	name := in.Args()[0]
	pod, err := client.Get(ctx, podKind.GVR, ns, name)
	if err != nil {
		return runtime.Result{}, err
	}

	owners, warnings := ownerChain(ctx, client, ns, pod)

	var metrics map[string]any
	if m, merr := client.Get(ctx, podMetricsGVR, ns, name); merr != nil {
		warnings = append(warnings, metricsUnavailableWarning(clusterID))
	} else {
		metrics = m.Object
	}

	logs := map[string]string{}
	for _, cname := range containerNames(pod.Object) {
		out, lerr := client.Logs(ctx, ns, name, cname, tailLines)
		if lerr != nil {
			warnings = append(warnings, fmt.Sprintf("logs unavailable for container %s: %v", cname, lerr))
			continue
		}
		logs[cname] = out
	}

	return runtime.Result{
		Kind:     "PodInspection",
		Data:     podResult{Pod: pod.Object, Owners: owners, Metrics: metrics, Logs: logs},
		Warnings: warnings,
	}, nil
}

// parseTail defaults an empty --tail to 50 and rejects anything that is not a
// non-negative integer with a VALIDATION error.
func parseTail(s string) (int, error) {
	if s == "" {
		s = defaultTailLines
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--tail %q is not a valid non-negative integer", s)).
			WithHint("pass --tail <N>")
	}
	return n, nil
}

// podResult is the `inspect pod` payload; it implements output.Tabular so
// `-o table` renders one summary row.
type podResult struct {
	Pod     map[string]any    `json:"pod"`
	Owners  []map[string]any  `json:"owners"`
	Metrics map[string]any    `json:"metrics"`
	Logs    map[string]string `json:"logs"`
}

func (p podResult) Columns() []string { return []string{"NAME", "READY", "STATUS", "OWNER"} }

func (p podResult) Rows() [][]string {
	name, _, _ := unstructured.NestedString(p.Pod, "metadata", "name")
	phase, _, _ := unstructured.NestedString(p.Pod, "status", "phase")
	ready, total := readyCount(p.Pod)
	owner := ""
	if len(p.Owners) > 0 {
		top := p.Owners[len(p.Owners)-1]
		owner = fmt.Sprintf("%v/%v", top["kind"], top["name"])
	}
	return [][]string{{name, fmt.Sprintf("%d/%d", ready, total), phase, owner}}
}

func readyCount(pod map[string]any) (ready, total int) {
	statuses, _, _ := unstructured.NestedSlice(pod, "status", "containerStatuses")
	total = len(statuses)
	for _, s := range statuses {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if r, _, _ := unstructured.NestedBool(sm, "ready"); r {
			ready++
		}
	}
	return ready, total
}
