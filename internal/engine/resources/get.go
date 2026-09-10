// Package resources holds generic verbs (get, delete) driven by the kind
// table: one command implementation per verb, parameterized by a kinds.Kind.
package resources

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// Flag names shared across the generic verbs. Flag generation (getFlags),
// routing (Route), and the manifest namespace reconciliation (reconcile) must
// agree on these exact strings, so they are named once here.
const (
	FlagCluster   = engine.FlagCluster
	FlagNamespace = "namespace"
	FlagProject   = "project"
	flagFilename  = "filename"
	flagPatch     = "patch"
	flagWait      = "wait"
	flagFor       = "for"
	flagTimeout   = "timeout"
	// flagIncludeSystem is generated only on kinds with a SystemFilter (get
	// path only; delete/patch reuse getFlags and must never see it).
	flagIncludeSystem = "include-system"
	// FlagSelector filters a list by a Kubernetes label selector, sent to the
	// server as the list's labelSelector. Get path only, for the same reason
	// as flagIncludeSystem: a delete or patch addresses one named object.
	// Exported for the bespoke get overrides that mirror this descriptor.
	FlagSelector = "selector"
)

const defaultWaitTimeout = 120 * time.Second

type getCmd struct {
	k       kinds.Kind
	clients engine.ClusterClientFactory
	enrich  []GetEnricher
}

// GetEnricher inspects one rendered object and optionally returns a warning
// to attach to Result.Warnings - the read-path counterpart to NewPatch's
// EffectEnricher. It runs once per rendered object: once for a named get,
// once per item for a list.
type GetEnricher func(ctx context.Context, obj *unstructured.Unstructured) (string, bool)

// NewGet builds `get <kind> [name]` (readOnly) from a Kind: the Descriptor,
// scope-based flags, and cluster/namespace routing all derive from the table
// row. enrich runs against every rendered object, named or listed.
func NewGet(k kinds.Kind, clients engine.ClusterClientFactory, enrich ...GetEnricher) runtime.Command {
	return &getCmd{k: k, clients: clients, enrich: enrich}
}

// enrichWarnings runs every enricher against obj, collecting every warning
// triggered - never just the first.
func (c getCmd) enrichWarnings(ctx context.Context, obj *unstructured.Unstructured) []string {
	var out []string
	for _, e := range c.enrich {
		if w, ok := e(ctx, obj); ok {
			out = append(out, w)
		}
	}
	return out
}

func (c getCmd) Describe() runtime.Descriptor {
	// --selector sits with the routing flags, ahead of the named-only wait
	// trio getFlags appends last, so help reads list flags then wait flags.
	flags := getFlags(c.k)
	flags = slices.Insert(flags, len(flags)-3, runtime.ArgSpec{Name: FlagSelector, Type: "string", Aliases: []string{"l"},
		Desc: "filter a list by Kubernetes label `selector`, e.g. app=web,tier!=cache (kubectl's -l)"})
	var desc string
	if c.k.SystemFilter != nil {
		flags = append(flags, runtime.ArgSpec{Name: flagIncludeSystem, Type: "bool",
			Desc: "include " + kinds.Plural(c.k.SystemFilter.Label) + " (hidden by default)"})
		desc = kinds.Plural(c.k.Name) + " hide " + kinds.Plural(c.k.SystemFilter.Label) +
			" by default; pass --include-system to show them. Naming a " + c.k.Name +
			" always returns it, hidden or not."
	}
	return runtime.Descriptor{
		Name:         "get " + c.k.Name,
		Aliases:      c.k.Aliases,
		Usage:        "List " + kinds.Plural(c.k.Name) + " or show one",
		Desc:         desc,
		CommandType:  runtime.ReadOnly,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:        flags,
		Network:      true,
		Auth:         true,
		ProjectScope: kinds.ProjectScopable(c.k),
		Capability:   c.k.Capability,
		Shape:        runtime.ShapeTabular,
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
// schema/TTY description lives in one place. required is true for kinds that
// always route to a downstream cluster - the schema must never advertise an
// invocation Route would reject. NSCluster kinds and create resource pass
// false and override Desc: the former lists across clusters when the flag is
// omitted, the latter can take the cluster from the manifest.
func ClusterFlag(required bool) runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagCluster, Type: "string", Required: required, Desc: "target cluster `id|name`",
		MissingMessage: engine.ClusterRequiredMessage, MissingHint: engine.ClusterRequiredHint}
}

func namespaceFlag() runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagNamespace, Type: "string", Desc: "`namespace` to operate in"}
}

func getFlags(k kinds.Kind) []runtime.ArgSpec {
	var flags []runtime.ArgSpec
	switch k.Policy {
	case kinds.NSCluster:
		f := ClusterFlag(false)
		f.Desc = "cluster `id|name`; required to address a " + k.Name + " by name, omit to list across clusters"
		flags = []runtime.ArgSpec{f}
	case kinds.NSWorkspace:
		flags = []runtime.ArgSpec{namespaceFlag()}
	case kinds.NSPlain:
		if k.Scope == kinds.Downstream {
			flags = []runtime.ArgSpec{ClusterFlag(true), namespaceFlag()}
		} else {
			flags = []runtime.ArgSpec{namespaceFlag()}
		}
	default: // NSNone
		if k.Scope == kinds.Downstream {
			flags = []runtime.ArgSpec{ClusterFlag(true)}
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

	if named && c.k.SystemFilter != nil && in.IsSet(flagIncludeSystem) {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--include-system does not apply to a named "+c.k.Name+"; a named get always returns the object")
	}
	labelSelector := in.String(FlagSelector)
	if _, err := ParseListSelector(c.k.Name, named, labelSelector); err != nil {
		return runtime.Result{}, err
	}
	if !wait && (forExpr != "" || timeoutStr != "") {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--for/--timeout require --wait")
	}
	if wait && !named {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--wait requires a resource name")
	}
	if wait && c.k.WaitDefault == "" && forExpr == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s has no default wait condition; pass --for state=<value> or --for condition=<Type>[=<Status>]", c.k.Name))
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
			timeout, terr := parseWaitTimeout(in)
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
		warnings := append(waitWarnings, c.enrichWarnings(ctx, one)...)
		return runtime.Result{Kind: title, Data: newTable(c.k, []unstructured.Unstructured{*one}, true),
			Summary: waitSummary, Warnings: warnings}, nil
	}
	selector := labelSelector
	if scope != nil {
		selector = joinSelectors(scope.Selector, labelSelector)
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
	var warnings []string
	if c.k.SystemFilter != nil && !in.Bool(flagIncludeSystem) {
		var hidden int
		items, hidden = hideSystemManaged(items, c.k.SystemFilter)
		if hidden > 0 {
			warnings = append(warnings, systemHiddenWarning(hidden, c.k.SystemFilter.Label))
		}
	}
	for i := range items {
		warnings = append(warnings, c.enrichWarnings(ctx, &items[i])...)
	}
	return runtime.Result{Kind: title + "List", Data: newTable(c.k, items, false), Warnings: warnings}, nil
}

// hideSystemManaged drops objects matching f, returning the kept objects and
// the count hidden. Matching is per element (isSystemManaged): a non-string
// element is skipped, never fatal, matching Rancher's own transformer.
func hideSystemManaged(items []unstructured.Unstructured, f *kinds.PrefixFilter) ([]unstructured.Unstructured, int) {
	kept := make([]unstructured.Unstructured, 0, len(items))
	hidden := 0
	for _, item := range items {
		if isSystemManaged(item, f) {
			hidden++
			continue
		}
		kept = append(kept, item)
	}
	return kept, hidden
}

// isSystemManaged reports whether any element of the string array at f.Path
// has f.Prefix. A non-array or absent field is never hidden; each element is
// string-asserted independently, and a non-string element is skipped rather
// than disqualifying the whole array - Rancher's user-store transformer casts
// each principalIds element the same way and continues past failed casts.
func isSystemManaged(obj unstructured.Unstructured, f *kinds.PrefixFilter) bool {
	val, found, err := unstructured.NestedFieldNoCopy(obj.Object, f.Path...)
	if err != nil || !found {
		return false
	}
	arr, ok := val.([]any)
	if !ok {
		return false
	}
	for _, elem := range arr {
		s, ok := elem.(string)
		if ok && strings.HasPrefix(s, f.Prefix) {
			return true
		}
	}
	return false
}

// systemHiddenWarning renders the announced-filtering warning, pluralized
// from Label with no literal "(s)": "1 system account hidden - pass
// --include-system to show it" / "4 system accounts hidden - pass
// --include-system to show them".
func systemHiddenWarning(count int, label string) string {
	noun, pronoun := label, "it"
	if count != 1 {
		noun, pronoun = kinds.Plural(label), "them"
	}
	return fmt.Sprintf("%d %s hidden - pass --include-system to show %s", count, noun, pronoun)
}

// waitElapsed renders the wait summary's elapsed display: Go's
// second-rounded duration format at or above one second (1m35s, 2h15m30s -
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

// parseWaitTimeout reads --timeout, defaulting to 120s; a value that is
// not a positive duration is a VALIDATION error rather than a fallback to
// the default or an already-expired wait.
func parseWaitTimeout(in runtime.Input) (time.Duration, error) {
	return runtime.DurationFlag(in, flagTimeout, defaultWaitTimeout, "5m")
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
// nil there - membership is enforced by the server, not the caller).
type ProjectScope struct {
	Members  map[string]bool
	Selector string
}

// TypedProject returns the --project value the user typed - THE project
// accessor. Project scope is a filter, and no default may filter: only an
// explicit per-invocation flag ever scopes anything. Exported so every
// project-scope consumer - Route, usage, and the composites - shares this
// one seam.
func TypedProject(in runtime.Input) string { return in.String(FlagProject) }

// rejectProjectScope enforces PC-3's "never a silent no-op" rule for kind
// families that do not support project scoping: an explicitly TYPED
// --project is a VALIDATION error, never an ignored value.
func rejectProjectScope(in runtime.Input, k kinds.Kind) error {
	if TypedProject(in) == "" {
		return nil
	}
	return clierr.New(clierr.CodeValidation, "--project does not apply to "+k.Name)
}

// resolveProjectScope resolves the typed --project for a Downstream
// namespaced kind, returning ("", nil, nil) when no project is in
// play. clusterID is the already-resolved downstream cluster the member
// namespace list is fetched from.
func resolveProjectScope(ctx context.Context, clients engine.ClusterClientFactory, k kinds.Kind, in runtime.Input, clusterID string) (projectID string, members map[string]bool, err error) {
	raw := TypedProject(in)
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
// ResolveCluster - the flag is required, a VALIDATION error when absent.
//
// SAFETY (review F3) - for NSCluster kinds an empty --cluster must NEVER
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
			if err := rejectProjectScope(in, k); err != nil {
				return "", "", "", nil, err
			}
		}
		clusterFlag := in.String(FlagCluster)
		if clusterFlag == "" {
			if named {
				return "", "", "", nil, clierr.New(clierr.CodeValidation,
					"--cluster is required to address a "+k.Name+" by name")
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
			if err := rejectProjectScope(in, k); err != nil {
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
		// Namespace is a pure input requirement - check it before any network
		// resolution so a missing --namespace fails fast. A named op with an
		// empty --namespace would pass ns="" to the API and 404 opaquely; a
		// nameless list keeps ns="" (all-namespaces), which is valid and useful.
		nspace := in.String(FlagNamespace)
		if named && nspace == "" {
			return "", "", "", nil, clierr.New(clierr.CodeValidation,
				"--namespace is required to address a "+k.Name+" by name")
		}
		if !scopable {
			if err := rejectProjectScope(in, k); err != nil {
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
					"pass a project namespace, or omit --namespace to scope the whole project")
			}
		}
		return clientCluster, ns, effectCluster, &ProjectScope{Members: members}, nil

	default: // NSNone - cluster-scoped, no namespace
		if scopable {
			return routeNamespaceKind(ctx, clients, k, in)
		}
		if err := rejectProjectScope(in, k); err != nil {
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
	raw := TypedProject(in)
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
			return "", "", "", nil, projects.NotMemberError(name, projectID, "pass a project namespace")
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
// requires an explicit --cluster (VALIDATION when absent).
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

// ParseListSelector validates a --selector value for a get: it does not
// apply to a named get (a named get returns the object, a selector filters
// a list), and a malformed value is a VALIDATION error with the parser's
// reason before any request, not a generic 400 from the server. An empty
// value returns a nil selector. Shared by the generic get and the bespoke
// get overrides that inherit its descriptor.
func ParseListSelector(kindName string, named bool, raw string) (labels.Selector, error) {
	if raw == "" {
		return nil, nil
	}
	if named {
		return nil, clierr.New(clierr.CodeValidation,
			"--selector does not apply to a named "+kindName+"; a named get returns the object, a selector filters a list")
	}
	sel, err := labels.Parse(raw)
	if err != nil {
		return nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--selector %q is not a valid label selector: %v", raw, err))
	}
	return sel, nil
}

// joinSelectors combines the project scope's selector with the caller's
// --selector as one conjunction: a Kubernetes label selector is a
// comma-separated list of requirements that must all hold.
func joinSelectors(scope, flag string) string {
	switch {
	case scope == "":
		return flag
	case flag == "":
		return scope
	}
	return scope + "," + flag
}
