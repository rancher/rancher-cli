package inspect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type projectCmd struct {
	clients engine.ClusterClientFactory
}

// NewInspectProject builds `inspect project <name-or-id> --cluster <c>`: the
// project's identity, member namespaces with workload counts, members
// (prtbs), resourceQuota spec, and a usage summary, joined into one
// document. Mirrors pod.go/topnode.go's degradation pattern (NOT
// deployment.go, which is fail-hard): a prtb-read or metrics failure becomes
// a warning, never a command error. resourceQuota absence is not a
// failure — null is enough, no warning.
func NewInspectProject(clients engine.ClusterClientFactory) runtime.Command {
	return &projectCmd{clients: clients}
}

func (c *projectCmd) Describe() runtime.Descriptor {
	k, _ := kinds.Lookup("project")
	return runtime.Descriptor{
		Name:        "inspect project",
		Aliases:     k.Aliases,
		Usage:       "Inspect a project: namespaces, members, quota, and usage",
		CommandType: runtime.ReadOnly,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags:       []runtime.ArgSpec{{Name: resources.FlagCluster, Type: "string", Desc: "target cluster `id|name` (defaults to the current context)"}},
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c *projectCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, err := resources.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	mgmtClient, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	projectID, err := projects.ResolveProject(ctx, c.clients, clusterID, in.Args()[0])
	if err != nil {
		return runtime.Result{}, err
	}
	projectKind, _ := kinds.Lookup("project")
	project, err := mgmtClient.Get(ctx, projectKind.GVR, clusterID, projectID)
	if err != nil {
		return runtime.Result{}, err
	}
	displayName, _, _ := unstructured.NestedString(project.Object, "spec", "displayName")

	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	members, err := projects.ProjectNamespaces(ctx, client, projectID)
	if err != nil {
		return runtime.Result{}, err
	}

	podKind, _ := kinds.Lookup("pod")
	depKind, _ := kinds.Lookup("deployment")
	namespaces := make([]string, 0, len(members))
	for ns := range members {
		namespaces = append(namespaces, ns)
	}

	// ONE cluster-wide List per kind, filtered client-side by membership —
	// N per-namespace List pairs would cost 2N round trips for no reason.
	podList, err := client.List(ctx, podKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	depList, err := client.List(ctx, depKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	podCounts := map[string]int{}
	depCounts := map[string]int{}
	allPods := map[string][]unstructured.Unstructured{}
	for i := range podList.Items {
		ns := podList.Items[i].GetNamespace()
		if !members[ns] {
			continue
		}
		podCounts[ns]++
		allPods[projectID] = append(allPods[projectID], podList.Items[i])
	}
	for i := range depList.Items {
		ns := depList.Items[i].GetNamespace()
		if members[ns] {
			depCounts[ns]++
		}
	}
	nsRows := make([]projectNamespace, 0, len(members))
	for ns := range members {
		nsRows = append(nsRows, projectNamespace{
			Name:      ns,
			Workloads: projectWorkloads{Pods: podCounts[ns], Deployments: depCounts[ns]},
		})
	}
	sort.Slice(nsRows, func(i, j int) bool { return nsRows[i].Name < nsRows[j].Name })

	var warnings []string
	memberRows, mwarn, merr := projectMembers(ctx, mgmtClient, project)
	if merr != nil {
		return runtime.Result{}, merr
	}
	warnings = append(warnings, mwarn...)

	quota, _, _ := unstructured.NestedMap(project.Object, "spec", "resourceQuota")

	var usage *resourceTotals
	if len(namespaces) > 0 {
		res, uerr := aggregateUsage(ctx, client, clusterID, allPods, namespaces)
		if uerr != nil {
			return runtime.Result{}, uerr
		}
		warnings = append(warnings, res.Warnings...)
		if rows, ok := res.Data.(usageRows); ok {
			if totals, ok := rows[projectID]; ok {
				usage = &totals
			}
		}
	}

	result := projectResult{
		ID:            projectID,
		DisplayName:   displayName,
		Cluster:       clusterID,
		Namespaces:    nsRows,
		Members:       memberRows,
		ResourceQuota: quota,
		Usage:         usage,
	}
	return runtime.Result{
		Kind:     "ProjectInspection",
		Data:     result,
		Text:     renderProjectText(result),
		Warnings: warnings,
	}, nil
}

// projectMembers lists the project's prtbs from status.backingNamespace
// (falling back to the project id namespace when absent), resolves display
// names via resolveMembers, and degrades a listing failure (typically RBAC)
// to a warning plus a nil member slice (serializes "members":null — the
// pre-existing non-omitempty contract; [] means verified empty).
func projectMembers(ctx context.Context, mgmtClient engine.ClusterClient, project *unstructured.Unstructured) ([]projectMember, []string, error) {
	backingNS, found, _ := unstructured.NestedString(project.Object, "status", "backingNamespace")
	if !found || backingNS == "" {
		backingNS = project.GetName()
	}
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	list, err := mgmtClient.List(ctx, prtbKind.GVR, backingNS, "")
	if err != nil {
		if lookupCanceled(ctx, err) {
			return nil, nil, err
		}
		// nil (not empty) on unavailability: serializes "members":null — the
		// pre-existing non-omitempty contract; [] means verified empty.
		return nil, []string{"projectroletemplatebindings unavailable: " + err.Error()}, nil
	}
	resolved, warnings, err := resolveMembers(ctx, mgmtClient, list.Items)
	if err != nil {
		return nil, nil, err
	}
	rows := make([]projectMember, 0, len(resolved))
	for _, m := range resolved {
		rows = append(rows, projectMember{User: m.Principal, RoleTemplate: m.Role, DisplayName: m.DisplayName})
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

type projectMember struct {
	User         string `json:"user"`
	RoleTemplate string `json:"roleTemplate"`
	DisplayName  string `json:"displayName,omitempty"`
}

// projectResult is `inspect project`'s payload; the default rendering is the
// sectioned text card produced by renderProjectText.
type projectResult struct {
	ID            string             `json:"id"`
	DisplayName   string             `json:"displayName"`
	Cluster       string             `json:"cluster"`
	Namespaces    []projectNamespace `json:"namespaces"`
	Members       []projectMember    `json:"members"`
	ResourceQuota map[string]any     `json:"resourceQuota"`
	Usage         *resourceTotals    `json:"usage"`
}

// renderProjectText is the default rendering: the same multi-section card
// pattern as renderClusterText. Omitted sections render a concise
// availability marker; detailed causes live in Result.Warnings (stderr).
func renderProjectText(r projectResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PROJECT\n  id: %s\n  display: %s\n  cluster: %s\n", r.ID, r.DisplayName, r.Cluster)

	b.WriteString("\nNAMESPACES\n")
	if len(r.Namespaces) == 0 {
		b.WriteString("  none\n")
	}
	for _, ns := range r.Namespaces {
		fmt.Fprintf(&b, "  %s (pods: %d, deployments: %d)\n", ns.Name, ns.Workloads.Pods, ns.Workloads.Deployments)
	}

	b.WriteString("\nMEMBERS\n")
	switch {
	case r.Members == nil:
		b.WriteString("  members: unavailable\n")
	case len(r.Members) == 0:
		b.WriteString("  none\n")
	default:
		for _, m := range r.Members {
			if m.DisplayName != "" {
				fmt.Fprintf(&b, "  %s (%s)  %s\n", m.DisplayName, m.User, m.RoleTemplate)
			} else {
				fmt.Fprintf(&b, "  %s  %s\n", m.User, m.RoleTemplate)
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
	case len(r.Namespaces) == 0:
		// A project with zero namespaces has nothing to measure — a
		// verified-empty state, not an unavailable one.
		b.WriteString("  none (no namespaces)\n")
	default:
		b.WriteString("  usage: unavailable\n")
	}
	return b.String()
}

// writeSortedMap renders a nested map with recursively sorted keys —
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
