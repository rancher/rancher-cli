package projects

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type addNamespaceCmd struct {
	clients engine.ClusterClientFactory
}

// NewAddNamespace builds `project add-namespace <namespace> --project <p>
// --cluster <c>` (mutating): sets the membership label and annotation on an
// existing namespace.
func NewAddNamespace(clients engine.ClusterClientFactory) runtime.Command {
	return &addNamespaceCmd{clients: clients}
}

func (addNamespaceCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "project add-namespace",
		Usage:       "Add a namespace to a project",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "namespace", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			runtime.ProjectFlag(true, "target project `id|name`"),
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` of the project"},
		},
		Examples:    []string{"rancher project add-namespace team-a --project p-x --cluster c-x --apply"},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c addNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	nsName, err := membershipArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	cluster, client, err := membershipCluster(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	p, ok, err := runtime.TypedProjectOn(ctx, in, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeGeneric, "command bug: project add-namespace runs without the required --project")
	}
	projectID := p.ID
	before, err := getNamespace(ctx, client, nsName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	current, _, _ := unstructured.NestedString(before.Object, "metadata", "labels", projectLabel)
	if current != "" && current != projectID {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("namespace %q belongs to project %q; remove it from that project first", nsName, current)).
			WithHint(fmt.Sprintf(clierr.Rancher+" project remove-namespace %s --cluster %s",
				clierr.ShellQuote(nsName), clierr.ShellQuote(cluster)))
	}

	after := before.DeepCopy()
	SetMembership(after, cluster, projectID)

	return planMembershipUpdate(ctx, client, before, after, cluster, fmt.Sprintf("added to project %s", projectID))
}

type removeNamespaceCmd struct {
	clients engine.ClusterClientFactory
}

// NewRemoveNamespace builds `project remove-namespace <namespace> --cluster
// <c>` (mutating): clears the membership label and annotation from an
// existing namespace. The namespace's own project label names the project
// it leaves, so the command takes no --project.
func NewRemoveNamespace(clients engine.ClusterClientFactory) runtime.Command {
	return &removeNamespaceCmd{clients: clients}
}

func (removeNamespaceCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "project remove-namespace",
		Usage:       "Remove a namespace from a project",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "namespace", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` of the namespace"},
		},
		Examples:    []string{"rancher project remove-namespace team-a --cluster c-x --apply"},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// ProjectRefusalHint is the command to run instead of a typed --project:
// the same removal without it, previewed, when the namespace and --cluster
// are typed.
func (removeNamespaceCmd) ProjectRefusalHint(in runtime.Input) string {
	args := in.Args()
	cluster := in.String(engine.FlagCluster)
	if len(args) == 0 || args[0] == "" || cluster == "" {
		return ""
	}
	return clierr.Rancher + " project remove-namespace " + clierr.ShellQuote(args[0]) +
		" --cluster " + clierr.ShellQuote(cluster) + " --dry-run"
}

func (c removeNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	nsName, err := membershipArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	cluster, client, err := membershipCluster(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	before, err := getNamespace(ctx, client, nsName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	current, _, _ := unstructured.NestedString(before.Object, "metadata", "labels", projectLabel)
	if current == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("namespace %s is in no project; there is nothing to remove", clierr.ShellQuote(nsName)))
	}

	after := before.DeepCopy()
	ClearMembership(after)

	return planMembershipUpdate(ctx, client, before, after, cluster, fmt.Sprintf("removed from project %s", current))
}

// membershipArgs validates the shared add/remove-namespace input shape: a
// required positional namespace name.
func membershipArgs(in runtime.Input) (nsName string, err error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return "", clierr.New(clierr.CodeValidation, "argument <namespace> is required")
	}
	return args[0], nil
}

// membershipCluster resolves the cluster add/remove-namespace act on and its
// client.
func membershipCluster(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) (string, engine.ClusterClient, error) {
	cluster, err := engine.ResolveDownstreamCluster(ctx, clients, in)
	if err != nil {
		return "", nil, err
	}
	client, err := clients.Cluster(ctx, cluster)
	if err != nil {
		return "", nil, err
	}
	return cluster, client, nil
}

// getNamespace fetches the live namespace object add/remove-namespace
// mutate.
func getNamespace(ctx context.Context, client engine.ClusterClient, nsName string) (*unstructured.Unstructured, error) {
	nsKind, _ := kinds.Lookup("namespace")
	return client.Get(ctx, nsKind.GVR, "", nsName)
}

// planMembershipUpdate diffs before/after and previews the update through the
// shared PlanUpdate. An unchanged namespace (re-adding to its own project)
// yields an empty diff rather than an error. contextLine is the bespoke,
// already-worded detail line appended, indented, below the shared
// "namespace/<ns> patched" text once the mutation commits.
func planMembershipUpdate(ctx context.Context, client engine.ClusterClient, before, after *unstructured.Unstructured, cluster, contextLine string) (safety.Effect, runtime.ExecuteFunc, error) {
	diff := engine.DiffObjects(before.Object, after.Object)
	rv := before.GetResourceVersion()
	after.SetResourceVersion(rv)
	nsKind, _ := kinds.Lookup("namespace")
	eff, exec, err := engine.PlanUpdate(ctx, client, nsKind.GVR, "", after, cluster, nsKind.Name, "Namespace", diff, false)
	if err != nil {
		return eff, exec, err
	}
	wrapped := func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		tr := runtime.TextLines(strings.TrimSuffix(res.Text, "\n"), contextLine)
		res.Text, res.StyledText = tr.Text, tr.StyledText
		return res, nil
	}
	return eff, wrapped, nil
}
