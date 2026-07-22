package rbac

import (
	"context"
	"errors"
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

// RevokeFailure identifies the single delete that failed a multi-binding
// revoke, with the underlying failure's structured code preserved.
type RevokeFailure struct {
	Name    string `json:"name"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RevokePartialFailure is revoke's execute-phase details payload: what
// this execution actually knows — deleted by this run, the failed delete,
// and never-attempted names. No claim about current server state.
type RevokePartialFailure struct {
	Deleted     []string      `json:"deleted"`
	Failed      RevokeFailure `json:"failed"`
	Unattempted []string      `json:"unattempted"`
}

type revokeCmd struct {
	clients engine.ClusterClientFactory
}

// NewRevoke builds `revoke <subject> [--role <r>] (--project|--cluster|--global)`
// (mutating, destructive): deletes the subject's role binding(s) at the
// scope. Without --role every binding for the subject at the scope goes;
// with --role only that role's. Multi-binding deletion is sequential and
// non-atomic — previewed via an Effect warning and reported on failure
// via the RevokePartialFailure details payload.
func NewRevoke(clients engine.ClusterClientFactory) runtime.Command {
	return &revokeCmd{clients: clients}
}

func (revokeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "revoke",
		Usage:       "Revoke a user's or group's role binding(s) at project, cluster, or global scope",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "subject", Type: "string", Required: true, Desc: "user/group name, id, or fully qualified principal"}},
		Flags: []runtime.ArgSpec{
			{Name: "role", Type: "string", Desc: "revoke only this role; absent revokes every role at the scope"},
			{Name: "project", Type: "string", Desc: "project scope: project `id|name`"},
			{Name: "cluster", Type: "string", Desc: "cluster scope, or the cluster qualifying --project"},
			{Name: "global", Type: "bool", Desc: "global scope"},
			{Name: "type", Type: "string", Enum: []string{"user", "group"}, Desc: "subject type; recognized principal forms are inferred, other fully qualified forms require this flag"},
		},
		Examples: []string{
			"rancher revoke u-abc123 --project p-x --cluster c-x --apply --yes",
			"rancher revoke jane --role cluster-owner --cluster c-x --apply --yes",
			"rancher revoke okta_group://gid --role admin --global --apply --yes",
		},
		Destructive: "true",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c revokeCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <subject> is required").
			WithHint("provide a user/group name, id, or fully qualified principal")
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
		// No aliasPrincipals: the spec's revoke contract is narrower —
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
		e := clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("no %s for subject %q at this scope", bindKind.GVR.Resource, args[0]))
		if len(heldRoles) > 0 {
			held := make([]string, 0, len(heldRoles))
			for r := range heldRoles {
				held = append(held, r)
			}
			sort.Strings(held)
			e = e.WithHint("subject holds: " + strings.Join(held, ", "))
		} else {
			// bindKind.Name, NOT bindKind.GVR.Resource: kinds.Lookup
			// matches Name/Key/Aliases only, and the plural GVR
			// resource string is not among them for prtb/crtb — a
			// GVR-based hint would name a command that doesn't exist.
			// Scope the hint so it returns the same binding set revoke
			// was searching; without a scope the get falls back to an
			// all-namespaces/all-clusters list.
			switch scope.Kind {
			case ScopeProject:
				e = e.WithHint("run: rancher get " + bindKind.Name + " --namespace " + scope.NS + " -o json")
			case ScopeCluster:
				e = e.WithHint("run: rancher get " + bindKind.Name + " --cluster " + scope.Cluster + " -o json")
			default: // ScopeGlobal — no namespace qualifier needed
				e = e.WithHint("run: rancher get " + bindKind.Name + " -o json")
			}
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
	var rv string
	var warnings []string
	if len(names) > 1 {
		target = fmt.Sprintf("%s (%d) for %s", bindKind.GVR.Resource, len(names), args[0])
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
		Cluster:           effCluster,
		ResourceVersion:   rv,
		ConflictDetection: len(names) == 1,
		Reversible:        false,
		Destructive:       true,
		Warnings:          warnings,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		deleted := make([]string, 0, len(names))
		for i, name := range names {
			if err := client.Delete(ctx, bindKind.GVR, scope.NS, name, rvByName[name], false); err != nil {
				return runtime.Result{}, partialFailure(err, name, deleted, names[i+1:])
			}
			deleted = append(deleted, name)
		}
		return runtime.Result{Kind: bindKind.K8sKind + "Delete", Data: map[string]any{"deleted": deleted}}, nil
	}
	return eff, exec, nil
}

// partialFailure wraps a mid-sequence delete failure with the
// deleted/failed/unattempted split, preserving the underlying failure's
// code and retryability on the top-level error.
func partialFailure(err error, failedName string, deleted, unattempted []string) error {
	code, msg, retryable := clierr.CodeGeneric, err.Error(), false
	var ce *clierr.Error
	if errors.As(err, &ce) {
		code, msg, retryable = ce.Code, ce.Message, ce.Retryable
	}
	if deleted == nil {
		deleted = []string{}
	}
	if unattempted == nil {
		unattempted = []string{}
	}
	return clierr.New(code,
		fmt.Sprintf("revoke partially applied: %d deleted, failed on %q, %d unattempted",
			len(deleted), failedName, len(unattempted))).
		WithRetryable(retryable).
		WithHint("re-run the same revoke command; it re-matches the remaining bindings").
		WithDetails(RevokePartialFailure{
			Deleted:     deleted,
			Failed:      RevokeFailure{Name: failedName, Code: code, Message: msg},
			Unattempted: append([]string{}, unattempted...),
		})
}
