package inspect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	projectscomplete "github.com/rancher/rancher-cli/internal/engine/projects/complete"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type projectCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewInspectProject builds `inspect project <name-or-id> --cluster <c>`: the
// project's identity, member namespaces with workload counts, members
// (prtbs), resourceQuota spec, and a usage summary, joined into one
// document. Only the project itself is required: every other collector
// degrades to a `degraded` entry and its warning rather than failing the
// command. resourceQuota absence is not a failure - null is enough, no
// warning. The positional resolves through the projects steve lists for
// the caller (roots), by id or display name.
func NewInspectProject(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &projectCmd{clients: clients, roots: roots}
}

func (c *projectCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("project")
	return runtime.Descriptor{
		Name:    "inspect project",
		Aliases: k.Aliases,
		Usage:   "Inspect a project: namespaces, members, quota, and usage",
		Desc: degradedContractDesc +
			"Cluster resolution and the project resolve plus GET are required and hard-error. " +
			"Degradable sections: namespaces (unavailable - the namespace rows, whose integer counts " +
			"cannot represent an unknown), members (each row reports principal, displayName, role, binding, " +
			"and system; unavailable - collected independently from the " +
			"management plane, so downstream failures leave it intact), usage (unavailable - blocked by " +
			"a downstream-client, membership, or pod-list failure), metrics (unavailable - request and " +
			"limit totals stay). A membership set that resolves to zero namespaces is a verified-empty " +
			"scope: no workload or usage read is issued and nothing degrades.",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags:       []runtime.ArgSpec{engine.ClusterFlag(true, false)},
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// LiveSource hands the surface the command's positional source: project
// ids and unique display names, which the command resolves; the generated
// project verbs take ids only.
func (*projectCmd) LiveSource() completion.Source { return projectscomplete.ProjectPositionalSource() }

func (c *projectCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, false)
	if err != nil {
		return runtime.Result{}, err
	}
	mgmtClient, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	root, err := c.roots.Root()
	if err != nil {
		return runtime.Result{}, err
	}
	get := steve.RootGetter(root)
	resolved, matchWarning, err := steve.ResolveProjectArgument(ctx, get, mgmtClient, clusterID, in.Args()[0])
	if err != nil {
		return runtime.Result{}, err
	}
	projectID := resolved.ID
	projectKind, _ := kinds.Lookup("project")
	project, err := mgmtClient.Get(ctx, projectKind.GVR, clusterID, projectID)
	if err != nil {
		return runtime.Result{}, err
	}
	displayName, _, _ := unstructured.NestedString(project.Object, "spec", "displayName")

	sec := newSections(ctx, sectionNamespaces, sectionMembers, sectionUsage, sectionMetrics)

	// nsRows stays nil until the namespace section is fully known: the row's
	// workload counts are non-pointer integers, so a failed workload list
	// cannot be reported without fabricating zeroes. null is the whole
	// section's unavailable shape; [] means verified empty.
	var nsRows []projectNamespace
	var usage *resourceTotals

	client, cerr := c.clients.Cluster(ctx, clusterID)
	if cerr != nil {
		// The management-plane document is already readable; only the
		// downstream sections are lost.
		if herr := sec.Unavailable(sectionNamespaces, cerr); herr != nil {
			return runtime.Result{}, herr
		}
		if herr := sec.Blocked(sectionUsage, sectionNamespaces, cerr); herr != nil {
			return runtime.Result{}, herr
		}
	} else {
		var derr error
		nsRows, usage, derr = c.downstreamSections(ctx, client, get, clusterID, projectID, sec)
		if derr != nil {
			return runtime.Result{}, derr
		}
	}

	memberRows, warnings, merr := projectMembers(ctx, mgmtClient, project, sec)
	if merr != nil {
		return runtime.Result{}, merr
	}
	if matchWarning != "" {
		warnings = append([]string{matchWarning}, warnings...)
	}

	quota, _, _ := unstructured.NestedMap(project.Object, "spec", "resourceQuota")

	degraded, degradedWarnings := sec.Finalize()
	result := projectResult{
		ID:            projectID,
		DisplayName:   displayName,
		Cluster:       clusterID,
		Namespaces:    nsRows,
		Members:       memberRows,
		ResourceQuota: quota,
		Usage:         usage,
		Degraded:      degraded,
	}
	text := renderProjectText(result)
	return runtime.Result{
		Kind:       "ProjectInspection",
		Data:       result,
		Text:       text,
		StyledText: func(s style.Style) string { return styleProjectText(text, s) },
		Warnings:   append(warnings, degradedWarnings...),
	}, nil
}

// downstreamSections collects the namespace rows and the usage summary from
// the downstream cluster. Membership defines both domains, so a membership
// failure degrades them together; a pod-list failure additionally blocks
// usage (it aggregates pods), while a deployment-list failure leaves usage
// intact. A membership set proven empty short-circuits both workload lists:
// the caller established the scope, and reads that could not have
// contributed rows must not turn a verified-empty project into an RBAC
// failure.
func (c *projectCmd) downstreamSections(ctx context.Context, client engine.ClusterClient, get steve.Getter, clusterID, projectID string, sec *sections) ([]projectNamespace, *resourceTotals, error) {
	members, err := steve.ProjectNamespaces(ctx, client, get, clusterID, projectID)
	if err != nil {
		if herr := sec.Unavailable(sectionNamespaces, err); herr != nil {
			return nil, nil, herr
		}
		if herr := sec.Blocked(sectionUsage, sectionNamespaces, err); herr != nil {
			return nil, nil, herr
		}
		return nil, nil, nil
	}
	namespaces := make([]string, 0, len(members))
	for ns := range members {
		namespaces = append(namespaces, ns)
	}
	if len(namespaces) == 0 {
		return []projectNamespace{}, nil, nil
	}

	podKind, _ := kinds.Lookup("pod")
	depKind, _ := kinds.Lookup("deployment")

	// ONE cluster-wide List per kind, filtered client-side by membership -
	// N per-namespace List pairs would cost 2N round trips for no reason.
	// The two lists are independent reads feeding disjoint outputs, so they
	// run concurrently (the errgroup/WaitGroup pattern inspect cluster uses);
	// the degrade DECISIONS stay sequential after the join - recording both
	// failures would hit the accumulator's duplicate-namespaces rule.
	var (
		podList, depList *unstructured.UnstructuredList
		podErr, depErr   error
	)
	var wg sync.WaitGroup
	wg.Go(func() { podList, podErr = client.List(ctx, podKind.GVR, "", "") })
	wg.Go(func() { depList, depErr = client.List(ctx, depKind.GVR, "", "") })
	wg.Wait()
	if podErr != nil {
		if herr := sec.Unavailable(sectionNamespaces, podErr); herr != nil {
			return nil, nil, herr
		}
		if herr := sec.Blocked(sectionUsage, sectionPods, podErr); herr != nil {
			return nil, nil, herr
		}
		return nil, nil, nil
	}
	podCounts := map[string]int{}
	allPods := map[string][]unstructured.Unstructured{}
	// Seed before the loop so aggregateUsage produces verified-zero totals when
	// there are namespaces but no matching pods. append-to-nil is safe.
	allPods[projectID] = nil
	for i := range podList.Items {
		ns := podList.Items[i].GetNamespace()
		if !members[ns] {
			continue
		}
		podCounts[ns]++
		allPods[projectID] = append(allPods[projectID], podList.Items[i])
	}

	var nsRows []projectNamespace
	if depErr != nil {
		if herr := sec.Unavailable(sectionNamespaces, depErr); herr != nil {
			return nil, nil, herr
		}
	} else {
		depCounts := map[string]int{}
		for i := range depList.Items {
			ns := depList.Items[i].GetNamespace()
			if members[ns] {
				depCounts[ns]++
			}
		}
		nsRows = make([]projectNamespace, 0, len(members))
		for ns := range members {
			nsRows = append(nsRows, projectNamespace{
				Name:      ns,
				Workloads: projectWorkloads{Pods: podCounts[ns], Deployments: depCounts[ns]},
			})
		}
		sort.Slice(nsRows, func(i, j int) bool { return nsRows[i].Name < nsRows[j].Name })
	}

	rows, err := aggregateUsage(ctx, client, sec, allPods, namespaces)
	if err != nil {
		return nil, nil, err
	}
	var usage *resourceTotals
	if totals, ok := rows[projectID]; ok {
		usage = &totals
	}
	return nsRows, usage, nil
}

// projectMembers lists the project's prtbs from status.backingNamespace
// (falling back to the project id namespace when absent), resolves display
// names via rbac.ResolveMembers, and degrades a listing failure (typically
// RBAC) to a warning plus a nil member slice (serializes "members":null -
// the pre-existing non-omitempty contract; [] means verified empty).
func projectMembers(ctx context.Context, mgmtClient engine.ClusterClient, project *unstructured.Unstructured, sec *sections) ([]rbac.MemberRow, []string, error) {
	backingNS, found, _ := unstructured.NestedString(project.Object, "status", "backingNamespace")
	if !found || backingNS == "" {
		backingNS = project.GetName()
	}
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	list, err := mgmtClient.List(ctx, prtbKind.GVR, backingNS, "")
	if err != nil {
		// nil (not empty) on unavailability: serializes "members":null - the
		// pre-existing non-omitempty contract; [] means verified empty.
		if herr := sec.Unavailable(sectionMembers, err); herr != nil {
			return nil, nil, herr
		}
		return nil, nil, nil
	}
	rows, warnings, err := rbac.ResolveMembers(ctx, mgmtClient, list.Items)
	if err != nil {
		return nil, nil, err
	}
	return rows, warnings, nil
}

type projectWorkloads struct {
	Pods        int `json:"pods"`
	Deployments int `json:"deployments"`
}

type projectNamespace struct {
	Name      string           `json:"name"`
	Workloads projectWorkloads `json:"workloads"`
}

// projectResult is `inspect project`'s payload; the default rendering is the
// sectioned text card produced by renderProjectText.
type projectResult struct {
	ID            string             `json:"id"`
	DisplayName   string             `json:"displayName"`
	Cluster       string             `json:"cluster"`
	Namespaces    []projectNamespace `json:"namespaces"`
	Members       []rbac.MemberRow   `json:"members"`
	ResourceQuota map[string]any     `json:"resourceQuota"`
	Usage         *resourceTotals    `json:"usage"`
	// Degraded names the sections this run could not read in full. Absent
	// when every collector succeeded.
	Degraded []sectionOutcome `json:"degraded,omitempty"`
}

// renderProjectText is the default rendering: the same multi-section card
// pattern as renderClusterText. Omitted sections render a concise
// availability marker; detailed causes live in Result.Warnings (stderr).
func renderProjectText(r projectResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROJECT\n  id: %s\n  display: %s\n  cluster: %s\n", r.ID, r.DisplayName, r.Cluster)

	b.WriteString("\nNAMESPACES\n")
	switch {
	case r.Namespaces == nil:
		b.WriteString("  namespaces: unavailable\n")
	case len(r.Namespaces) == 0:
		b.WriteString("  none\n")
	default:
		for _, ns := range r.Namespaces {
			fmt.Fprintf(&b, "  %s (pods: %d, deployments: %d)\n", ns.Name, ns.Workloads.Pods, ns.Workloads.Deployments)
		}
	}

	b.WriteString("\nMEMBERS\n")
	switch {
	case r.Members == nil:
		b.WriteString("  members: unavailable\n")
	case len(r.Members) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range r.Members {
			suffix := ""
			if m.System {
				suffix = " [system]"
			}
			if m.DisplayName != "" {
				fmt.Fprintf(&b, "  %s (%s)  %s%s\n", output.Printable(m.DisplayName), output.Printable(m.Principal), output.Printable(m.Role), suffix)
			} else {
				fmt.Fprintf(&b, "  %s  %s%s\n", output.Printable(m.Principal), output.Printable(m.Role), suffix)
			}
		}
	}

	b.WriteString("\nQUOTA\n")
	if len(r.ResourceQuota) == 0 {
		b.WriteString("  none\n")
	} else {
		writeSortedMap(&b, r.ResourceQuota, 1)
	}

	b.WriteString("\nUSAGE\n")
	switch {
	case r.Usage != nil:
		cpu := fmt.Sprintf("  cpu: request=%s limit=%s", r.Usage.CPURequest, r.Usage.CPULimit)
		if r.Usage.CPUUsage != "" {
			cpu += " usage=" + r.Usage.CPUUsage
		}
		b.WriteString(cpu + "\n")
		mem := fmt.Sprintf("  memory: request=%s limit=%s", r.Usage.MemoryRequest, r.Usage.MemoryLimit)
		if r.Usage.MemoryUsage != "" {
			mem += " usage=" + r.Usage.MemoryUsage
		}
		b.WriteString(mem + "\n")
	case r.Namespaces != nil && len(r.Namespaces) == 0:
		// A project with zero namespaces has nothing to measure - a
		// verified-empty state, not an unavailable one. Namespaces null is a
		// failed read, which says nothing about usage.
		b.WriteString("  none (no namespaces)\n")
	default:
		b.WriteString("  usage: unavailable\n")
	}
	return b.String()
}

// projectSectionHeaders is the fixed, always-present section header set
// renderProjectText emits - every one of them, every call, regardless of
// which sub-objects came back unavailable.
var projectSectionHeaders = map[string]bool{
	"PROJECT": true, "NAMESPACES": true, "MEMBERS": true, "QUOTA": true, "USAGE": true,
}

// styleProjectText applies inspect project's bold spans to text, the plain
// rendering renderProjectText already produced: every section header, bold,
// whole line. Nothing inside a section is styled.
func styleProjectText(text string, s style.Style) string {
	return runtime.BoldExactLines(text, projectSectionHeaders, s)
}

// writeSortedMap renders a nested map with recursively sorted keys -
// canonical order, never Go map iteration order.
func writeSortedMap(b *strings.Builder, m map[string]any, depth int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	indent := strings.Repeat("  ", depth)
	for _, k := range keys {
		if child, ok := m[k].(map[string]any); ok {
			fmt.Fprintf(b, "%s%s:\n", indent, k)
			writeSortedMap(b, child, depth+1)
			continue
		}
		fmt.Fprintf(b, "%s%s: %v\n", indent, k, m[k])
	}
}
