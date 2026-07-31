package rbac

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
		Name:  "grant",
		Usage: "Grant a role to a user or group at project, cluster, or global scope",
		Desc: "Resolves the subject and verifies the role exists at the declared scope " +
			"(roletemplate for project/cluster, globalrole for global), then checks for an " +
			"existing duplicate binding and plans a create. " +
			"Discover available roles: rancher get roletemplates (project/cluster scopes) " +
			"or rancher get globalroles (global scope).",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "subject", Type: "string", Required: true, Desc: "user/group name, id, or fully qualified principal"}},
		Flags: []runtime.ArgSpec{
			{Name: "role", Type: "string", Required: true, Desc: "role for the scope: a roletemplate `id` (project/cluster) or globalrole `id` (global); discover with: rancher get roletemplates (project/cluster) or rancher get globalroles (global)"},
			{Name: "project", Type: "string", Desc: "project scope: project `id|name`"},
			{Name: "cluster", Type: "string", Desc: "cluster scope, or the cluster qualifying --project"},
			{Name: "global", Type: "bool", Desc: "global scope"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type; recognized principal forms are inferred, other fully qualified forms require this flag"},
		},
		Examples: []string{
			"rancher grant u-abc123 --project p-x --cluster c-x --role project-member --apply --yes",
			"rancher grant jane --cluster c-x --role cluster-owner --apply --yes",
			"rancher grant okta_group://gid --global --role admin --apply --yes",
		},
		Destructive: "true",
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

	if err := checkRole(ctx, client, role, scope.Kind); err != nil {
		return safety.Effect{}, nil, err
	}

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

// checkRole verifies that role exists at the given scope before the mutation is
// planned. On NOT_FOUND it returns a structured error with nearby candidates
// (roletemplates filtered by context == scope, or globalroles for global scope)
// and a discovery hint. A non-cancellation, non-NotFound Get failure degrades
// gracefully — an RBAC-restricted granter must not be blocked by an unreadable
// role object, so validation is skipped and the grant proceeds. Caller-context
// cancellation always propagates.
func checkRole(ctx context.Context, client engine.ClusterClient, role string, scope ScopeKind) error {
	roleKindKey := "roletemplate"
	if scope == ScopeGlobal {
		roleKindKey = "globalrole"
	}
	roleKind, _ := kinds.Lookup(roleKindKey)

	_, err := client.Get(ctx, roleKind.GVR, "", role)
	if err == nil {
		return nil
	}
	if !clierr.IsNotFound(err) {
		// Degrade: non-cancellation errors skip role validation so an
		// RBAC-restricted granter is not blocked by unreadable role objects.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	hint := "run: rancher get roletemplates"
	if scope == ScopeGlobal {
		hint = "run: rancher get globalroles"
	}
	list, lerr := client.List(ctx, roleKind.GVR, "", "")
	if lerr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Degraded list: return NOT_FOUND with hint but no candidates.
		return clierr.New(clierr.CodeNotFound, "role not found: "+role).WithHint(hint)
	}

	var pool []*unstructured.Unstructured
	for i := range list.Items {
		rt := &list.Items[i]
		if scope != ScopeGlobal {
			rtContext, _, _ := unstructured.NestedString(rt.Object, "context")
			if rtContext != string(scope) {
				continue
			}
		}
		pool = append(pool, rt)
	}

	return clierr.New(clierr.CodeNotFound, "role not found: "+role).
		WithCandidates(nearRoleCandidates(role, pool)).
		WithHint(hint)
}

// nearRoleCandidates filters roles to case-insensitive substring matches of
// input against id and displayName, capped at candidateCap.
func nearRoleCandidates(input string, roles []*unstructured.Unstructured) []clierr.Candidate {
	needle := strings.ToLower(input)
	var out []clierr.Candidate
	for _, r := range roles {
		if len(out) == candidateCap {
			break
		}
		display, _, _ := unstructured.NestedString(r.Object, "displayName")
		hay := strings.ToLower(r.GetName() + " " + display)
		if strings.Contains(hay, needle) {
			out = append(out, clierr.Candidate{ID: r.GetName(), Name: display})
		}
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
