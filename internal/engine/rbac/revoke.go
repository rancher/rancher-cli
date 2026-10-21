package rbac

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// RevokeFailure identifies the single delete that failed a multi-binding
// revoke, with the underlying failure's structured code preserved.
type RevokeFailure struct {
	Name    string `json:"name"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RevokePartialFailure is revoke's execute-phase details payload: what
// this execution actually knows - deleted by this run, the failed delete,
// and never-attempted names. No claim about current server state.
type RevokePartialFailure struct {
	Deleted     []string      `json:"deleted"`
	Failed      RevokeFailure `json:"failed"`
	Unattempted []string      `json:"unattempted"`
}

// TextDetails projects what this run actually deleted, the binding it failed
// on, and what it never attempted, onto indented lines, closing with the
// standing recovery note: a partial revoke never undoes what already
// succeeded, so a re-run only needs to reach the rest.
func (p RevokePartialFailure) TextDetails() []string {
	var lines []string
	if len(p.Deleted) > 0 {
		lines = append(lines, "deleted: "+strings.Join(p.Deleted, ", "))
	}
	lines = append(lines, fmt.Sprintf("failed: %s (%s: %s)", p.Failed.Name, p.Failed.Code, p.Failed.Message))
	if len(p.Unattempted) > 0 {
		lines = append(lines, "not attempted: "+strings.Join(p.Unattempted, ", "))
	}
	lines = append(lines, "bindings already deleted stay deleted; run the same revoke command again to delete the rest")
	return lines
}

type revokeCmd struct {
	clients engine.ClusterClientFactory
}

// NewRevoke builds `revoke <subject> [--role <r>] (--project|--cluster|--global)`
// (mutating, destructive): deletes the subject's role binding(s) at the
// scope. Without --role every binding for the subject at the scope goes;
// with --role only that role's. Multi-binding deletion is sequential and
// non-atomic - previewed via an Effect warning and reported on failure
// via the RevokePartialFailure details payload.
func NewRevoke(clients engine.ClusterClientFactory) runtime.Command {
	return &revokeCmd{clients: clients}
}

func (revokeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "revoke",
		Usage: "Revoke a subject's role(s) at project, cluster, or global scope",
		Desc: "Finds the subject's bindings at the scope (only those for --role when it is given), " +
			"checks each deletion with a server-side dry-run, and previews deleting them one at a time. " +
			"List the available roles with: rancher get roletemplates (project and cluster scopes) " +
			"or rancher get globalroles (global scope).",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "subject", Type: "string", Required: true, Desc: "user/group name, id, or fully qualified principal"}},
		Flags: []runtime.ArgSpec{
			{Name: "role", Type: "string", Desc: "revoke only this role; without it, every role of the subject at the scope is revoked"},
			runtime.ProjectFlag(false, "project scope: project `id|name`"),
			{Name: "cluster", Type: "string", Desc: "cluster scope, or the cluster the --project is in"},
			{Name: "global", Type: "bool", Desc: "global scope"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type, user or group; the CLI infers it from a principal prefix it recognizes, and requires it for any other fully qualified principal"},
		},
		Examples: []string{
			"rancher revoke u-abc123 --project p-x --cluster c-x --apply --yes",
			"rancher revoke jane --cluster c-x --role cluster-owner --apply --yes",
			"rancher revoke okta_group://gid --global --role admin --apply --yes",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// LiveSource hands the surface revoke's source: --role completes the
// roles of the scope typed on the line, the subject positional the user
// subjects unless --type group is typed; every other site is declined.
func (revokeCmd) LiveSource() completion.Source { return complete.GrantRevokeSource() }

func (c revokeCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"argument <subject> is required; provide a user/group name, id, or fully qualified principal")
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
	list, err := client.List(ctx, bindKind.GVR, scope.NS, "")
	if err != nil {
		return safety.Effect{}, nil, err
	}

	roleFilter := in.String("role")
	var names []string
	rvByName := map[string]string{}
	heldRoles := map[string]bool{}
	for i := range list.Items {
		b := &list.Items[i]
		// No aliasPrincipals: revoke matches more narrowly -
		// a provisioned binding is revocable by the SAME principal
		// string that granted it, plus resolved-userName matches.
		if !bindingMatchesSubject(b, rs, sub, false) {
			continue
		}
		r, _, _ := unstructured.NestedString(b.Object, scope.RoleField)
		heldRoles[r] = true
		if roleFilter != "" && r != roleFilter {
			continue
		}
		names = append(names, b.GetName())
		rvByName[b.GetName()] = b.GetResourceVersion()
	}
	sort.Strings(names)

	if len(names) == 0 {
		msg := fmt.Sprintf("no %s for subject %q at this scope", bindKind.GVR.Resource, args[0])
		var hint string
		held := slices.DeleteFunc(slices.Sorted(maps.Keys(heldRoles)), func(r string) bool { return r == "" })
		var heldCands []clierr.Candidate
		if len(held) > 0 {
			msg = fmt.Sprintf("no %s with that role for subject %q at this scope; the subject has: %s", bindKind.GVR.Resource, args[0], strings.Join(held, ", "))
			for _, r := range held {
				heldCands = append(heldCands, clierr.Candidate{ID: r, Name: r})
			}
		} else {
			// bindKind.Name, NOT bindKind.GVR.Resource: kinds.Lookup
			// matches Name/Key/Aliases only, and the plural GVR
			// resource string is not among them for prtb/crtb - a
			// GVR-based hint would name a command that doesn't exist.
			// Scope the hint so it returns the same binding set revoke
			// was searching; without a scope the get falls back to an
			// all-namespaces/all-clusters list.
			switch scope.Kind {
			case ScopeProject:
				hint = fmt.Sprintf("rancher get %s --namespace %s -o json", bindKind.Name, clierr.ShellQuote(scope.NS))
			case ScopeCluster:
				hint = fmt.Sprintf("rancher get %s --cluster %s -o json", bindKind.Name, clierr.ShellQuote(scope.Cluster))
			default: // ScopeGlobal - no namespace qualifier needed
				hint = "rancher get " + bindKind.Name + " -o json"
			}
		}
		e := clierr.New(clierr.CodeNotFound, msg)
		if hint != "" {
			e = e.WithHint(hint)
		}
		if len(heldCands) > 0 {
			e = e.WithCandidates(heldCands)
		}
		return safety.Effect{}, nil, e
	}

	// Admission preview: per-object dry-run delete with rv preconditions.
	for _, name := range names {
		if err := client.Delete(ctx, bindKind.GVR, scope.NS, name, rvByName[name], true); err != nil {
			return safety.Effect{}, nil, err
		}
	}

	target := bindKind.GVR.Resource + "/" + names[0]
	displayName := names[0]
	var rv string
	var warnings []string
	if len(names) > 1 {
		target = fmt.Sprintf("%s (%d) for %s", bindKind.GVR.Resource, len(names), args[0])
		displayName = fmt.Sprintf("(%d) for %s", len(names), args[0])
		warnings = append(warnings, fmt.Sprintf(
			"%d bindings are deleted sequentially; on failure some may already be deleted", len(names)))
	} else {
		rv = rvByName[names[0]]
	}
	effCluster := scope.Cluster
	if scope.Kind == ScopeGlobal {
		effCluster = engine.LocalCluster
	}
	eff := safety.Effect{
		Action:            safety.ActionDelete,
		Target:            target,
		DisplayKind:       bindKind.Name,
		DisplayName:       displayName,
		Cluster:           effCluster,
		ResourceVersion:   rv,
		ConflictDetection: len(names) == 1,
		Reversible:        false,
		Destructive:       true,
		Warnings:          warnings,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		deleted := make([]string, 0, len(names))
		var text strings.Builder
		for i, name := range names {
			if err := client.Delete(ctx, bindKind.GVR, scope.NS, name, rvByName[name], false); err != nil {
				return runtime.Result{}, partialFailure(err, name, deleted, names[i+1:])
			}
			deleted = append(deleted, name)
			text.WriteString(runtime.MutationText(bindKind.Name, name, "deleted").Text)
		}
		final := text.String()
		return runtime.Result{
			Kind:       bindKind.K8sKind + "Delete",
			Data:       map[string]any{"deleted": deleted},
			Text:       final,
			StyledText: runtime.StyleEveryLineBold(final),
		}, nil
	}
	return eff, exec, nil
}

// partialFailure wraps a mid-sequence delete failure with the
// deleted/failed/unattempted split, preserving the underlying failure's
// code and retryability on the top-level error.
func partialFailure(err error, failedName string, deleted, unattempted []string) error {
	code, msg, retryable := clierr.CodeGeneric, err.Error(), false
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		code, msg, retryable = ce.Code, ce.Message, ce.Retryable
	}
	if deleted == nil {
		deleted = []string{}
	}
	if unattempted == nil {
		unattempted = []string{}
	}
	return clierr.New(code,
		fmt.Sprintf("revoke partially applied: %d deleted, failed on %q, %d not attempted"+
			"; run the same revoke command again to delete the remaining bindings",
			len(deleted), failedName, len(unattempted))).
		WithRetryable(retryable).
		WithDetails(RevokePartialFailure{
			Deleted:     deleted,
			Failed:      RevokeFailure{Name: failedName, Code: code, Message: msg},
			Unattempted: append([]string{}, unattempted...),
		})
}
