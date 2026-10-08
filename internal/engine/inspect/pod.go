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
	k, _ := kinds.Lookup("pod")
	return runtime.Descriptor{
		Name:    "inspect pod",
		Aliases: k.Aliases,
		Usage:   "Inspect a pod: owners, metrics, and container logs",
		Desc: degradedContractDesc +
			"Cluster resolution, the namespace, and the pod GET are required and hard-error. " +
			"Degradable sections: owners (partial, target = the canonical owner identity " +
			"apiVersion|kind|namespace|name|uid), metrics (unavailable), logs (partial, target = " +
			"container name, one entry per container whose log read failed).",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			resources.ClusterFlag(true),
			{Name: resources.FlagNamespace, Type: "string", Required: true, Desc: "`namespace` to operate in"},
			{Name: flagTail, Type: "string", Desc: "max log `lines` per container (default 50)"},
		},
		Auth:  true,
		Shape: runtime.ShapeTabular,
	}
}

func (c *podCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	podKind, _ := kinds.Lookup("pod")
	tailLines, terr := parseTail(in.String(flagTail))
	if terr != nil {
		return runtime.Result{}, terr
	}
	clusterID, ns, _, _, err := resources.Route(ctx, c.clients, podKind, in, resources.OpNamed, in.Args()[0])
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

	sec := newSections(ctx, sectionOwners, sectionMetrics, sectionLogs)

	owners, _, oerr := ownerChain(ctx, client, sec, sectionOwners, "", ns, pod)
	if oerr != nil {
		return runtime.Result{}, oerr
	}

	var metrics map[string]any
	if m, merr := client.Get(ctx, podMetricsGVR, ns, name); merr != nil {
		if herr := sec.Unavailable(sectionMetrics, merr); herr != nil {
			return runtime.Result{}, herr
		}
	} else {
		metrics = m.Object
	}

	tl := int64(tailLines)
	logs := map[string]string{}
	// seenContainers guards the accumulator's duplicate-(section,target)
	// rule against wire data: admission rejects a pod whose containers and
	// initContainers share a name, but this CLI reads unvalidated objects
	// through a proxy and must degrade, never crash, on one that does.
	seenContainers := map[string]bool{}
	for _, cname := range engine.ContainerNames(pod.Object) {
		if seenContainers[cname] {
			continue
		}
		seenContainers[cname] = true
		out, lerr := client.Logs(ctx, ns, name, engine.LogOptions{Container: cname, TailLines: &tl})
		if lerr != nil {
			// One entry per failed container: the other containers' logs
			// are still there, so the section is partial, not lost.
			if herr := sec.Partial(sectionLogs, cname, lerr); herr != nil {
				return runtime.Result{}, herr
			}
			continue
		}
		logs[cname] = out
	}

	degraded, warnings := sec.Finalize()
	return runtime.Result{
		Kind:     "PodInspection",
		Data:     podResult{Pod: pod.Object, Owners: owners, Metrics: metrics, Logs: logs, Degraded: degraded},
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
			fmt.Sprintf("--tail %q is not a valid non-negative integer", s))
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
	// Degraded names the sections this run could not read in full. Absent
	// when every collector succeeded.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
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
