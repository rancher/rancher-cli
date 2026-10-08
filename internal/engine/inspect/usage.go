package inspect

import (
	"context"
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
	roots   engine.RootClientFactory
}

const (
	byNamespace = "namespace"
	byProject   = "project"
	byNode      = "node"
	byPod       = "pod"
)

// NewUsage builds `usage --cluster [--namespace] [--project] [--by] [--node]`:
// CPU/memory requests and limits from pod specs, plus usage from pod metrics,
// grouped by the --by discriminator. A missing metrics server degrades every
// usage field to empty plus one warning. roots reads the cluster's projects
// for --by project.
func NewUsage(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &usageCmd{clients: clients, roots: roots}
}

func (c *usageCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "usage",
		Usage: "Show CPU/memory requests, limits, and usage for namespaces, projects, nodes, or pods",
		Desc: "Groups results by --by (namespace|project|node|pod). " +
			"--namespace, --project, and --node are filters that narrow every grouping: " +
			"--node keeps only pods scheduled on that node; --namespace with --project requires a project namespace. " +
			"--by node accepts only --node; combining --by node with --namespace or --project is a validation error. " +
			"--project without --by returns project-namespace rows (namespace grouping filtered to the project). " +
			"--by project --project X returns a single project bucket. " +
			degradedContractDesc +
			"Required collectors hard-error: cluster resolution and the pod list always; project " +
			"resolution and the membership list with --project; the namespace map with --by project, " +
			"and the project map with --by project and no --project; the node list with --by node, or the named node GET with --by node --node N. " +
			"Degradable section: metrics (unavailable - requests and limits stay). A scope that resolves " +
			"to zero namespaces is verified empty: no pod or metrics read is issued.",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			resources.ClusterFlag(true),
			{Name: resources.FlagNamespace, Type: "string", Desc: "`namespace` to operate in"},
			runtime.ProjectFlag(false, "keep only the project's namespaces (not with --by node)"),
			{Name: "by", Type: "string", Enum: []string{byNamespace, byProject, byNode, byPod},
				Desc: "Group results by namespace, project, node, or pod (default: namespace)"},
			{Name: "node", Type: "string",
				Desc: "Limit results to one node"},
		},
		Auth:  true,
		Shape: runtime.ShapeTabular,
	}
}

func (c *usageCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	by := in.String("by")
	if by == "" {
		by = byNamespace
	}

	// --by node rejects --namespace and --project.
	if by == byNode {
		if in.String(resources.FlagNamespace) != "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"--namespace does not apply to --by node; use --by pod --node <node> to see per-pod usage on a specific node")
		}
		if in.String(runtime.FlagProject) != "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"--project does not apply to --by node; use --by pod --project <project> to see where a project's pods are scheduled")
		}
	}

	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}

	// Every usage mode's required collectors (cluster resolution, the pod
	// list, and the scope-establishing reads) hard-error; only the metrics
	// fetch degrades.
	sec := newSections(ctx, sectionMetrics)
	switch by {
	case byNode:
		return c.collectByNode(ctx, client, sec, in)
	case byPod:
		return c.collectByPod(ctx, client, clusterID, sec, in)
	case byProject:
		return c.collectByProject(ctx, client, clusterID, sec, in)
	default:
		return c.collectByNamespace(ctx, client, clusterID, sec, in)
	}
}

// usageResult builds the command's Result from the collected rows and the
// accumulator, so entries and their warnings always travel together.
func usageResult(by string, rows usageRows, names map[string]string, sec *sections) runtime.Result {
	degraded, warnings := sec.Finalize()
	return runtime.Result{
		Kind:     "ResourceUsage",
		Data:     usageEnvelope{By: by, Data: rows, Degraded: degraded, names: names},
		Warnings: warnings,
	}
}

func (c *usageCmd) collectByNamespace(ctx context.Context, client engine.ClusterClient, clusterID string, sec *sections, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	podKind, _ := kinds.Lookup("pod")

	p, scoped, err := runtime.TypedProjectOn(ctx, in, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	var projectID string
	var members map[string]bool
	if scoped {
		projectID = p.ID
		members, err = p.Namespaces(ctx)
		if err != nil {
			return runtime.Result{}, err
		}
	}

	grouped := map[string][]unstructured.Unstructured{}
	var namespaces []string
	switch {
	case namespace != "" && projectID != "":
		if !members[namespace] {
			return runtime.Result{}, projects.NotMemberError(namespace, projectID,
				"pass a project namespace, or omit --namespace to scope the whole project")
		}
		namespaces = []string{namespace}
		l, lerr := client.List(ctx, podKind.GVR, namespace, "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		grouped[namespace] = filterPodsByNode(l.Items, nodeName)

	case projectID != "":
		// A membership list the caller was authorized to read, returning zero
		// namespaces, is a verified-empty scope: no pod could belong to it, so
		// no pod or metrics read is issued and no collector can degrade.
		if len(members) == 0 {
			return usageResult(byNamespace, usageRows{}, nil, sec), nil
		}
		// --project without --by returns member-namespace
		// rows, not a single project bucket. The single-bucket form is
		// --by project --project X (collectByProject). Every member namespace
		// gets a row, zero-pod members included.
		for ns := range members {
			namespaces = append(namespaces, ns)
			grouped[ns] = nil
		}
		l, lerr := client.List(ctx, podKind.GVR, "", "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		for _, pod := range filterPodsByNode(l.Items, nodeName) {
			if members[pod.GetNamespace()] {
				grouped[pod.GetNamespace()] = append(grouped[pod.GetNamespace()], pod)
			}
		}

	case namespace != "":
		namespaces = []string{namespace}
		l, lerr := client.List(ctx, podKind.GVR, namespace, "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		grouped[namespace] = filterPodsByNode(l.Items, nodeName)

	default:
		l, lerr := client.List(ctx, podKind.GVR, "", "")
		if lerr != nil {
			return runtime.Result{}, lerr
		}
		for _, pod := range filterPodsByNode(l.Items, nodeName) {
			grouped[pod.GetNamespace()] = append(grouped[pod.GetNamespace()], pod)
		}
	}

	rows, err := aggregateUsage(ctx, client, sec, grouped, namespaces)
	if err != nil {
		return runtime.Result{}, err
	}
	return usageResult(byNamespace, rows, nil, sec), nil
}

// fetchPodMetrics lists pod metrics with ONE List call: scoped to the single
// namespace when exactly one is given (--namespace mode never pulls
// cluster-wide metrics to summarize a handful of pods), cluster-wide
// otherwise. A multi-namespace --project run relies on the namespace+name
// key lookup in aggregateUsage to filter - an unscoped fetch is correct
// there because non-member pods are simply never looked up.
func fetchPodMetrics(ctx context.Context, client engine.ClusterClient, namespaces []string) (map[string]*unstructured.Unstructured, error) {
	scope := ""
	if len(namespaces) == 1 {
		scope = namespaces[0]
	}
	list, err := client.List(ctx, podMetricsGVR, scope, "")
	if err != nil {
		return nil, err
	}
	byKey := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		m := list.Items[i]
		byKey[m.GetNamespace()+"/"+m.GetName()] = &list.Items[i]
	}
	return byKey, nil
}

// aggregateUsage sums CPU/memory requests, limits, and (when metrics.k8s.io
// is reachable) usage per grouped key, degrading a metrics fetch failure to
// a metrics section rather than a command error - request and limit totals
// stay readable, so the usage section itself is never unavailable for this.
// Shared by usage's own per-namespace/per-project modes and inspect
// project's usage summary. sec MUST have sectionMetrics registered in its
// vocabulary - the metrics degrade records against it, and an unregistered
// section is the accumulator's programmer-error panic. One row is emitted
// per key present in grouped, even for nil (empty) pod slices - callers
// that need a guaranteed row for a scope with no pods must seed the key
// first.
func aggregateUsage(ctx context.Context, client engine.ClusterClient, sec *sections, grouped map[string][]unstructured.Unstructured, namespaces []string) (usageRows, error) {
	metricsOK := true
	metricsByKey, merr := fetchPodMetrics(ctx, client, namespaces)
	if merr != nil {
		if cerr := sec.Unavailable(sectionMetrics, merr); cerr != nil {
			return nil, cerr
		}
		metricsOK = false
	}

	result := usageRows{}
	for scope, pods := range grouped {
		cpuReq, cpuLim, memReq, memLim := sumPodResources(pods)
		var cpuUse, memUse resource.Quantity
		if metricsOK {
			for _, pod := range pods {
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
	return result, nil
}

// addQuantity parses the string quantity at path within container (a
// map[string]any decoded from spec.containers[i] or a pod-metrics
// containers[i] entry) and adds it to q. A missing or unparsable value is a
// silent no-op - an absent request/limit/usage contributes zero.
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

// sumPodResources walks pods' spec.containers and sums CPU/memory requests
// and limits. It is intentionally request/limit-only: usage (metrics.k8s.io)
// is a per-pod lookup that aggregateUsage handles after this call, and the
// node collector fetches node-level usage via nodeMetricsGVR instead.
func sumPodResources(pods []unstructured.Unstructured) (cpuReq, cpuLim, memReq, memLim resource.Quantity) {
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
	}
	return
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
// --by project). It implements output.Tabular for `-o table`.
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

// usageEnvelope is the top-level Result.Data for the `usage` command. It
// wraps the bare usageRows with a "by" discriminator so agents know the
// grouping without re-deriving it from flags. aggregateUsage still returns
// bare usageRows so project.go's type-assert is untouched. names,
// when non-nil, maps a row key to the label the table SCOPE column shows
// (the project grouping keys rows by project id but displays the project
// name); it is unexported so the JSON shape stays {"by","rows"}.
type usageEnvelope struct {
	By   string    `json:"by"`
	Data usageRows `json:"rows"`
	// Degraded names the collectors that failed while the required ones
	// succeeded; absent when everything the command needed was readable.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
	names    map[string]string
}

func (e usageEnvelope) Columns() []string { return e.Data.Columns() }

func (e usageEnvelope) Rows() [][]string {
	rows := e.Data.Rows()
	if len(e.names) == 0 {
		return rows
	}
	// Substitute display names before sorting so rows sort by the label shown
	// in the table, not by the internal id.
	for _, row := range rows {
		if display, ok := e.names[row[0]]; ok {
			row[0] = display
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i][0] < rows[j][0]
	})
	return rows
}

// filterPodsByNode returns the pods scheduled on nodeName; an empty nodeName
// is a no-op. Every grouping except --by node uses it to honor --node -
// an accepted flag must never be a silent no-op. Filtering is client-side:
// ClusterClient.List takes a label selector only (no field selectors).
func filterPodsByNode(pods []unstructured.Unstructured, nodeName string) []unstructured.Unstructured {
	if nodeName == "" {
		return pods
	}
	out := make([]unstructured.Unstructured, 0, len(pods))
	for _, pod := range pods {
		nn, _, _ := unstructured.NestedString(pod.Object, "spec", "nodeName")
		if nn == nodeName {
			out = append(out, pod)
		}
	}
	return out
}
