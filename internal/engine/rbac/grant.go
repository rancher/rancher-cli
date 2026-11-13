package rbac

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// accepted).
func NewGrant(clients engine.ClusterClientFactory) runtime.Command {
	return &grantCmd{clients: clients}
}

func (grantCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "grant",
		Usage: "Grant a role to a user or group at project, cluster, or global scope",
		Desc: "Checks that the role exists for the scope (a roletemplate for project or cluster scope, " +
			"a globalrole for global scope), finds the subject, checks that the subject does not already " +
			"have the role there, and previews creating the binding. " +
			"List the available roles with: `rancher get roletemplates` (project and cluster scopes) " +
			"or `rancher get globalroles` (global scope). " +
			"When Rancher refuses the read of users, the command warns and uses the subject exactly as typed if it is `local://<id>` or an id that looks like u-lpnxw " +
			"(u-, m- or user- followed by at least five characters from bcdfghjklmnpqrstvwxz2456789). " +
			"A username, a display name or an external principal can be found only by reading users: pass the user id instead.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "subject", Type: "string", Required: true, Desc: "user/group name, id, or fully qualified principal"}},
		Flags: []runtime.ArgSpec{
			{Name: "role", Type: "string", Required: true, Desc: "role for the scope: a roletemplate `id` (project/cluster) or globalrole `id` (global); discover with: `rancher get roletemplates` (project/cluster) or `rancher get globalroles` (global)"},
			runtime.ProjectFlag(false, "project scope: project `id|name`"),
			{Name: "cluster", Type: "string", Desc: "cluster scope, or the cluster the --project is in"},
			{Name: "global", Type: "bool", Desc: "global scope"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type, user or group; the CLI infers it from a principal prefix it recognizes, and requires it for any other fully qualified principal"},
		},
		Examples: []string{
			"rancher grant u-abc123 --project p-x --cluster c-x --role project-member --apply --yes",
			"rancher grant jane --cluster c-x --role cluster-owner --apply --yes",
			"rancher grant okta_group://gid --global --role admin --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// LiveSource hands the surface grant's source: --role completes the
// roles of the scope typed on the line, leaving out role templates marked
// locked at project and cluster scope and, at global scope, global roles
// that inherit a locked role template or one that does not exist, and the
// subject positional the user subjects unless --type group is typed; every
// other site is declined.
func (grantCmd) LiveSource() completion.Source { return complete.GrantSource() }

// Plan verifies the role, resolves the subject, checks for a duplicate binding,
// and plans a create. Preflight error policy: role verification (checkRole)
// degrades on non-cancellation, non-NotFound errors - the server enforces the
// role constraint regardless, so blocking an RBAC-restricted granter from an
// unreadable role object would be worse than a missed client-side check. Subject
// resolution (ResolveSubject) is fatal on any error, except that a user id the
// server refuses to read is used as typed, with a warning. The duplicate
// check degrades only when the server refuses to list the bindings, with a
// warning; any other list error is fatal.
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
	var warnings []string
	if rs.UserReadRefused {
		warnings = append(warnings, userReadRefusedWarning(rs))
	}

	// Best-effort duplicate preflight (a check/use race is accepted).
	list, err := client.List(ctx, bindKind.GVR, scope.NS, "")
	if err != nil {
		// A caller who may create bindings but not list them (the server
		// refused the read) continues: the server-side dry-run create is the
		// only check. Any other failure stays fatal, and cancellation always
		// propagates.
		if ctx.Err() != nil {
			return safety.Effect{}, nil, ctx.Err()
		}
		if !isRefusedRead(err) {
			return safety.Effect{}, nil, err
		}
		warnings = append(warnings, bindingListRefusedWarning)
		list = &unstructured.UnstructuredList{}
	}
	for i := range list.Items {
		b := &list.Items[i]
		r, _, _ := unstructured.NestedString(b.Object, scope.RoleField)
		if r != role || !bindingMatchesSubject(b, rs, sub, true) {
			continue
		}
		msg := fmt.Sprintf("subject already has role %q at this scope (binding %s)", role, b.GetName())
		var hint clierr.Command
		hinted := false
		// Alias-only match: the binding carries a different principal alias
		// than the caller supplied. Revoke matches with aliasPrincipals=false
		// and won't find it by the same spelling - name the exact principal
		// stored in the binding, and the revoke command that removes it, so
		// the caller can act precisely.
		if !bindingMatchesSubject(b, rs, sub, false) {
			if upn, _, _ := unstructured.NestedString(b.Object, "userPrincipalName"); upn != "" {
				msg += fmt.Sprintf("; that binding uses another principal of the same user, %q", upn)
				hint, hinted = aliasRevokeHint(upn, role, scope), true
				if hint.OK() {
					msg += "; the hint previews the revoke that removes it: add --apply --yes after reading the preview"
				}
			}
		}
		if !hinted {
			msg += "; nothing to grant; to grant it again, revoke it first"
		}
		e := clierr.New(clierr.CodeConflict, msg)
		if hinted {
			e = e.WithHintCommand(hint)
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
		// An id without local:// that Rancher refused to read has no principal;
		// an empty userPrincipalName is never sent.
		if rs.Principal != "" {
			fields["userPrincipalName"] = rs.Principal
		}
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
	eff, exec, err := engine.PlanCreate(ctx, client, bindKind.GVR, scope.NS, obj, bindKind.Redaction(), effCluster, bindKind.Name, bindKind.K8sKind, bindKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, errorWithWarnings(err, warnings)
	}
	// generateName: the dry-run name is a throwaway server suffix; show
	// the prefix pattern (same reasoning as project add-member did). The
	// committed success text is unaffected - PlanCreate's exec closure
	// derives it from the REAL create's returned object name, never from
	// this preview override.
	eff.Target = bindKind.GVR.Resource + "/" + prefix + "*"
	eff.DisplayName = prefix + "*"
	if rs.Provision {
		eff.Warnings = append(eff.Warnings, fmt.Sprintf(
			"no user exists for principal %q; Rancher will provision user %q when the binding is created",
			rs.Principal, ProvisionedUserID(rs.Principal)))
	}
	eff.Warnings = append(eff.Warnings, warnings...)
	return eff, exec, nil
}

// errorWithWarnings returns err carrying warnings when it is a *clierr.Error,
// so a failed dry-run still says which check the command skipped; any other
// error is returned as it is.
func errorWithWarnings(err error, warnings []string) error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok && len(warnings) > 0 {
		return ce.WithWarnings(warnings...)
	}
	return err
}

// bindingListRefusedWarning is the preview warning for a grant when the check
// for an existing binding was skipped because Rancher refused to list the
// existing bindings.
const bindingListRefusedWarning = "cannot list the existing bindings (Rancher refused the read); " +
	"the CLI could not check whether the subject already has this role; the server-side dry-run checks only that Rancher accepts the create"

// candidatesWithFallback returns every item whose hay(i) contains needle
// (case-insensitive substring, in list order) as candidates, or, when none
// does, the whole pool. An empty needle matches every item, so the whole
// pool is returned. The caller cuts the list to CandidateLimit with
// WithCandidatesCut, which records how many entries it left out.
func candidatesWithFallback(n int, hay func(i int) string, cand func(i int) clierr.Candidate, needle string) []clierr.Candidate {
	lneedle := strings.ToLower(needle)
	var near []clierr.Candidate
	for i := range n {
		if strings.Contains(strings.ToLower(hay(i)), lneedle) {
			near = append(near, cand(i))
		}
	}
	if len(near) > 0 {
		return near
	}
	pool := make([]clierr.Candidate, n)
	for i := range n {
		pool[i] = cand(i)
	}
	return pool
}

// roleCandidates returns the candidates for a role error: the roles of pool
// whose id or name contains needle, or the whole pool when none does. An
// empty needle returns the whole at-scope pool (used for scope-mismatch
// errors where any valid at-scope role is equally useful).
func roleCandidates(pool []clierr.Candidate, needle string) []clierr.Candidate {
	return candidatesWithFallback(len(pool),
		func(i int) string { return pool[i].ID + " " + pool[i].Name },
		func(i int) clierr.Candidate { return pool[i] },
		needle)
}

// checkRole verifies that role exists at the given scope before the mutation
// is planned. On NOT_FOUND it returns a structured error with nearby
// candidates (the roletemplates whose context is the scope's and that are not
// locked, or the globalroles Rancher accepts in a binding, for global scope)
// and a discovery hint. On a context mismatch, or a
// missing context, the full at-scope pool is returned directly - the
// name exists but the template cannot bind at this scope, so nearness
// filtering would be misleading. A non-cancellation,
// non-NotFound Get failure degrades gracefully - an RBAC-restricted granter
// must not be blocked by an unreadable role object. Caller-context
// cancellation always propagates.
func checkRole(ctx context.Context, client engine.ClusterClient, role string, scope ScopeKind) error {
	roleKindKey, _ := complete.RoleKind(complete.Scope(scope))
	roleKind, _ := kinds.Lookup(roleKindKey)

	obj, err := client.Get(ctx, roleKind.GVR, "", role)
	if err == nil {
		// Context check for non-global scopes: Rancher's webhook binds a
		// RoleTemplate only when its context is exactly the binding's
		// ("project" for a ProjectRoleTemplateBinding, "cluster" for a
		// ClusterRoleTemplateBinding); a template with another context, or
		// with none, is refused at the dry-run. Catch it here with the
		// usable pool so the raw webhook error does not surface.
		if scope != ScopeGlobal {
			if rtCtx, _, _ := unstructured.NestedString(obj.Object, "context"); rtCtx != string(scope) {
				pool, perr := rolePoolAtScope(ctx, client, roleKind, scope)
				if perr != nil {
					return perr
				}
				msg := fmt.Sprintf("role %s is a %s role, not usable at %s scope", role, rtCtx, scope)
				if rtCtx == "" {
					msg = fmt.Sprintf("role %s has no context; Rancher binds only roletemplates whose context is %s", role, scope)
				}
				return clierr.New(clierr.CodeNotFound, msg).
					WithCandidatesCut(roleCandidates(pool, ""), CandidateLimit).
					WithHintCommand(clierr.NewCommand("get roletemplates"))
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

	hint := clierr.NewCommand("get roletemplates")
	if scope == ScopeGlobal {
		hint = clierr.NewCommand("get globalroles")
	}
	pool, perr := rolePoolAtScope(ctx, client, roleKind, scope)
	if perr != nil {
		return perr
	}
	return clierr.New(clierr.CodeNotFound, "role not found: "+role).
		WithCandidatesCut(roleCandidates(pool, role), CandidateLimit).
		WithHintCommand(hint)
}

// rolePoolAtScope lists the role kind and returns as Candidates the roles
// Rancher accepts in a new binding at scope: at global scope the globalroles
// that inherit no locked roletemplate and no roletemplate that does not
// exist (the roletemplates are listed too, only when some globalrole names an
// inherited one; when that list fails, only the globalroles that name none
// remain), else the roletemplates whose context equals the scope and that are
// not locked (a template with another context, or none, is refused by the
// webhook). Cancellation propagates; any other list failure degrades to
// (nil, nil) so an RBAC-restricted granter is not blocked.
func rolePoolAtScope(ctx context.Context, client engine.ClusterClient, roleKind kinds.Kind, scope ScopeKind) ([]clierr.Candidate, error) {
	list, err := client.List(ctx, roleKind.GVR, "", "")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // degraded
	}
	items := list.Items
	if scope == ScopeGlobal {
		roles := make([]map[string]any, len(items))
		for i := range items {
			roles[i] = items[i].Object
		}
		bindable, err := kinds.BindableGlobalRoles(roles, func() ([]map[string]any, error) {
			return listRoleTemplateObjects(ctx, client)
		})
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		items = make([]unstructured.Unstructured, 0, len(bindable))
		for _, role := range bindable {
			items = append(items, unstructured.Unstructured{Object: role})
		}
	}
	cands := make([]clierr.Candidate, 0, len(items))
	for i := range items {
		rt := &items[i]
		if scope != ScopeGlobal {
			rtCtx, _, _ := unstructured.NestedString(rt.Object, "context")
			if rtCtx != string(scope) || kinds.RoleTemplateLocked(rt.Object) {
				continue
			}
		}
		display, _, _ := unstructured.NestedString(rt.Object, "displayName")
		cands = append(cands, clierr.Candidate{ID: rt.GetName(), Name: display})
	}
	return cands, nil
}

// listRoleTemplateObjects lists every roletemplate, whatever its context:
// Rancher reads the roletemplates a globalrole inherits by name alone.
func listRoleTemplateObjects(ctx context.Context, client engine.ClusterClient) ([]map[string]any, error) {
	k, ok := kinds.Lookup("roletemplate")
	if !ok {
		return nil, errors.New("roletemplate kind is not registered")
	}
	list, err := client.List(ctx, k.GVR, "", "")
	if err != nil {
		return nil, err
	}
	objs := make([]map[string]any, len(list.Items))
	for i := range list.Items {
		objs[i] = list.Items[i].Object
	}
	return objs, nil
}

// bindingMatchesSubject reports whether an existing binding is for the same
// subject. The base rule (grant AND revoke): userName equals the resolved id,
// or userPrincipalName/groupPrincipalName equals the principal AS GIVEN by the
// caller (sub.Principal, NOT rs.Principal, which bare-name resolution rewrites
// to preferredPrincipal). With aliasPrincipals (grant's duplicate detection
// only), any of the resolved user's principalIds also matches, so a
// principal-only binding under a different identity alias of the same user is
// still a duplicate.
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

// aliasRevokeHint is the preview of the revoke of the binding that holds the
// principal upn, with the flags that name the scope.
func aliasRevokeHint(upn, role string, scope Scope) clierr.Command {
	return clierr.NewCommand("revoke").Arg(upn).FlagValue("--role", role).With(scope.Flags()).Flag("--dry-run")
}
