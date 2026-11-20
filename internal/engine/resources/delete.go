package resources

import (
	"context"
	"slices"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type deleteCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
}

// NewDelete builds `delete <kind> <name>` (mutating) from a Kind. Plan fetches
// the object, captures its resourceVersion, dry-run-deletes for an admission
// preview, and returns an Effect plus an executor that deletes with the RV
// precondition and, unless --wait=false, waits until the object is gone.
func NewDelete(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return &deleteCmd{k: k, clients: clients}
}

func (c deleteCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:    "delete " + c.k.Name,
		Aliases: c.k.Aliases,
		// "Delete the <kind>" (a single named resource). "a/an" can't be computed
		// for kind names (ingress -> "an", user -> "a"), and get pluralizes
		// instead because it lists (see plural in get.go); patch matches this.
		Usage:       "Delete the " + c.k.Name,
		Desc:        deleteDesc(c.k),
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{argSpec(c.k, true)},
		Flags:       deleteFlags(c.k),
		Destructive: "true",
		Auth:        true,
		Capability:  c.k.Capability,
		Shape:       runtime.ShapeText,
	}
}

// deleteDesc is the description of the generated delete: how it waits,
// then the row's own text.
func deleteDesc(k kinds.Kind) string {
	desc := "Deletes the " + k.Name + " <" + argName(k) + "> with a resourceVersion precondition from its server-side dry-run preview. " +
		"By default it waits until the object is gone, then returns (--wait=false turns the wait off). The object is gone when the server responds with not found (404), " +
		"a watch reports it deleted, or an object with that name has a different uid from the one the command read before the delete (a new object took the name). " +
		"--timeout (a positive duration, default 5m) limits how long it waits. " +
		"A timeout, an interrupt, or an authentication or certificate failure after the delete request is an error whose details say the deletion " +
		"was requested and name a get command to check it. When the server refuses (403) a read of an object the command read before the delete, " +
		"the command stops waiting for that object; the result says deletion started, with a warning."
	if k.DeleteDesc != "" {
		desc += " " + k.DeleteDesc
	}
	return desc
}

// deleteFlags is namedFlags followed by the delete's wait flags: every
// delete waits until the object is gone unless --wait=false is given, and
// --timeout limits the wait. A delete takes no --for: gone is the only
// condition it waits for.
func deleteFlags(k kinds.Kind) []runtime.ArgSpec {
	return append(namedFlags(k), deleteWaitFlags(k)...)
}

// deleteWaitFlags are --wait and --timeout of a delete of k.
func deleteWaitFlags(k kinds.Kind) []runtime.ArgSpec {
	return []runtime.ArgSpec{
		{Name: flagWait, Type: "bool", Desc: "wait until the " + k.Name + " is gone before returning (default true; --wait=false returns once the server accepts the request)"},
		{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, a positive duration, e.g. 10m (default 5m)"},
	}
}

// namedFlags is get's flag set without get's wait flags (--wait, --for and
// --timeout), with the flags a named op on one object (delete and patch)
// requires marked Required so the pipeline rejects before Plan:
//   - an NSCluster kind's --cluster (it becomes the namespace; an empty value
//     must not fall through to the local cluster);
//   - an NSPlain kind's --namespace (an empty value would 404 opaquely).
//
// NSWorkspace deletes need nothing marked Required - the kind's
// DefaultNamespace satisfies the named-op rule. A row with
// ProjectBackingNamespace does not mark --cluster or --namespace as
// required: Route refuses a delete that has no --namespace and no --project
// with --cluster, and its message names both ways.
// --project describes the named object, never a list.
func namedFlags(k kinds.Kind) []runtime.ArgSpec {
	flags := WithoutWaitFlags(getFlags(k))
	for i := range flags {
		switch {
		case k.Policy == kinds.NSCluster && flags[i].Name == FlagCluster:
			flags[i].Required = true
			flags[i].Desc = "cluster `id|name` of the " + k.Name
		case k.Policy == kinds.NSPlain && flags[i].Name == FlagNamespace && !k.ProjectBackingNamespace:
			flags[i].Required = true
		case flags[i].Name == FlagProject && k.ProjectBackingNamespace:
			flags[i].Desc = "project `id|name` (with --cluster) the " + k.Name + " is in"
		case flags[i].Name == FlagProject && k.Policy == kinds.NSNone:
			flags[i].Desc = "the namespace must be in this project"
		case flags[i].Name == FlagProject:
			flags[i].Desc = "the object's namespace must be in this project"
		}
	}
	return flags
}

func (c deleteCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	w, err := ParseDeleteWait(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return c.planWith(ctx, in, w)
}

// planWith is Plan with the wait already read from the flags.
func (c deleteCmd) planWith(ctx context.Context, in runtime.Input, w DeleteWait) (safety.Effect, runtime.ExecuteFunc, error) {
	client, ns, name, obj, effectCluster, err := c.fetch(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	var checkWarnings []string
	if oc := c.k.ObjectChecks; oc != nil && oc.Delete != nil {
		verdict := oc.Delete(obj)
		if verdict.Refusal != "" {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, verdict.Refusal)
		}
		checkWarnings = verdict.Warnings
	}
	eff, exec, err := PlanFromFetched(ctx, client, c.k, ns, name, obj, effectCluster, w)
	if err != nil {
		return eff, exec, err
	}
	eff.Warnings = append(eff.Warnings, checkWarnings...)
	return eff, exec, nil
}

// RunsObjectChecks marks the command as one that runs the row's ObjectChecks
// (ObjectChecked).
func (deleteCmd) RunsObjectChecks() {}

// fetch routes the command, reads the object to delete and returns it with
// the client that read it, the namespace it lives in and the cluster the
// Effect names.
func (c deleteCmd) fetch(ctx context.Context, in runtime.Input) (client engine.ClusterClient, ns, name string, obj *unstructured.Unstructured, effectCluster string, err error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, "", "", nil, "", clierr.New(clierr.CodeValidation, "argument <"+argName(c.k)+"> is required")
	}
	name = args[0]

	// delete always addresses a resource by name; Route enforces the
	// Management+Namespaced --cluster requirement independently of the flag.
	clientCluster, ns, effectCluster, _, err := Route(ctx, c.clients, c.k, in, OpNamed, in.Args()[0])
	if err != nil {
		return nil, "", "", nil, "", err
	}
	client, err = c.clients.Cluster(ctx, clientCluster)
	if err != nil {
		return nil, "", "", nil, "", err
	}
	obj, err = client.Get(ctx, c.k.GVR, ns, name)
	if err != nil {
		return nil, "", "", nil, "", lookupHandle(ctx, c.clients, client, c.k, ns, name, err)
	}
	return client, ns, name, obj, effectCluster, nil
}

// PlanFromFetched builds a delete Effect and exec closure from an object
// already fetched by the caller. Both the dry-run admission preview and the
// exec closure derive their resourceVersion from obj - no second GET occurs,
// so the guard's safety decision and the deletion's precondition are bound to
// the same read.
//
// obj is the response to a GET of name by this same client, sent before the
// delete: the wait after the delete therefore treats a later 403 on that
// object as access that ended after the request. w is how the executor
// waits after the delete; PlanFromFetched refuses, before any request, a
// DeleteWait that ParseDeleteWait did not make.
func PlanFromFetched(ctx context.Context, client engine.ClusterClient, k kinds.Kind, ns, name string, obj *unstructured.Unstructured, effectCluster string, w DeleteWait) (safety.Effect, runtime.ExecuteFunc, error) {
	if w.mode == waitUnset {
		return safety.Effect{}, nil, clierr.New(clierr.CodeGeneric, "internal error: the delete has no wait setting")
	}
	rv := obj.GetResourceVersion()
	// Admission preview: a server-side dry-run delete surfaces RBAC/webhook
	// denials before the user confirms. A TOCTOU race can also produce a
	// NOT_FOUND here; the dynamic client curates hint-less 404s at the source.
	if err := client.Delete(ctx, k.GVR, ns, name, rv, true); err != nil {
		return safety.Effect{}, nil, err
	}
	eff := safety.Effect{
		PreviewSource:     safety.PreviewServerDryRun,
		Action:            safety.ActionDelete,
		Target:            k.GVR.Resource + "/" + name,
		DisplayKind:       k.Name,
		DisplayName:       name,
		Cluster:           effectCluster,
		ResourceVersion:   rv,
		ConflictDetection: true,
		Reversible:        false,
		Destructive:       true,
	}
	// A row-declared cascade note rides the generic delete so a kind whose
	// server-side removal reaches beyond the named object warns without a
	// bespoke command (guarded overrides append their own text instead).
	if k.DeleteCascadeWarning != "" {
		eff.Warnings = append(eff.Warnings, k.DeleteCascadeWarning)
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		if err := client.Delete(ctx, k.GVR, ns, name, rv, false); err != nil {
			return runtime.Result{}, err
		}
		var end deleteEnd
		switch {
		case w.ownToken:
			// No read with a deleted token can confirm anything.
			end.warnings = []string{ownTokenNote}
		case w.mode == waitOn:
			// The deadline starts once the server accepted the delete.
			var err error
			end, err = waitDeleted(ctx, client, append([]deleteTarget{newDeleteTarget(k, ns, name, obj, effectCluster)}, w.also...), time.Now().Add(w.timeout), w.timeoutText)
			if err != nil {
				return runtime.Result{}, err
			}
		}
		if len(w.notWaited) > 0 {
			end.waited = false
			end.warnings = append(slices.Clip(w.notWaited), end.warnings...)
		}
		return deleteResult(k, name, end), nil
	}
	return eff, exec, nil
}
