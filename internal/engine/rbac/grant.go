package rbac

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type grantCmd struct {
	clients engine.ClusterClientFactory
}

// NewGrant builds `grant <subject> --role <r> (--project|--cluster|--global)`
// (mutating): creates the scope's role binding. Subjects resolve per the
// rbac resolution rules; a fully qualified principal with no user object
// takes the provisioning path (userPrincipalName only + preview warning).
// Duplicate detection is a best-effort client-side list (check/use race
// accepted and documented in the design).
func NewGrant(clients engine.ClusterClientFactory) runtime.Command {
	return &grantCmd{clients: clients}
}

func (grantCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "grant",
		Usage:       "Grant a role to a user or group at project, cluster, or global scope",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "subject", Type: "string", Required: true, Desc: "user/group name, id, or fully qualified principal"}},
		Flags: []runtime.ArgSpec{
			{Name: "role", Type: "string", Required: true, Desc: "role for the scope: a roletemplate `id` (project/cluster) or globalrole `id` (global)"},
			{Name: "project", Type: "string", Desc: "project scope: project `id|name`"},
			{Name: "cluster", Type: "string", Desc: "cluster scope, or the cluster qualifying --project"},
			{Name: "global", Type: "bool", Desc: "global scope"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type; recognized principal forms are inferred, other fully qualified forms require this flag"},
		},
		Examples: []string{
			"rancher grant u-abc123 --role project-member --project p-x --cluster c-x --apply --yes",
			"rancher grant jane --role cluster-owner --cluster c-x --apply --yes",
			"rancher grant okta_group://gid --role admin --global --apply --yes",
		},
		Destructive: "true",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c grantCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <subject> is required").
			WithHint("provide a user/group name, id, or fully qualified principal")
	}
	role := in.String("role")
	if role == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--role is required").
			WithHint("pass --role <roletemplate-id|globalrole-id>")
	}
	sub, err := ParseSubject(args[0], in.String("type"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	scope, err := ResolveScope(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	rs, err := ResolveSubject(ctx, c.clients, sub)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	bindKind, _ := kinds.Lookup(scope.KindKey)

	// Best-effort duplicate preflight (design: check/use race accepted).
	list, err := client.List(ctx, bindKind.GVR, scope.NS, "")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	for i := range list.Items {
		b := &list.Items[i]
		r, _, _ := unstructured.NestedString(b.Object, scope.RoleField)
		if r != role || !bindingMatchesSubject(b, rs, sub, true) {
			continue
		}
		hint := "nothing to grant; use revoke to remove it first if re-granting"
		// Alias-only match: the binding carries a different principal alias
		// than the caller supplied. Revoke matches with aliasPrincipals=false
		// and won't find it by the same spelling — surface the exact
		// principal stored in the binding so the caller can revoke precisely.
		if !bindingMatchesSubject(b, rs, sub, false) {
			if upn, _, _ := unstructured.NestedString(b.Object, "userPrincipalName"); upn != "" {
				hint = fmt.Sprintf("alias-matched binding uses principal %q; revoke %s --role %s ... removes it", upn, upn, role)
			}
		}
		return safety.Effect{}, nil, clierr.New(clierr.CodeConflict,
			fmt.Sprintf("subject already holds role %q at this scope (binding %s)", role, b.GetName())).
			WithHint(hint)
	}

	prefix := role + "-"
	if scope.Kind == ScopeGlobal {
		prefix = "grb-" // rancher's own GRB generateName convention
	}
	fields := map[string]any{scope.RoleField: role}
	switch scope.Kind {
	case ScopeProject:
		fields["projectName"] = scope.Cluster + ":" + scope.ProjectID
	case ScopeCluster:
		fields["clusterName"] = scope.Cluster
	}
	if sub.Kind == SubjectGroup {
		fields["groupPrincipalName"] = rs.Principal
	} else if rs.Provision {
		fields["userPrincipalName"] = rs.Principal
	} else {
		fields["userName"] = rs.UserID
		fields["userPrincipalName"] = rs.Principal
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       bindKind.K8sKind,
		"metadata":   map[string]any{"generateName": prefix, "namespace": scope.NS},
	}}
	for k, v := range fields {
		_ = unstructured.SetNestedField(obj.Object, v, k)
	}
	if scope.Kind == ScopeGlobal {
		_ = unstructured.SetNestedField(obj.Object, prefix, "metadata", "generateName")
		unstructured.RemoveNestedField(obj.Object, "metadata", "namespace")
	}

	effCluster := scope.Cluster
	if scope.Kind == ScopeGlobal {
		effCluster = engine.LocalCluster
	}
	eff, exec, err := engine.PlanCreate(ctx, client, bindKind.GVR, scope.NS, obj, effCluster, bindKind.K8sKind, bindKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// generateName: the dry-run name is a throwaway server suffix; show
	// the prefix pattern (same reasoning as project add-member did).
	eff.Target = bindKind.GVR.Resource + "/" + prefix + "*"
	eff.Diff = grantDiff(fields)
	if rs.Provision {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf(
			"no user exists for principal %q — Rancher will provision user %q when the binding applies",
			rs.Principal, ProvisionedUserID(rs.Principal)))
	}
	return eff, exec, nil
}

// grantDiff renders the authorization fields as create-diff entries:
// JSON-pointer paths on the binding, From nil, To the submitted value —
// EXACTLY the field set grant writes, nothing else (no /metadata,
// /apiVersion, /kind), sorted for stable output.
func grantDiff(fields map[string]any) []safety.DiffEntry {
	paths := make([]string, 0, len(fields))
	for k := range fields {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	out := make([]safety.DiffEntry, 0, len(paths))
	for _, k := range paths {
		out = append(out, safety.DiffEntry{Path: "/" + k, From: nil, To: fields[k]})
	}
	return out
}

// bindingMatchesSubject reports whether an existing binding is for the
// same subject. The base rule (grant AND revoke): userName equals the
// resolved id, or userPrincipalName/groupPrincipalName equals the
// principal AS GIVEN by the caller (sub.Principal — what the spec
// promises; NOT rs.Principal, which bare-name resolution rewrites to
// preferredPrincipal). With aliasPrincipals (grant's duplicate detection
// only), any of the resolved user's principalIds also matches, so a
// principal-only binding under a different identity alias of the same
// user is still a duplicate.
func bindingMatchesSubject(b *unstructured.Unstructured, rs ResolvedSubject, sub Subject, aliasPrincipals bool) bool {
	if sub.Kind == SubjectGroup {
		gp, _, _ := unstructured.NestedString(b.Object, "groupPrincipalName")
		return gp == sub.Principal
	}
	un, _, _ := unstructured.NestedString(b.Object, "userName")
	up, _, _ := unstructured.NestedString(b.Object, "userPrincipalName")
	if rs.UserID != "" && un == rs.UserID {
		return true
	}
	if sub.Principal != "" && up == sub.Principal {
		return true
	}
	if aliasPrincipals && up != "" {
		for _, p := range rs.PrincipalIDs {
			if up == p {
				return true
			}
		}
	}
	return false
}
