package inspect

import (
	"context"
	"sort"
	"strconv"

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
	return runtime.Descriptor{
		Name:        "inspect project",
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
	memberRows, mwarn := projectMembers(ctx, mgmtClient, project)
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

	return runtime.Result{
		Kind: "ProjectInspection",
		Data: projectResult{
			ID:            projectID,
			DisplayName:   displayName,
			Cluster:       clusterID,
			Namespaces:    nsRows,
			Members:       memberRows,
			ResourceQuota: quota,
			Usage:         usage,
		},
		Warnings: warnings,
	}, nil
}

// projectMembers lists the project's prtbs from status.backingNamespace
// (falling back to the project id namespace when absent) and degrades a
// listing failure (typically RBAC) to a warning plus an empty member list.
func projectMembers(ctx context.Context, mgmtClient engine.ClusterClient, project *unstructured.Unstructured) ([]projectMember, []string) {
	backingNS, found, _ := unstructured.NestedString(project.Object, "status", "backingNamespace")
	if !found || backingNS == "" {
		backingNS = project.GetName()
	}
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	list, err := mgmtClient.List(ctx, prtbKind.GVR, backingNS, "")
	if err != nil {
		return nil, []string{"projectroletemplatebindings unavailable: " + err.Error()}
	}
	rows := make([]projectMember, 0, len(list.Items))
	for i := range list.Items {
		user, _, _ := unstructured.NestedString(list.Items[i].Object, "userPrincipalName")
		roleTemplate, _, _ := unstructured.NestedString(list.Items[i].Object, "roleTemplateName")
		rows = append(rows, projectMember{User: user, RoleTemplate: roleTemplate})
	}
	return rows, nil
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
}

// projectResult is `inspect project`'s payload; it implements
// output.Tabular so `-o table` renders one summary row.
type projectResult struct {
	ID            string             `json:"id"`
	DisplayName   string             `json:"displayName"`
	Cluster       string             `json:"cluster"`
	Namespaces    []projectNamespace `json:"namespaces"`
	Members       []projectMember    `json:"members"`
	ResourceQuota map[string]any     `json:"resourceQuota"`
	Usage         *resourceTotals    `json:"usage"`
}

func (p projectResult) Columns() []string { return []string{"ID", "DISPLAY", "NAMESPACES", "MEMBERS"} }

func (p projectResult) Rows() [][]string {
	return [][]string{{p.ID, p.DisplayName, strconv.Itoa(len(p.Namespaces)), strconv.Itoa(len(p.Members))}}
}
