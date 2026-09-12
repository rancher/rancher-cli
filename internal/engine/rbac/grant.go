package rbac

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
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
		Desc: "Verifies the role exists at the declared scope " +
			"(roletemplate for project/cluster, globalrole for global), resolves the subject, " +
			"then checks for an existing duplicate binding and plans a create. " +
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
		Shape:       runtime.ShapeText,
	}
}

// LiveSource hands the surface grant's source: --role completes the
// roles of the scope typed on the line, the subject positional the user
// subjects unless --type group is typed; every other site is declined.
func (grantCmd) LiveSource() completion.Source { return complete.GrantRevokeSource() }

// Plan verifies the role, resolves the subject, checks for a duplicate binding,
// and plans a create. Preflight error policy: role verification (checkRole)
// degrades on non-cancellation, non-NotFound errors - the server enforces the
// role constraint regardless, so blocking an RBAC-restricted granter from an
// unreadable role object would be worse than a missed client-side check. Subject
// resolution (ResolveSubject) is fatal on any error: the binding requires a
// resolved subject, and there is no safe fallback without one.
func (c grantCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"argument <subject> is required; provide a user/group name, id, or fully qualified principal")
	}
	role := in.String("role")
	if role == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--role is required")
	}
	sub, err := ParseSubject(args[0], in.String("type"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	scope, err := ResolveScope(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	bindKind, _ := kinds.Lookup(scope.KindKey)

	// checkRole runs before ResolveSubject and the duplicate-binding List:
	// a bad role fails immediately without a subject round-trip. A role
	// deleted out-of-band while a stale binding exists surfaces as
	// NOT_FOUND - a grant of a nonexistent role cannot proceed regardless;
	// a CONFLICT naming a role that no longer exists misleads.
	if err := checkRole(ctx, client, role, scope.Kind); err != nil {
		return safety.Effect{}, nil, err
	}

	rs, err := ResolveSubject(ctx, c.clients, sub)
	if err != nil {
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
		msg := fmt.Sprintf("subject already holds role %q at this scope (binding %s)", role, b.GetName())
		var hint string
		// Alias-only match: the binding carries a different principal alias
		// than the caller supplied. Revoke matches with aliasPrincipals=false
		// and won't find it by the same spelling - name the exact principal
		// stored in the binding, and the revoke command that removes it, so
		// the caller can act precisely.
		if !bindingMatchesSubject(b, rs, sub, false) {
			if upn, _, _ := unstructured.NestedString(b.Object, "userPrincipalName"); upn != "" {
				msg += fmt.Sprintf("; alias-matched binding uses principal %q", upn)
				hint = fmt.Sprintf("rancher revoke %s --role %s%s", clierr.ShellQuote(upn), clierr.ShellQuote(role), scope.Flags())
			}
		}
		if hint == "" {
			msg += "; nothing to grant; use revoke to remove it first if re-granting"
		}
		e := clierr.New(clierr.CodeConflict, msg)
		if hint != "" {
			e = e.WithHint(hint)
		}
		return safety.Effect{}, nil, e
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
	eff, exec, err := engine.PlanCreate(ctx, client, bindKind.GVR, scope.NS, obj, effCluster, bindKind.Name, bindKind.K8sKind, bindKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// generateName: the dry-run name is a throwaway server suffix; show
	// the prefix pattern (same reasoning as project add-member did). The
	// committed success text is unaffected - PlanCreate's exec closure
	// derives it from the REAL create's returned object name, never from
	// this preview override.
	eff.Target = bindKind.GVR.Resource + "/" + prefix + "*"
	eff.DisplayName = prefix + "*"
	eff.Diff = grantDiff(fields)
	if rs.Provision {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf(
			"no user exists for principal %q - Rancher will provision user %q when the binding applies",
			rs.Principal, ProvisionedUserID(rs.Principal)))
	}
	return eff, exec, nil
}

// grantDiff renders the authorization fields as create-diff entries:
// JSON-pointer paths on the binding, From nil, To the submitted value -
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
		out = append(out, safety.DiffEntry{Path: engine.PointerFor([]string{k}), From: nil, To: fields[k]})
	}
	return out
}

// candidatesWithFallback builds a candidate slice in a single pass: it
// simultaneously collects near-matches (case-insensitive substring of needle
// in hay(i)) up to candidateCap and the first candidateCap items via cand(i)
// as a fallback. Returns near-matches if any exist, else the fallback. An
// empty needle matches every item, so the capped pool is returned directly.
func candidatesWithFallback(n int, hay func(i int) string, cand func(i int) clierr.Candidate, needle string) []clierr.Candidate {
	lneedle := strings.ToLower(needle)
	var near, fallback []clierr.Candidate
	for i := range n {
		if len(fallback) < candidateCap {
			fallback = append(fallback, cand(i))
		}
		if len(near) < candidateCap && strings.Contains(strings.ToLower(hay(i)), lneedle) {
			near = append(near, cand(i))
		}
		if len(near) == candidateCap && len(fallback) == candidateCap {
			break
		}
	}
	if len(near) > 0 {
		return near
	}
	return fallback
}

// roleCandidates builds the candidate slice for a role error. An empty needle
// matches every item, so the capped at-scope pool is returned directly (used
// for scope-mismatch errors where any valid at-scope role is equally useful).
func roleCandidates(pool []clierr.Candidate, needle string) []clierr.Candidate {
	return candidatesWithFallback(len(pool),
		func(i int) string { return pool[i].ID + " " + pool[i].Name },
		func(i int) clierr.Candidate { return pool[i] },
		needle)
}

// checkRole verifies that role exists at the given scope before the mutation
// is planned. On NOT_FOUND it returns a structured error with nearby
// candidates (roletemplates whose context is empty or matches scope, or
// globalroles for global scope) and a discovery hint. On scope mismatch the
// full at-scope pool is returned directly - the name is right, the scope is
// wrong, so nearness filtering would be misleading. A non-cancellation,
// non-NotFound Get failure degrades gracefully - an RBAC-restricted granter
// must not be blocked by an unreadable role object. Caller-context
// cancellation always propagates.
func checkRole(ctx context.Context, client engine.ClusterClient, role string, scope ScopeKind) error {
	roleKindKey, _ := complete.RoleKind(complete.Scope(scope))
	roleKind, _ := kinds.Lookup(roleKindKey)

	obj, err := client.Get(ctx, roleKind.GVR, "", role)
	if err == nil {
		// Scope check for non-global scopes: a roletemplate whose top-level
		// context field is non-empty and differs from the requested scope is
		// not usable there - catch it here so the raw webhook error does not
		// surface. Empty-context objects always pass (legacy/inherited roles).
		if scope != ScopeGlobal {
			if rtCtx, _, _ := unstructured.NestedString(obj.Object, "context"); rtCtx != "" && rtCtx != string(scope) {
				hint := "rancher get roletemplates"
				pool, perr := rolePoolAtScope(ctx, client, roleKind, scope)
				if perr != nil {
					return perr
				}
				return clierr.New(clierr.CodeNotFound,
					fmt.Sprintf("role %s is a %s role, not usable at %s scope", role, rtCtx, scope)).
					WithCandidates(roleCandidates(pool, "")).
					WithHint(hint)
			}
		}
		return nil
	}
	if !clierr.IsNotFound(err) {
		// Degrade: see Plan's preflight error policy - the server enforces
		// the role constraint; an unreadable role object must not block a
		// legitimately permitted granter. Cancellation always propagates.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}

	hint := "rancher get roletemplates"
	if scope == ScopeGlobal {
		hint = "rancher get globalroles"
	}
	pool, perr := rolePoolAtScope(ctx, client, roleKind, scope)
	if perr != nil {
		return perr
	}
	return clierr.New(clierr.CodeNotFound, "role not found: "+role).
		WithCandidates(roleCandidates(pool, role)).
		WithHint(hint)
}

// rolePoolAtScope lists the role kind and returns all roles whose context is
// empty (valid at every scope) or equals scope as Candidates. Cancellation
// propagates; any other list failure degrades to (nil, nil) so an
// RBAC-restricted granter is not blocked.
func rolePoolAtScope(ctx context.Context, client engine.ClusterClient, roleKind kinds.Kind, scope ScopeKind) ([]clierr.Candidate, error) {
	list, err := client.List(ctx, roleKind.GVR, "", "")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // degraded
	}
	cands := make([]clierr.Candidate, 0, len(list.Items))
	for i := range list.Items {
		rt := &list.Items[i]
		if scope != ScopeGlobal {
			rtCtx, _, _ := unstructured.NestedString(rt.Object, "context")
			if rtCtx != "" && rtCtx != string(scope) {
				continue
			}
		}
		display, _, _ := unstructured.NestedString(rt.Object, "displayName")
		cands = append(cands, clierr.Candidate{ID: rt.GetName(), Name: display})
	}
	return cands, nil
}

// bindingMatchesSubject reports whether an existing binding is for the
// same subject. The base rule (grant AND revoke): userName equals the
// resolved id, or userPrincipalName/groupPrincipalName equals the
// principal AS GIVEN by the caller (sub.Principal - what the spec
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
		if slices.Contains(rs.PrincipalIDs, up) {
			return true
		}
	}
	return false
}
