package projects

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// resolveUser maps a user display name or id to its metadata.name plus its
// principalIds, mirroring ResolveProject's collision semantics exactly: an id
// match and any display-name matches are collected as a union of candidate
// ids, not an id-wins short-circuit. The user kind is Management-scoped and
// NSNone, so the list is cluster-independent (always read via the "local"
// proxy client).
func resolveUser(ctx context.Context, clients engine.ClusterClientFactory, nameOrID string) (id string, principalIDs []string, err error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", nil, err
	}
	userKind, _ := kinds.Lookup("user")
	list, err := client.List(ctx, userKind.GVR, "", "")
	if err != nil {
		return "", nil, err
	}

	var idMatch string
	var idFound bool
	var displayMatches []string
	var cands []clierr.Candidate
	principalsByID := map[string][]string{}
	for i := range list.Items {
		uid := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "displayName")
		cands = append(cands, clierr.Candidate{ID: uid, Name: display})
		pids, _, _ := unstructured.NestedStringSlice(list.Items[i].Object, "principalIds")
		principalsByID[uid] = pids
		if uid == nameOrID {
			idMatch, idFound = uid, true
		}
		if display == nameOrID {
			displayMatches = append(displayMatches, uid)
		}
	}

	matched := map[string]bool{}
	if idFound {
		matched[idMatch] = true
	}
	for _, uid := range displayMatches {
		matched[uid] = true
	}
	var ids []string
	for uid := range matched {
		ids = append(ids, uid)
	}

	switch len(ids) {
	case 1:
		return ids[0], principalsByID[ids[0]], nil
	case 0:
		return "", nil, clierr.New(clierr.CodeNotFound, "user not found: "+nameOrID).
			WithCandidates(cands).
			WithHint("run: rancher get users")
	default:
		msg := fmt.Sprintf("%d users have display name %q", len(displayMatches), nameOrID)
		if idFound {
			msg = fmt.Sprintf("%q matches both a user id and a different user's display name", nameOrID)
		}
		return "", nil, clierr.New(clierr.CodeAmbiguousSelection, msg).
			WithCandidates(cands).WithHint("pass the user id")
	}
}

// memberArgs validates the shared add/remove-member positional + --project
// input shape.
func memberArgs(in runtime.Input) (principal, projectFlag string, err error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return "", "", clierr.New(clierr.CodeValidation, "argument <principal> is required").WithHint("provide <principal>")
	}
	projectFlag = in.String("project")
	if projectFlag == "" {
		return "", "", clierr.New(clierr.CodeValidation, "--project is required").WithHint("pass --project <id|name>")
	}
	return args[0], projectFlag, nil
}

// resolveMemberTarget resolves the downstream cluster, the project, and the
// project's backing namespace PRTBs live in — the shared setup for
// add-member and remove-member.
func resolveMemberTarget(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) (cluster, projectID, backingNS string, err error) {
	cluster, err = engine.ResolveDownstreamCluster(ctx, clients, in)
	if err != nil {
		return "", "", "", err
	}
	projectID, err = ResolveProject(ctx, clients, cluster, in.String("project"))
	if err != nil {
		return "", "", "", err
	}
	backingNS, err = BackingNamespace(ctx, clients, cluster, projectID)
	if err != nil {
		return "", "", "", err
	}
	return cluster, projectID, backingNS, nil
}

type addMemberCmd struct {
	clients engine.ClusterClientFactory
}

// NewAddMember builds `project add-member <principal> --project <p> --role
// <r> [--cluster <c>] [--group]` (mutating): creates a projectroletemplatebinding
// in the project's backing namespace. A user principal is resolved to a
// single user object and the binding carries BOTH userName and
// userPrincipalName from that one object — a principal-only binding whose
// value isn't an existing user's principalIds entry makes Rancher's
// EnsureUser silently mint an orphan user and bind the role to it instead
// (verified against rancher/rancher); setting both fields from the same
// resolved object forecloses that. --group is passthrough raw into
// groupPrincipalName (no group kind exists to resolve against).
func NewAddMember(clients engine.ClusterClientFactory) runtime.Command {
	return &addMemberCmd{clients: clients}
}

func (addMemberCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "project add-member",
		Usage:       "Add a member to a project via a role template binding",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "principal", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "project", Type: "string", Required: true},
			{Name: "role", Type: "string", Required: true},
			{Name: "cluster", Type: "string"},
			{Name: "group", Type: "bool"},
		},
		Examples:    []string{"rancher project add-member u-abc123 --project p-x --role project-member --cluster c-x --apply"},
		Destructive: "false",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c addMemberCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	principal, _, err := memberArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	role := in.String("role")
	if role == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--role is required").
			WithHint("pass --role <project-owner|project-member|read-only|roletemplate-id>")
	}

	cluster, projectID, backingNS, err := resolveMemberTarget(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion":       "management.cattle.io/v3",
		"kind":             "ProjectRoleTemplateBinding",
		"metadata":         map[string]any{"generateName": role + "-", "namespace": backingNS},
		"projectName":      cluster + ":" + projectID,
		"roleTemplateName": role,
	}}

	if in.Bool("group") {
		_ = unstructured.SetNestedField(obj.Object, principal, "groupPrincipalName")
	} else {
		userID, principalIDs, uerr := resolveUser(ctx, c.clients, principal)
		if uerr != nil {
			return safety.Effect{}, nil, uerr
		}
		if len(principalIDs) == 0 {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("user %q has no principal ids", userID)).
				WithHint("pass --group for group principals or fix the user object")
		}
		_ = unstructured.SetNestedField(obj.Object, userID, "userName")
		_ = unstructured.SetNestedField(obj.Object, principalIDs[0], "userPrincipalName")
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	return engine.PlanCreate(ctx, client, prtbKind.GVR, backingNS, obj, cluster, "ProjectRoleTemplateBinding")
}

type removeMemberCmd struct {
	clients engine.ClusterClientFactory
}

// NewRemoveMember builds `project remove-member <principal> --project <p>
// [--cluster <c>] [--group]` (mutating, destructive): finds every
// projectroletemplatebinding in the project's backing namespace bound to the
// resolved principal and deletes them all as ONE Effect — safety.Effect has
// a single Target string, so a multi-match (same principal, different roles)
// renders as one descriptive Target rather than invented per-object
// DiffEntries.
func NewRemoveMember(clients engine.ClusterClientFactory) runtime.Command {
	return &removeMemberCmd{clients: clients}
}

func (removeMemberCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "project remove-member",
		Usage:       "Remove a member from a project by deleting their role template binding(s)",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "principal", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "project", Type: "string", Required: true},
			{Name: "cluster", Type: "string"},
			{Name: "group", Type: "bool"},
		},
		Examples:    []string{"rancher project remove-member u-abc123 --project p-x --cluster c-x --apply --yes"},
		Destructive: "true",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c removeMemberCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	principal, _, err := memberArgs(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	cluster, _, backingNS, err := resolveMemberTarget(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	prtbKind, _ := kinds.Lookup("projectroletemplatebinding")
	list, err := client.List(ctx, prtbKind.GVR, backingNS, "")
	if err != nil {
		return safety.Effect{}, nil, err
	}

	group := in.Bool("group")
	var userID string
	if !group {
		userID, _, err = resolveUser(ctx, c.clients, principal)
		if err != nil {
			return safety.Effect{}, nil, err
		}
	}

	var names []string
	for i := range list.Items {
		obj := &list.Items[i]
		if group {
			gp, _, _ := unstructured.NestedString(obj.Object, "groupPrincipalName")
			if gp == principal {
				names = append(names, obj.GetName())
			}
			continue
		}
		un, _, _ := unstructured.NestedString(obj.Object, "userName")
		if un == userID {
			names = append(names, obj.GetName())
		}
	}

	if len(names) == 0 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("no projectroletemplatebinding for principal %q in project", principal)).
			WithHint("run: rancher get projectroletemplatebindings --namespace " + backingNS + " -o json")
	}

	// Admission preview: a server-side dry-run delete on every match surfaces
	// RBAC/webhook denials before the user confirms.
	for _, name := range names {
		if err := client.Delete(ctx, prtbKind.GVR, backingNS, name, "", true); err != nil {
			return safety.Effect{}, nil, err
		}
	}

	target := fmt.Sprintf("%s/%s", prtbKind.GVR.Resource, names[0])
	if len(names) > 1 {
		target = fmt.Sprintf("%s (%d) for %s", prtbKind.GVR.Resource, len(names), principal)
	}
	eff := safety.Effect{
		Action:            safety.ActionDelete,
		Target:            target,
		Cluster:           cluster,
		ConflictDetection: len(names) == 1,
		Reversible:        false,
		Destructive:       true,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		deleted := make([]string, 0, len(names))
		for _, name := range names {
			if err := client.Delete(ctx, prtbKind.GVR, backingNS, name, "", false); err != nil {
				return runtime.Result{}, err
			}
			deleted = append(deleted, name)
		}
		return runtime.Result{Kind: "ProjectRoleTemplateBindingDelete", Data: map[string]any{"deleted": deleted}}, nil
	}
	return eff, exec, nil
}
