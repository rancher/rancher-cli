package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type deleteCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
}

// NewDelete builds `delete <kind> <name>` (mutating) from a Kind. Plan fetches
// the object, captures its resourceVersion, dry-run-deletes for an admission
// preview, and returns an Effect plus an executor that deletes with the RV
// precondition — the end-to-end proof of Approach B.
func NewDelete(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return &deleteCmd{k: k, clients: clients}
}

func (c deleteCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "delete " + c.k.Name,
		Aliases:     c.k.Aliases,
		Usage:       "Delete a " + c.k.Name,
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags:       deleteFlags(c.k),
		Destructive: "true",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// deleteFlags is get's flag set with the flags a delete (always a named op)
// requires marked Required so the pipeline rejects before Plan:
//   - an NSCluster kind's --cluster (it becomes the namespace; an empty value
//     must not fall through to the local cluster — F3);
//   - an NSPlain kind's --namespace (an empty value would 404 opaquely).
//
// NSWorkspace deletes need nothing marked Required — the kind's
// DefaultNamespace satisfies the named-op rule.
func deleteFlags(k kinds.Kind) []runtime.ArgSpec {
	flags := getFlags(k)
	for i := range flags {
		switch {
		case k.Policy == kinds.NSCluster && flags[i].Name == "cluster":
			flags[i].Required = true
		case k.Policy == kinds.NSPlain && flags[i].Name == "namespace":
			flags[i].Required = true
		}
	}
	return flags
}

func (c deleteCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	name := args[0]

	// delete always addresses a resource by name; route enforces the
	// Management+Namespaced --cluster requirement independently of the flag.
	clientCluster, ns, effectCluster, err := route(ctx, c.clients, c.k, in, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, clientCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	obj, err := client.Get(ctx, c.k.GVR, ns, name)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	rv := obj.GetResourceVersion()
	// Admission preview: a server-side dry-run delete surfaces RBAC/webhook
	// denials before the user confirms.
	if err := client.Delete(ctx, c.k.GVR, ns, name, rv, true); err != nil {
		return safety.Effect{}, nil, err
	}
	title := kindTitle(c.k.Name)
	eff := safety.Effect{
		Action:            safety.ActionDelete,
		Target:            c.k.GVR.Resource + "/" + name,
		Cluster:           effectCluster,
		ResourceVersion:   rv,
		ConflictDetection: true,
		Reversible:        false,
		Destructive:       true,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		if err := client.Delete(ctx, c.k.GVR, ns, name, rv, false); err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: title + "Delete", Data: map[string]any{"deleted": name}}, nil
	}
	return eff, exec, nil
}
