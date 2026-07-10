package inspect

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type usageCmd struct {
	clients engine.ClusterClientFactory
}

// NewUsage builds `usage --cluster [--namespace] [--project]`: CPU/memory
// requests and limits from pod specs, plus usage from pod metrics, summed per
// namespace — or, under --project, summed across every namespace in that
// project into one bucket. --project has no local flag: the global
// --project flag reaches it via urfave lineage. A missing metrics server
// degrades every usage field to empty plus one warning.
func NewUsage(clients engine.ClusterClientFactory) runtime.Command {
	return &usageCmd{clients: clients}
}

func (c *usageCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "usage",
		Usage:       "Show CPU/memory requests, limits, and usage per namespace or project",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: resources.FlagCluster, Type: "string"},
			{Name: resources.FlagNamespace, Type: "string"},
		},
		Toolset: "core",
		Network: true,
		Auth:    true,
	}
}

func (c *usageCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	projectFlag := in.String(resources.FlagProject)
	clusterID, err := resources.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	podKind, _ := kinds.Lookup("pod")

	var projectID string
	var members map[string]bool
	if projectFlag != "" {
		projectID, err = projects.ResolveProject(ctx, c.clients, clusterID, projectFlag)
		if err != nil {
			return runtime.Result{}, err
		}
		members, err = projects.ProjectNamespaces(ctx, client, projectID)
		if err != nil {
			return runtime.Result{}, err
		}
	}

	grouped := map[string][]unstructured.Unstructured{}
	var namespaces []string // nil = cluster-wide; scopes the metrics fetch below
	switch {
	case namespace != "" && projectID != "":
		// PC-3's universal rule: --namespace + --project is allowed, provided
		// the namespace is a member, then behaves as plain namespace scope.
		if !members[namespace] {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("namespace %q is not in project %q", namespace, projectID)).
				WithHint("pass a member namespace, or omit --namespace to scope the whole project")
		}
		namespaces = []string{namespace}
		l, lerr := client.List(ctx, podKind.GVR, namespace, "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		grouped[namespace] = l.Items

	case projectID != "":
		if len(members) == 0 {
			return runtime.Result{}, clierr.New(clierr.CodeNotFound,
				fmt.Sprintf("no namespaces found for --project %q", projectFlag)).
				WithHint("check the project id")
		}
		var pods []unstructured.Unstructured
		for ns := range members {
			namespaces = append(namespaces, ns)
			l, lerr := client.List(ctx, podKind.GVR, ns, "")
			if lerr != nil {
				return runtime.Result{}, lerr
			}
			pods = append(pods, l.Items...)
		}
		grouped[projectID] = pods

	case namespace != "":
		namespaces = []string{namespace}
		l, lerr := client.List(ctx, podKind.GVR, namespace, "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		grouped[namespace] = l.Items

	default:
		l, lerr := client.List(ctx, podKind.GVR, "", "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		for _, pod := range l.Items {
			grouped[pod.GetNamespace()] = append(grouped[pod.GetNamespace()], pod)
		}
	}

	return aggregateUsage(ctx, client, clusterID, grouped, namespaces)
}

// fetchPodMetrics lists pod metrics scoped to namespaces (cluster-wide when
// nil, one List per namespace otherwise), so a --namespace/--project run
// never pulls metrics for the whole cluster to summarize a handful of pods.
func fetchPodMetrics(ctx context.Context, client engine.ClusterClient, namespaces []string) (map[string]*unstructured.Unstructured, error) {
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	byKey := map[string]*unstructured.Unstructured{}
	for _, ns := range scopes {
		list, err := client.List(ctx, podMetricsGVR, ns, "")
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			m := list.Items[i]
			byKey[m.GetNamespace()+"/"+m.GetName()] = &list.Items[i]
		}
	}
	return byKey, nil
}

// aggregateUsage sums CPU/memory requests, limits, and (when metrics.k8s.io
// is reachable) usage per grouped key, degrading a metrics fetch failure to
// a warning rather than a command error. Shared by usage's own per-namespace/
// per-project modes and inspect project's usage summary.
func aggregateUsage(ctx context.Context, client engine.ClusterClient, clusterID string, grouped map[string][]unstructured.Unstructured, namespaces []string) (runtime.Result, error) {
	var warnings []string
	metricsOK := true
	metricsByKey, merr := fetchPodMetrics(ctx, client, namespaces)
	if merr != nil {
		warnings = append(warnings, metricsUnavailableWarning(clusterID))
		metricsOK = false
	}

	result := usageRows{}
	for scope, pods := range grouped {
		var cpuReq, cpuLim, cpuUse, memReq, memLim, memUse resource.Quantity
		for _, pod := range pods {
			containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
			for _, item := range containers {
				cm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				addQuantity(&cpuReq, cm, "resources", "requests", "cpu")
				addQuantity(&cpuLim, cm, "resources", "limits", "cpu")
				addQuantity(&memReq, cm, "resources", "requests", "memory")
				addQuantity(&memLim, cm, "resources", "limits", "memory")
			}
			if !metricsOK {
				continue
			}
			m, ok := metricsByKey[pod.GetNamespace()+"/"+pod.GetName()]
			if !ok {
				continue
			}
			mcontainers, _, _ := unstructured.NestedSlice(m.Object, "containers")
			for _, item := range mcontainers {
				cm, ok := item.(map[string]any)
				if !ok {
					continue
				}
				addQuantity(&cpuUse, cm, "usage", "cpu")
				addQuantity(&memUse, cm, "usage", "memory")
			}
		}
		totals := resourceTotals{
			CPURequest: cpuReq.String(), CPULimit: cpuLim.String(),
			MemoryRequest: memReq.String(), MemoryLimit: memLim.String(),
		}
		if metricsOK {
			totals.CPUUsage = cpuUse.String()
			totals.MemoryUsage = memUse.String()
		}
		result[scope] = totals
	}
	return runtime.Result{Kind: "ResourceUsage", Data: result, Warnings: warnings}, nil
}

// addQuantity parses the string quantity at path within container (a
// map[string]any decoded from spec.containers[i] or a pod-metrics
// containers[i] entry) and adds it to q. A missing or unparsable value is a
// silent no-op — an absent request/limit/usage contributes zero.
func addQuantity(q *resource.Quantity, container map[string]any, path ...string) {
	v, found, _ := unstructured.NestedString(container, path...)
	if !found || v == "" {
		return
	}
	parsed, err := resource.ParseQuantity(v)
	if err != nil {
		return
	}
	q.Add(parsed)
}

type resourceTotals struct {
	CPURequest    string `json:"cpuRequest"`
	CPULimit      string `json:"cpuLimit"`
	CPUUsage      string `json:"cpuUsage,omitempty"`
	MemoryRequest string `json:"memoryRequest"`
	MemoryLimit   string `json:"memoryLimit"`
	MemoryUsage   string `json:"memoryUsage,omitempty"`
}

// usageRows is `usage`'s payload, keyed by namespace (or by project id under
// --project). It implements output.Tabular for `-o table`.
type usageRows map[string]resourceTotals

func (u usageRows) Columns() []string {
	return []string{"SCOPE", "CPU_REQUEST", "CPU_LIMIT", "CPU_USAGE", "MEMORY_REQUEST", "MEMORY_LIMIT", "MEMORY_USAGE"}
}

func (u usageRows) Rows() [][]string {
	keys := make([]string, 0, len(u))
	for k := range u {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][]string, 0, len(keys))
	for _, k := range keys {
		t := u[k]
		out = append(out, []string{k, t.CPURequest, t.CPULimit, t.CPUUsage, t.MemoryRequest, t.MemoryLimit, t.MemoryUsage})
	}
	return out
}
