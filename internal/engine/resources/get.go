// Package resources holds generic verbs (get, delete) driven by the kind
// table: one command implementation per verb, parameterized by a kinds.Kind.
package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Flag names shared across the generic verbs. Flag generation (getFlags),
// routing (Route), and the manifest namespace reconciliation (reconcile) must
// agree on these exact strings, so they are named once here.
const (
	FlagCluster   = engine.FlagCluster
	FlagNamespace = "namespace"
	flagProject   = "project"
	flagFilename  = "filename"
	flagPatch     = "patch"
	flagWait      = "wait"
	flagFor       = "for"
	flagTimeout   = "timeout"
)

const defaultWaitTimeout = 120 * time.Second

type getCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
}

// NewGet builds `get <kind> [name]` (readOnly) from a Kind: the Descriptor,
// scope-based flags, and cluster/namespace routing all derive from the table
// row.
func NewGet(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return &getCmd{k: k, clients: clients}
}

func (c getCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:         "get " + c.k.Name,
		Aliases:      c.k.Aliases,
		Usage:        "List " + kinds.Plural(c.k.Name) + " or show one",
		CommandType:  runtime.ReadOnly,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:        getFlags(c.k),
		Network:      true,
		Auth:         true,
		ProjectScope: kinds.ProjectScopable(c.k),
	}
}

// getFlags declares the routing flags a kind's namespace policy needs:
//   - NSCluster: --cluster (it becomes the namespace).
//   - NSWorkspace: --namespace only (client is always management).
//   - NSPlain: --namespace, plus --cluster when the kind is Downstream.
//   - NSNone: --cluster only when the kind is Downstream; nothing otherwise.
//
// Every kind additionally gains --wait/--for/--timeout (named-get only; see
// Execute).
// clusterFlag and namespaceFlag are the single source for the two routing
// flags shared across get/delete/patch (via getFlags) and create, so their
// schema/TTY description lives in one place.
func clusterFlag() runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagCluster, Type: "string", Desc: "target cluster `id|name` (defaults to the current context)"}
}

func namespaceFlag() runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagNamespace, Type: "string", Desc: "`namespace` to operate in"}
}

func getFlags(k kinds.Kind) []runtime.ArgSpec {
	var flags []runtime.ArgSpec
	switch k.Policy {
	case kinds.NSCluster:
		flags = []runtime.ArgSpec{clusterFlag()}
	case kinds.NSWorkspace:
		flags = []runtime.ArgSpec{namespaceFlag()}
	case kinds.NSPlain:
		if k.Scope == kinds.Downstream {
			flags = []runtime.ArgSpec{clusterFlag(), namespaceFlag()}
		} else {
			flags = []runtime.ArgSpec{namespaceFlag()}
		}
	default: // NSNone
		if k.Scope == kinds.Downstream {
			flags = []runtime.ArgSpec{clusterFlag()}
		}
	}
	return append(flags,
		runtime.ArgSpec{Name: flagWait, Type: "bool", Desc: "wait for the resource to reach --for before returning"},
		runtime.ArgSpec{Name: flagFor, Type: "string", Desc: "wait `condition`, e.g. state=active or condition=Ready"},
		runtime.ArgSpec{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, e.g. 5m (default 120s)"},
	)
}

func (c getCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	wait := in.Bool(flagWait)
	forExpr := in.String(flagFor)
	timeoutStr := in.String(flagTimeout)
	named := hasName(in)

	if !wait && (forExpr != "" || timeoutStr != "") {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--for/--timeout require --wait").
			WithHint("pass --wait")
	}
	if wait && !named {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--wait requires a resource name").
			WithHint("pass a name argument")
	}
	if wait && c.k.WaitDefault == "" && forExpr == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s has no default wait condition", c.k.Name)).
			WithHint("pass --for state=<value> or --for condition=<Type>[=<Status>] to specify a custom signal")
	}

	clusterID, ns, _, scope, err := Route(ctx, c.clients, c.k, in, named)
	if err != nil {
		return runtime.Result{}, err
	}
	client, err := c.clients.Cluster(ctx, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	title := resultKind(c.k)
	if named {
		name := in.Args()[0]
		if c.k.Key == "managementcluster" {
			resolved, rerr := c.clients.ResolveCluster(ctx, name)
			if rerr != nil {
				return runtime.Result{}, rerr
			}
			name = resolved
		}
		var one *unstructured.Unstructured
		var waitSummary string
		var waitWarnings []string
		if wait {
			expr := forExpr
			if expr == "" {
				expr = c.k.WaitDefault
			}
			cond, cerr := parseWaitCondition(expr)
			if cerr != nil {
				return runtime.Result{}, cerr
			}
			timeout, terr := parseWaitTimeout(timeoutStr)
			if terr != nil {
				return runtime.Result{}, terr
			}
			start := time.Now()
			one, err = waitFor(ctx, client, c.k.GVR, ns, name, cond, timeout, c.k.WaitFailsOnStalled)
			if err == nil {
				waitSummary = fmt.Sprintf("wait: %s satisfied after %s", expr, waitElapsed(time.Since(start)))
				if c.k.WaitFailsOnStalled && !cond.isStalledTarget() {
					if w := freshSuccessWarning(one, c.k.Name, name, ns); w != "" {
						waitWarnings = append(waitWarnings, w)
					}
				}
			}
		} else {
			one, err = client.Get(ctx, c.k.GVR, ns, name)
		}
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: title, Data: newTable(c.k, []unstructured.Unstructured{*one}, true),
			Summary: waitSummary, Warnings: waitWarnings}, nil
	}
	selector := ""
	if scope != nil {
		selector = scope.Selector
	}
	list, lerr := client.List(ctx, c.k.GVR, ns, selector)
	if lerr != nil {
		return runtime.Result{}, lerr
	}
	items := list.Items
	// caller-side list filtering (PC-3): a Downstream NSPlain kind under a
	// project without --namespace carries the member set here; the
	// namespace-kind special case instead carries a server-side Selector
	// above and leaves Members nil.
	if scope != nil && scope.Members != nil {
		filtered := make([]unstructured.Unstructured, 0, len(items))
		for _, item := range items {
			if scope.Members[item.GetNamespace()] {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}
	return runtime.Result{Kind: title + "List", Data: newTable(c.k, items, false)}, nil
}

// waitElapsed renders the wait summary's elapsed display: Go's
// second-rounded duration format at or above one second (1m35s, 2h15m30s —
// full seconds precision, never truncated to a coarser unit). This
// deliberately diverges from the AGE-column short form (hours-only for long
// durations) which is the k8s table idiom; wait summaries warrant the full
// breakdown. Below one second: milliseconds, floored at 1ms so an instant
// wait never prints "0s".
func waitElapsed(elapsed time.Duration) string {
	if elapsed < time.Second {
		ms := elapsed.Round(time.Millisecond)
		if ms == 0 {
			ms = time.Millisecond
		}
		return ms.String()
	}
	return elapsed.Round(time.Second).String()
}

// parseWaitTimeout parses --timeout, defaulting to 120s; an unparseable
// duration is a VALIDATION error rather than falling back to the default.
func parseWaitTimeout(s string) (time.Duration, error) {
	if s == "" {
		return defaultWaitTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, clierr.New(clierr.CodeValidation, fmt.Sprintf("--timeout %q is not a valid duration", s)).
			WithHint("pass e.g. --timeout 5m")
	}
	return d, nil
}

func hasName(in runtime.Input) bool {
	args := in.Args()
	return len(args) > 0 && args[0] != ""
}

// ProjectScope carries the effective --project scope a routing decision
// resolved (PC-3): nil means unscoped. Members is the project's member
// namespace set, used for caller-side membership validation and list
// filtering (the generic Downstream NSPlain path). Selector is a server-side
// label selector, used only by the namespace-kind special case (Members is
// nil there — membership is enforced by the server, not the caller).
type ProjectScope struct {
	Members  map[string]bool
	Selector string
}

// EffectiveProject resolves --project (flag > context, PC-2's precedence).
// Exported so every project-scope consumer — Route, usage, and the
// composites that must actively reject a project — shares this one seam
// instead of some reading the flag directly and silently skipping the
// context fallback.
func EffectiveProject(clients engine.ClusterClientFactory, in runtime.Input) (string, error) {
	if p := in.String(flagProject); p != "" {
		return p, nil
	}
	return clients.ContextProject()
}

// rejectProjectScope enforces PC-3's "never a silent no-op" rule for kind
// families that do not support project scoping: any effective project (flag
// or context) is a VALIDATION error, never an ignored value.
func rejectProjectScope(clients engine.ClusterClientFactory, in runtime.Input, k kinds.Kind) error {
	project, err := EffectiveProject(clients, in)
	if err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	return clierr.New(clierr.CodeValidation, "--project does not apply to "+k.Name).
		WithHint("drop --project for this kind")
}

// resolveProjectScope resolves the effective project (flag > context) for a
// Downstream namespaced kind, returning ("", nil, nil) when no project is in
// play. clusterID is the already-resolved downstream cluster the member
// namespace list is fetched from.
func resolveProjectScope(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, clusterID string) (projectID string, members map[string]bool, err error) {
	raw, err := EffectiveProject(clients, in)
	if err != nil {
		return "", nil, err
	}
	if raw == "" {
		return "", nil, nil
	}
	id, err := projects.ResolveProject(ctx, clients, clusterID, raw)
	if err != nil {
		return "", nil, err
	}
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return "", nil, err
	}
	members, err = projects.ProjectNamespaces(ctx, client, id)
	if err != nil {
		return "", nil, err
	}
	return id, members, nil
}

// Route maps a kind's NamespacePolicy + input to the client cluster id, the
// namespace, the Effect's target cluster (the identity a mutation lands on,
// which differs from the client cluster for management namespaced kinds), and
// the effective project scope (PC-3; nil when unscoped):
//
//	NSNone:      client per scope,     ns "",                 effect = client
//	NSCluster:   client "local",       ns = resolved --cluster, effect that cluster
//	NSWorkspace: client "local",       ns = --namespace|Default, effect "local"
//	NSPlain:     client per scope,     ns = --namespace,      effect = client
//
// "per scope" (NSNone, NSPlain): a Management kind is served by the local proxy
// ("local"); a Downstream kind resolves --cluster (name-or-id) via
// ResolveCluster, falling back to the context cluster, else a VALIDATION error.
//
// SAFETY (review F3) — for NSCluster kinds an empty --cluster must NEVER
// silently become "local" (ResolveCluster("") returns LocalCluster, which as a
// namespace targets the local cluster's own resources): an empty --cluster is
// allowed only for a nameless list (namespace "" = across all clusters);
// addressing a resource by name requires --cluster. NSPlain applies the same
// fail-fast rule to --namespace before any network resolution.
//
// PC-3 project scope: a Downstream NSPlain kind validates/filters by project
// membership inside Route (named ops and mutations can never skip it); every
// other kind family rejects a non-empty project outright, EXCEPT the
// namespace kind itself (NSNone/Downstream), which converts an unnamed
// --project into a server-side label-selector list rather than a rejection.
//
// Route is shared by get and delete: delete always passes named=true and uses
// the effect cluster to stamp Effect.Cluster.
func Route(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, named bool) (clientCluster, ns, effectCluster string, scope *ProjectScope, err error) {
	scopable := kinds.ProjectScopable(k)
	switch k.Policy {
	case kinds.NSCluster:
		if !scopable {
			if err := rejectProjectScope(clients, in, k); err != nil {
				return "", "", "", nil, err
			}
		}
		clusterFlag := in.String(FlagCluster)
		if clusterFlag == "" {
			if named {
				return "", "", "", nil, clierr.New(clierr.CodeValidation,
					"--cluster is required to address a "+k.Name+" by name").
					WithHint("pass --cluster <id|name>")
			}
			return engine.LocalCluster, "", "", nil, nil // list across all clusters
		}
		id, rerr := clients.ResolveCluster(ctx, clusterFlag)
		if rerr != nil {
			return "", "", "", nil, rerr
		}
		// The namespace IS the cluster id; the Effect targets that cluster.
		return engine.LocalCluster, id, id, nil, nil

	case kinds.NSWorkspace:
		if !scopable {
			if err := rejectProjectScope(clients, in, k); err != nil {
				return "", "", "", nil, err
			}
		}
		// A management-plane namespace with a default; --cluster is never
		// consulted and a missing --namespace falls back to the default, so a
		// named op never fails validation here.
		nspace := in.String(FlagNamespace)
		if nspace == "" {
			nspace = k.DefaultNamespace
		}
		return engine.LocalCluster, nspace, engine.LocalCluster, nil, nil

	case kinds.NSPlain:
		// Namespace is a pure input requirement — check it before any network
		// resolution so a missing --namespace fails fast. A named op with an
		// empty --namespace would pass ns="" to the API and 404 opaquely; a
		// nameless list keeps ns="" (all-namespaces), which is valid and useful.
		nspace := in.String(FlagNamespace)
		if named && nspace == "" {
			return "", "", "", nil, clierr.New(clierr.CodeValidation,
				"--namespace is required to address a "+k.Name+" by name").
				WithHint("pass --namespace <namespace>")
		}
		if !scopable {
			if err := rejectProjectScope(clients, in, k); err != nil {
				return "", "", "", nil, err
			}
			clientCluster, ns, effectCluster, err = routeCluster(ctx, clients, k, in, nspace)
			return clientCluster, ns, effectCluster, nil, err
		}
		clientCluster, ns, effectCluster, err = routeCluster(ctx, clients, k, in, nspace)
		if err != nil {
			return "", "", "", nil, err
		}
		projectID, members, perr := resolveProjectScope(ctx, clients, k, in, clientCluster)
		if perr != nil {
			return "", "", "", nil, perr
		}
		if projectID == "" {
			return clientCluster, ns, effectCluster, nil, nil
		}
		if nspace != "" {
			if !members[nspace] {
				return "", "", "", nil, projects.NotMemberError(nspace, projectID,
					"pass a member namespace, or omit --namespace to scope the whole project")
			}
		}
		return clientCluster, ns, effectCluster, &ProjectScope{Members: members}, nil

	default: // NSNone — cluster-scoped, no namespace
		if scopable {
			return routeNamespaceKind(ctx, clients, k, in)
		}
		if err := rejectProjectScope(clients, in, k); err != nil {
			return "", "", "", nil, err
		}
		clientCluster, ns, effectCluster, err = routeCluster(ctx, clients, k, in, "")
		return clientCluster, ns, effectCluster, nil, err
	}
}

// routeNamespaceKind is PC-3's special case: the namespace kind's own
// membership IS the project label, so --project converts a list into a
// server-side label-selector query rather than a rejection or a caller-side
// membership filter. A named op still validates membership like any other
// Downstream NSPlain kind, since a label selector cannot address one object
// by name.
func routeNamespaceKind(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input) (clientCluster, ns, effectCluster string, scope *ProjectScope, err error) {
	clientCluster, ns, effectCluster, err = routeCluster(ctx, clients, k, in, "")
	if err != nil {
		return "", "", "", nil, err
	}
	raw, err := EffectiveProject(clients, in)
	if err != nil {
		return "", "", "", nil, err
	}
	if raw == "" {
		return clientCluster, ns, effectCluster, nil, nil
	}
	projectID, err := projects.ResolveProject(ctx, clients, clientCluster, raw)
	if err != nil {
		return "", "", "", nil, err
	}
	if hasName(in) {
		client, cerr := clients.Cluster(ctx, clientCluster)
		if cerr != nil {
			return "", "", "", nil, cerr
		}
		members, merr := projects.ProjectNamespaces(ctx, client, projectID)
		if merr != nil {
			return "", "", "", nil, merr
		}
		name := in.Args()[0]
		if !members[name] {
			return "", "", "", nil, projects.NotMemberError(name, projectID, "pass a member namespace")
		}
		return clientCluster, ns, effectCluster, nil, nil
	}
	selector, err := projects.ProjectSelector(projectID)
	if err != nil {
		return "", "", "", nil, err
	}
	return clientCluster, ns, effectCluster, &ProjectScope{Selector: selector}, nil
}

// routeCluster picks the serving cluster for the scope-driven policies (NSNone,
// NSPlain): a Management kind is served by the local proxy; a Downstream kind
// resolves --cluster (falling back to the context cluster, else VALIDATION).
// ns passes through unchanged and the Effect targets the serving cluster.
func routeCluster(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, ns string) (clientCluster, nsOut, effectCluster string, err error) {
	if k.Scope == kinds.Management {
		return engine.LocalCluster, ns, engine.LocalCluster, nil
	}
	id, err := engine.ResolveDownstreamCluster(ctx, clients, in)
	if err != nil {
		return "", "", "", err
	}
	return id, ns, id, nil
}

// resultKind is the Result envelope's Kind for a kind table row: the
// manifest's proper Kubernetes casing (k.K8sKind, e.g. "RoleTemplate").
// Non-empty K8sKind is a table invariant enforced by kinds validation. This
// keeps get/patch/delete's envelope kind consistent with create's, which
// already echoes k.K8sKind.
func resultKind(k kinds.Kind) string {
	return k.K8sKind
}
