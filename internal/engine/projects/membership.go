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
// [--cluster <c>]` (mutating): sets the membership label and annotation on an
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
			{Name: "project", Type: "string", Required: true, Desc: "target project `id|name`"},
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` of the project"},
		},
		Examples:    []string{"rancher project add-namespace team-a --project p-x --cluster c-x --apply"},
		Destructive: "false",
		Network:     true,
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c addNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	nsName, projectFlag, err := membershipArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	cluster, client, projectID, before, err := loadMembershipTarget(ctx, c.clients, in, nsName, projectFlag)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	current, _, _ := unstructured.NestedString(before.Object, "metadata", "labels", projectLabel)
	if current != "" && current != projectID {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("namespace %q belongs to project %q", nsName, current)).
			WithHint(fmt.Sprintf("run: rancher project remove-namespace %s --project %s --cluster %s first", nsName, current, cluster))
	}

	after := before.DeepCopy()
	SetMembership(after, cluster, projectID)

	return planMembershipUpdate(ctx, client, before, after, cluster, fmt.Sprintf("added to project %s", projectID))
}

type removeNamespaceCmd struct {
	clients engine.ClusterClientFactory
}

// NewRemoveNamespace builds `project remove-namespace <namespace> --project
// <p> [--cluster <c>]` (mutating): clears the membership label and
// annotation from an existing namespace.
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
			{Name: "project", Type: "string", Required: true, Desc: "target project `id|name`"},
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` of the project"},
		},
		Examples:    []string{"rancher project remove-namespace team-a --project p-x --cluster c-x --apply"},
		Destructive: "false",
		Network:     true,
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c removeNamespaceCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	nsName, projectFlag, err := membershipArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	cluster, client, projectID, before, err := loadMembershipTarget(ctx, c.clients, in, nsName, projectFlag)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	current, _, _ := unstructured.NestedString(before.Object, "metadata", "labels", projectLabel)
	if current != projectID {
		return safety.Effect{}, nil, NotMemberError(nsName, projectID, "check the namespace's current project")
	}

	after := before.DeepCopy()
	ClearMembership(after)

	return planMembershipUpdate(ctx, client, before, after, cluster, fmt.Sprintf("removed from project %s", projectID))
}

// membershipArgs validates the shared add/remove-namespace input shape: a
// required positional namespace name and a required --project flag.
func membershipArgs(in runtime.Input) (nsName, projectFlag string, err error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return "", "", clierr.New(clierr.CodeValidation, "argument <namespace> is required").WithHint("provide <namespace>")
	}
	projectFlag = in.String("project")
	if projectFlag == "" {
		return "", "", clierr.New(clierr.CodeValidation, "--project is required").WithHint("pass --project <id|name>")
	}
	return args[0], projectFlag, nil
}

// loadMembershipTarget resolves the cluster and project, and fetches the live
// namespace object both add/remove-namespace mutate.
func loadMembershipTarget(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input, nsName, projectFlag string) (cluster string, client engine.ClusterClient, projectID string, before *unstructured.Unstructured, err error) {
	cluster, err = engine.ResolveDownstreamCluster(ctx, clients, in)
	if err != nil {
		return "", nil, "", nil, err
	}
	client, err = clients.Cluster(ctx, cluster)
	if err != nil {
		return "", nil, "", nil, err
	}
	projectID, err = ResolveProject(ctx, clients, cluster, projectFlag)
	if err != nil {
		return "", nil, "", nil, err
	}
	nsKind, _ := kinds.Lookup("namespace")
	before, err = client.Get(ctx, nsKind.GVR, "", nsName)
	if err != nil {
		return "", nil, "", nil, err
	}
	return cluster, client, projectID, before, nil
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
		res.Text = runtime.TextLines(strings.TrimSuffix(res.Text, "\n"), contextLine)
		return res, nil
	}
	return eff, wrapped, nil
}
