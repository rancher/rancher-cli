// Package kinds is the single source of kind knowledge for generic verbs:
// canonical MCP-vocabulary keys (conformance-tested against rancher-ai-mcp's
// converter table), CLI-facing names/aliases, GVRs, scope, deletability,
// contract/stability bindings for GENERATED ops, and table projections.
package kinds

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

type Scope string

const (
	Management Scope = "management" // served by the management cluster ("local")
	Downstream Scope = "downstream" // served by a --cluster target
)

type Column struct {
	Header string
	Path   []string // unstructured.NestedString path; empty = metadata.name
	// AltPaths are fallback NestedString paths tried in order when Path
	// (and earlier alternates) yield no non-empty string - the first
	// non-empty value renders. Lets one column project a field that lives
	// under different keys per object (crtb/prtb MEMBER: userName |
	// groupPrincipalName | userPrincipalName).
	AltPaths [][]string
	// Bool marks the leaf boolean-typed: an absent field renders "false"
	// instead of blank, because an omitempty bool (e.g. NodeDriverSpec's
	// builtin) drops its false value from the wire entirely. Unmarked
	// (string-typed) columns keep rendering absent fields blank.
	Bool bool
	// Default, when non-empty, renders when Path (and AltPaths) yield
	// nothing - for fields whose ABSENCE means a non-blank value, e.g.
	// User.enabled is an omitempty *bool whose nil means true. Mutually
	// exclusive with Bool (both are absent-fallbacks; Bool is the
	// false-when-absent special case).
	Default string
	// Join marks Path as naming a string-ARRAY field: every element
	// renders comma-joined in server order (e.g. User.principalIds).
	// Deliberately not "first element": Rancher's server code treats
	// principal order as meaningless - every consumer scans the whole
	// slice - so projecting one index would assert an ordering contract
	// the server never made. Mutually exclusive with Bool and Condition;
	// AltPaths are not consulted.
	Join bool
	// Condition, when set, derives the cell from the status.conditions
	// entry of this type instead of Path/AltPaths (which are ignored):
	// status True renders the type name ("Ready"), False renders
	// "Not<Type>" ("NotReady"), anything else renders verbatim
	// ("Unknown") - the kubectl node-STATUS idiom. An absent condition -
	// or a degenerate entry with no status - renders blank: nothing is
	// fabricated. Exists because Rancher's Norman-computed status.state
	// never appears on raw CRD reads.
	//
	// Condition types are CamelCase by k8s convention (e.g. "Ready",
	// "DiskPressure"); the "Not" prefix rendering depends on this.
	// validateKinds rejects any value whose first rune is not uppercase.
	Condition string
	// Wide marks the column secondary: -o table omits it, -o wide includes
	// it (after every non-Wide column, in declaration order). Orthogonal to
	// every projection mode above - any column, whatever its axis, may
	// carry Wide.
	Wide bool
	// Count names the path to a map or slice field; the cell renders its
	// length: absent → blank, present-but-empty → "0" (a real count, never
	// fabricated). Covers configmap/secret DATA, serviceaccount SECRETS.
	// Mutually exclusive with Path, AltPaths, Bool, Default, Join, and
	// Condition - validateKinds enforces.
	Count []string
}

// NamespacePolicy decides namespace semantics and flag generation for a
// kind. Scope decides WHICH cluster serves the request; Policy decides how
// the namespace is chosen. The two are orthogonal.
type NamespacePolicy string

const (
	// NSNone: cluster-scoped resource - no namespace flag.
	NSNone NamespacePolicy = "none"
	// NSCluster: the namespace IS a cluster id (project,
	// clusterroletemplatebinding) - --cluster flag, resolved via
	// ResolveCluster, required for named ops; client is management.
	NSCluster NamespacePolicy = "cluster"
	// NSWorkspace: management-plane namespace with a default (fleet,
	// provisioning, CAPI kinds - all default fleet-default) - --namespace
	// flag with DefaultNamespace fallback, NO --cluster; client is management.
	NSWorkspace NamespacePolicy = "workspace"
	// NSPlain: ordinary namespace - --namespace flag, required for named
	// ops; --cluster routes when Scope is Downstream. For the one Management
	// NSPlain kind (projectroletemplatebinding) --namespace is a project id.
	NSPlain NamespacePolicy = "plain"
)

type Kind struct {
	Key              string   // canonical MCP-vocabulary key ("managementcluster")
	Name             string   // CLI-facing singular ("cluster")
	Aliases          []string // plural/short forms
	GVR              schema.GroupVersionResource
	Policy           NamespacePolicy // namespace semantics + flag generation
	DefaultNamespace string          // NSWorkspace fallback ("" elsewhere)
	K8sKind          string          // manifest `kind:` value ("Deployment") - consumed by parity branch 2 reverse lookup
	Scope            Scope
	Deletable        bool // false = no generic delete generated (clusters, nodes)
	// NoDeleteReason states, for a Deletable: false row, why generic delete
	// is absent - surfaced by `rancher schema`. "" renders a plain
	// "not available". Meaningless on Deletable: true rows; leave empty.
	NoDeleteReason string
	Sensitive      bool // patch on this kind is Destructive (parity branch 2)
	// WaitDefault is the --for expression --wait or --timeout alone applies;
	// "" = no default (--wait or --timeout alone rejects) - an explicit --for
	// is honored regardless. Set only for kinds where the expression is
	// grounded in a real, well-known status.state/status.conditions shape;
	// when in doubt, leave empty.
	WaitDefault string
	// WaitFailsOnStalled marks the kind's controller as kstatus-conformant and
	// gates BOTH consequences: the Stalled=True fail-fast (abort the wait even
	// when the awaited condition reads satisfied - Fleet sets Ready=True
	// alongside Stalled=True on a failed GitRepo clone) AND the fresh-success
	// advisory warning (freshSuccessWarning), AND the generation staleness gate
	// (settled). Opt-in per row because not every controller treats Stalled as
	// terminal; set it only where the kind's controller is known to use kstatus
	// abort semantics. Six condition=Ready rows (provisioningcluster,
	// capicluster, capimachine, capimachineset, capimachinedeployment,
	// k3kcluster) are candidate beneficiaries of the fresh-success advisory but
	// stay unset deliberately: their controllers are unvetted for kstatus
	// semantics and no birth-window race has been observed on them. A future
	// kind wanting only one of the gated behaviours would need a new axis.
	WaitFailsOnStalled bool
	// DeleteCascadeWarning, when non-empty, is appended to the generic
	// delete's Effect warnings: a plain statement of what the server removes
	// beyond the named object. Advisory only, never a gate (Destructive
	// already forces --yes). Bespoke delete overrides carry their own text.
	DeleteCascadeWarning string
	// Completable marks this kind as eligible for live name completion.
	// Deny-by-default: a new row gets no completion until this is explicitly set.
	// Secrets and cloud-credentials are never set.
	Completable bool
	Binding     safety.Binding
	Columns     []Column // extra table columns beyond NAME
	// ServerTable marks a kind whose text tables (the default format,
	// table and wide) come from the cluster's API server, as kubectl's do:
	// get asks for the server's Table and shows its columns and cells, so
	// the row carries no Columns, and the server's answer is shown as it
	// is, so the row carries no SystemFilter or RedactPaths either.
	ServerTable bool
	// RedactPaths lists dot-path field paths (each as a []string key sequence)
	// hidden from every serialized representation - list/get JSON/YAML output,
	// the result of a create or patch, and diff From/To entries - because they
	// carry live credentials. The root-level token field on
	// management.cattle.io/v3 Token is the canonical example. Hiding is removal
	// in JSON/YAML output (get and mutation results) and "<redacted>" in diff
	// entries.
	RedactPaths [][]string
	// NoCreateViaManifest, when non-empty, prevents `create resource` from
	// accepting a manifest whose resolved kind matches this row. The value is
	// surfaced as the error hint. Use it for kinds whose creation must go
	// through a higher-level command that handles key generation, lifecycle
	// derivation, or other invariants that a raw manifest write cannot satisfy.
	NoCreateViaManifest string
	// SystemFilter, when set, marks objects matching it as system-managed:
	// LIST hides them by default (announced via a warning and reversible
	// with the generated --include-system flag); a NAMED get always
	// returns the object, and a typed --include-system on a named get is
	// rejected as VALIDATION, never silently ignored.
	// Matching: any element of the string array at Path has Prefix.
	// Independent of the Columns axes: it filters rows, not cells.
	SystemFilter *PrefixFilter
	// Capability is this kind's capability requirement (the zero value means
	// none). Declared once here; get/patch/delete each copy it into their own
	// Descriptor so the manifest, help, and runtime pipeline cannot drift.
	Capability runtime.CapabilityRequirement
	// Handle is set on a kind whose positional argument is the object's id
	// while users know the object by another field (a username, a display
	// name): the generated get, patch and delete name the argument Handle.Arg
	// and, on a miss, look the typed text up in Handle.Lookup. Nil for a kind
	// whose name is what users type.
	Handle *Handle
	// ArgDesc is the schema description of the generated verbs' positional
	// argument. Set on every Handle row and on the management cluster, whose
	// argument takes an id or a display name and has no Handle.
	ArgDesc string
}

// Handle names the positional argument of a kind whose users do not type the
// object's id, and the fields they type instead.
type Handle struct {
	// Arg is the positional argument's name in help and the schema ("id").
	Arg string
	// Lookup lists the fields a user knows the object by. Two contracts
	// hold for every row:
	//   - a candidate is named by the last non-empty field of its object in
	//     Lookup order; the field that reads best as a name is listed last;
	//   - a project row is matched through the project lister on the
	//     project's display name, and its message uses the first label; its
	//     Path serves the guard and help only.
	// Completion also describes each id with every Lookup field, in order
	// (DescriptionPaths): a field added here shows in those descriptions.
	Lookup []HandleField
}

// HandleField is one field of an object that a user may type in place of
// its id.
type HandleField struct {
	Path  []string // field path in the object, e.g. {"spec", "displayName"}
	Label string   // plain name in messages, e.g. "display name"
}

// PrefixFilter marks objects whose string-array field at Path carries an
// element with the given Prefix as system-managed. Matching is PER ELEMENT:
// each element is string-asserted independently and a non-string element is
// skipped, never fatal - matching Rancher's own transformer, which casts
// each principalIds element independently and continues past failed casts.
type PrefixFilter struct {
	Path   []string // string-array field, e.g. principalIds
	Prefix string   // e.g. "system://"
	Label  string   // singular noun for warnings/desc, e.g. "system account"
}

// Matches reports whether obj, an object as decoded from JSON, carries
// an element with the prefix in the string array at Path. A missing or
// non-array field never matches; each element is string-asserted on its
// own and a non-string element is skipped, never fatal - the rule
// Rancher's user-store transformer applies to principalIds. A nil filter
// matches nothing: the kind has no system accounts.
func (f *PrefixFilter) Matches(obj map[string]any) bool {
	if f == nil {
		return false
	}
	var cur any = obj
	for _, key := range f.Path {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur = m[key]
	}
	values, ok := cur.([]any)
	if !ok {
		return false
	}
	for _, v := range values {
		if s, ok := v.(string); ok && strings.HasPrefix(s, f.Prefix) {
			return true
		}
	}
	return false
}

// Plural returns the English plural of a lowercase kind name. Sibilant endings
// take -es, a consonant followed by -y takes -ies, everything else -s. Used
// for the alias-invariant check in validateKinds, by hintToken's fallback, and
// by external callers that need a plain plural form without registering a kind
// (e.g. get's Usage string). Identical semantics to the former resources.plural,
// which is now removed - this is the canonical single copy.
func Plural(name string) string {
	switch {
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "x"),
		strings.HasSuffix(name, "z"), strings.HasSuffix(name, "ch"),
		strings.HasSuffix(name, "sh"):
		return name + "es"
	case len(name) > 1 && strings.HasSuffix(name, "y") && !isVowel(name[len(name)-2]):
		return name[:len(name)-1] + "ies"
	default:
		return name + "s"
	}
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// hintToken derives the CLI plural token for a kind's NOT_FOUND hint. The
// table invariant (enforced by validateKinds) is that Aliases[0] is always
// the plural of the hyphen-stripped name - "fleet-cluster" → "fleetclusters".
// When no alias is registered, plural(hyphen-stripped Name) is the fallback.
func hintToken(k Kind) string {
	if len(k.Aliases) > 0 {
		return k.Aliases[0]
	}
	return Plural(strings.ReplaceAll(k.Name, "-", ""))
}

// NotFoundCurated returns the curated NOT_FOUND error for a named-get miss:
// "<kind> not found: <name>" with a hint naming the list command. The dynamic
// client calls this after mapErr when ByGVR resolves the GVR to a kind row -
// that is the single seam, and it passes the --cluster value the failing
// invocation itself was already scoped to. Exported so test helpers can
// build the expected curated shape without duplicating the hint formula.
// A Downstream-scoped kind's list command is never valid without --cluster,
// so the hint carries it; a Management-scoped kind's list command needs no
// cluster and clusterID is ignored.
//
// Rule: an error that ALREADY CARRIES A HINT is already curated - the dynamic
// client checks for an existing hint in curateNotFound and passes such errors
// through untouched. Only hint-less NOT_FOUNDs (the raw apiserver passthrough)
// get the "<kind> not found: <name>" treatment.
func NotFoundCurated(k Kind, name, clusterID string) error {
	hint := "list with: " + clierr.Rancher + " get " + hintToken(k)
	if k.Scope == Downstream {
		hint += " --cluster " + clierr.ShellQuote(clusterID)
	}
	return clierr.New(clierr.CodeNotFound, k.Name+" not found: "+name).
		WithHint(hint)
}

// ForbiddenHint is the caller-scoped command for an RBAC refusal of a
// kind verb, or "" when there is none: the generated verbs keep kubectl's
// semantics on the local cluster's proxy, where Rancher answers some
// management kinds only for users with access to the local cluster, and
// the access and inspect commands answer from what the caller can read.
// namespace and name are the values the request carried.
func ForbiddenHint(k Kind, verb, namespace, name string) string {
	switch {
	case k.Key == "managementcluster" && verb == "list":
		return clierr.Rancher + " access clusters"
	case k.Key == "managementcluster" && verb == "get" && name != "":
		return clierr.Rancher + " inspect cluster " + clierr.ShellQuote(name)
	case k.Key == "project" && verb == "list" && namespace != "":
		return clierr.Rancher + " access projects --cluster " + clierr.ShellQuote(namespace)
	case k.Key == "project" && verb == "list":
		return clierr.Rancher + " access projects"
	}
	return ""
}

// groupResource is the (Group, Resource) pair used as a map key for O(1)
// kind lookups in kindsByGR and in validateKinds' uniqueness check.
type groupResource struct{ group, resource string }

// kindsByGR is the static Group+Resource → Kind index built once from the
// table. ByGVR uses it for O(1) lookup instead of a linear scan. The
// Group+Resource uniqueness invariant (enforced by validateKinds) guarantees
// the map is injective.
var kindsByGR = func() map[groupResource]Kind {
	m := make(map[groupResource]Kind, len(table))
	for _, k := range table {
		m[groupResource{k.GVR.Group, k.GVR.Resource}] = k
	}
	return m
}()

// wildcardVersionMatch reports whether a table row's version matches an
// incoming version. An empty row version is a discovery-resolved wildcard
// that matches any incoming version.
func wildcardVersionMatch(rowVersion, incoming string) bool {
	return rowVersion == "" || rowVersion == incoming
}

// ByGVR resolves a GroupVersionResource to a kind table row by matching
// Group and Resource. Empty-Version rows (CAPI kinds whose served version
// varies by Rancher release) match any incoming version for their Group+Resource;
// non-empty Version rows require an exact version match. Returns the matching
// Kind and true, or the zero Kind and false when no row matches.
//
// ByGVR is safe to call concurrently; it reads the package-level kindsByGR
// index without modification.
func ByGVR(gvr schema.GroupVersionResource) (Kind, bool) {
	k, ok := kindsByGR[groupResource{gvr.Group, gvr.Resource}]
	if !ok {
		return Kind{}, false
	}
	if !wildcardVersionMatch(k.GVR.Version, gvr.Version) {
		return Kind{}, false
	}
	return k, true
}

// MachineConfigGroup is the GVR group shared by every rke-machine-config
// kind row below (amazonec2config, azureconfig, digitaloceanconfig,
// harvesterconfig, vmwarevsphereconfig) - the single source createcluster.go
// filters kinds.All() on to derive the --machine-config kind set.
const MachineConfigGroup = "rke-machine-config.cattle.io"

var table = []Kind{
	{
		Key: "managementcluster", Name: "cluster", Aliases: []string{"clusters"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"},
		Policy: NSNone, K8sKind: "Cluster", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",

		ArgDesc: "cluster id or display name",

		NoDeleteReason: "to delete a cluster, use delete provisioning-cluster",
		Completable:    true,
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns:        []Column{{Header: "DISPLAY NAME", Path: []string{"spec", "displayName"}}, {Header: "READY", Condition: "Ready"}},
	},
	{
		Key: "project", Name: "project", Aliases: []string{"projects"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"},
		Policy: NSCluster, K8sKind: "Project", Scope: Management, Deletable: true,

		Handle:  &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"spec", "displayName"}, Label: "display name"}}},
		ArgDesc: "project id (p-xxxxx; a project created with rancher create project has the name you gave as its id)",

		// Rancher deletes the namespaces of the project that carry
		// field.cattle.io/creatorId and keeps the rest, removing their project
		// annotation; role bindings and project-scoped secrets go regardless.
		DeleteCascadeWarning: "deleting this project keeps its namespaces and their workloads, except a namespace with the field.cattle.io/creatorId annotation (which Rancher writes on namespaces created through its v3 API): that namespace is deleted with everything in it. Rancher removes the project annotation from each kept namespace and deletes the project's role bindings and project-scoped secrets.",
		Completable:          true,
		Binding:              safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		Columns: []Column{
			{Header: "DISPLAY NAME", Path: []string{"spec", "displayName"}},
			{Header: "CLUSTER", Path: []string{"spec", "clusterName"}},
			{Header: "BACKING-NS", Path: []string{"status", "backingNamespace"}},
			{Header: "DESCRIPTION", Path: []string{"spec", "description"}, Wide: true},
		},
	},
	{
		Key: "node", Name: "node", Aliases: []string{"nodes"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"},
		Policy: NSNone, K8sKind: "Node", Scope: Downstream, Deletable: false, WaitDefault: "condition=Ready",
		NoDeleteReason: "nodes are created and deleted by the machine pools of a cluster that has them (change a machine pool's number of nodes with cluster scale), or outside Rancher for an imported, custom or local cluster",
		Completable:    true,
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable:    true,
	},
	{
		Key: "namespace", Name: "namespace", Aliases: []string{"namespaces", "ns"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
		Policy: NSNone, K8sKind: "Namespace", Scope: Downstream, Deletable: true,
		Completable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "pod", Name: "pod", Aliases: []string{"pods", "po"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Policy: NSPlain, K8sKind: "Pod", Scope: Downstream, Deletable: true, WaitDefault: "condition=Ready",
		Completable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "deployment", Name: "deployment", Aliases: []string{"deployments", "deploy"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		Policy: NSPlain, K8sKind: "Deployment", Scope: Downstream, Deletable: true, WaitDefault: "condition=Available",
		Completable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "service", Name: "service", Aliases: []string{"services", "svc"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		Policy: NSPlain, K8sKind: "Service", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "configmap", Name: "configmap", Aliases: []string{"configmaps", "cm"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Policy: NSPlain, K8sKind: "ConfigMap", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "secret", Name: "secret", Aliases: []string{"secrets"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		Policy: NSPlain, K8sKind: "Secret", Scope: Downstream, Deletable: true, Sensitive: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},

	{
		Key: "statefulset", Name: "statefulset", Aliases: []string{"statefulsets", "sts"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
		Policy: NSPlain, K8sKind: "StatefulSet", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "daemonset", Name: "daemonset", Aliases: []string{"daemonsets", "ds"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		Policy: NSPlain, K8sKind: "DaemonSet", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "replicaset", Name: "replicaset", Aliases: []string{"replicasets", "rs"},
		GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"},
		Policy: NSPlain, K8sKind: "ReplicaSet", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "job", Name: "job", Aliases: []string{"jobs"},
		GVR:    schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"},
		Policy: NSPlain, K8sKind: "Job", Scope: Downstream, Deletable: true, WaitDefault: "condition=Complete",
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "cronjob", Name: "cronjob", Aliases: []string{"cronjobs"},
		GVR:    schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"},
		Policy: NSPlain, K8sKind: "CronJob", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "ingress", Name: "ingress", Aliases: []string{"ingresses", "ing"},
		GVR:    schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
		Policy: NSPlain, K8sKind: "Ingress", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "persistentvolume", Name: "persistentvolume", Aliases: []string{"persistentvolumes", "pv"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"},
		Policy: NSNone, K8sKind: "PersistentVolume", Scope: Downstream, Deletable: false,
		NoDeleteReason: "a persistent volume is released when its claim is deleted; delete the persistentvolumeclaim instead",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable:    true,
	},
	{
		Key: "persistentvolumeclaim", Name: "persistentvolumeclaim", Aliases: []string{"persistentvolumeclaims", "pvc"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
		Policy: NSPlain, K8sKind: "PersistentVolumeClaim", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "serviceaccount", Name: "serviceaccount", Aliases: []string{"serviceaccounts", "sa"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"},
		Policy: NSPlain, K8sKind: "ServiceAccount", Scope: Downstream, Deletable: true,
		Binding:     safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable: true,
	},
	{
		Key: "event", Name: "event", Aliases: []string{"events", "ev"},
		GVR:    schema.GroupVersionResource{Group: "", Version: "v1", Resource: "events"},
		Policy: NSPlain, K8sKind: "Event", Scope: Downstream, Deletable: false,
		NoDeleteReason: "an event is deleted when its TTL runs out",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		ServerTable:    true,
	},

	{
		Key: "gitrepo", Name: "gitrepo", Aliases: []string{"gitrepos"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "gitrepos"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "GitRepo", Scope: Management, Deletable: true, WaitDefault: "condition=Ready",
		WaitFailsOnStalled:   true,
		DeleteCascadeWarning: "deleting this GitRepo makes Fleet delete its Bundles and their BundleDeployments, which uninstalls the deployed workloads from every target cluster",
		Binding:              safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "bundle", Name: "bundle", Aliases: []string{"bundles"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundles"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Bundle", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		WaitFailsOnStalled: true,
		NoDeleteReason:     "this CLI does not delete bundles; deleting a GitRepo deletes the bundles Fleet created from it",
		Binding:            safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "bundledeployment", Name: "bundledeployment", Aliases: []string{"bundledeployments"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "bundledeployments"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "BundleDeployment", Scope: Management, Deletable: false,
		NoDeleteReason: "Fleet creates and manages it from a bundle; deleting a GitRepo deletes its bundles and their bundle deployments",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "clustergroup", Name: "clustergroup", Aliases: []string{"clustergroups"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "clustergroups"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "ClusterGroup", Scope: Management, Deletable: false,
		NoDeleteReason: "this CLI does not delete cluster groups",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		// K8sKind "Cluster" is shared with provisioningcluster/capicluster -
		// branch 2's manifest reverse lookup disambiguates by apiVersion group.
		Key: "fleetcluster", Name: "fleet-cluster", Aliases: []string{"fleetclusters", "fleet-clusters"},
		GVR:    schema.GroupVersionResource{Group: "fleet.cattle.io", Version: "v1alpha1", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, Sensitive: true, WaitDefault: "condition=Ready",
		WaitFailsOnStalled: true,
		NoDeleteReason:     "a controller creates and manages it; delete the object it was created from",
		Binding:            safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns:            []Column{{Header: "LAST-SEEN", Path: []string{"status", "agent", "lastSeen"}}},
	},

	{
		// Setting is a flat CRD: value/default/customized/source live at the
		// object root, not under spec/status. An effective-value VALUE column
		// (value when set, else default) is a plain string AltPaths fallback -
		// but some settings (cacerts and friends) carry credential-adjacent
		// material in value/default, and support-bundle's settingsAllowlist
		// (internal/engine/observe/bundle.go) deliberately withholds every
		// setting outside a short named list. The Column axes have no
		// per-object, name-keyed conditional (Path/AltPaths/Default/Join/Bool/
		// Condition all project the same field(s) for every row) - expressing
		// "reveal for these names, withhold for the rest" needs a hook this
		// package does not have, so VALUE stays unshipped rather than bending
		// an axis. CUSTOMIZED and SOURCE carry no secret material (provenance
		// only), so they ship instead.
		Key: "setting", Name: "setting", Aliases: []string{"settings"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "settings"},
		Policy: NSNone, K8sKind: "Setting", Scope: Management, Deletable: false, Sensitive: true,
		NoDeleteReason: "a Rancher setting or feature flag is changed with patch, not deleted",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns: []Column{
			{Header: "CUSTOMIZED", Path: []string{"customized"}},
			{Header: "SOURCE", Path: []string{"source"}},
		},
	},
	{
		// Feature's interesting fields (spec.value, status.default,
		// status.dynamic, status.lockedValue) are BOOLS. The table
		// renderer's typed leaf ladder falls Path/AltPaths through
		// string/bool/int64/float64 leaves, so a merged effective-value
		// column (spec.value when set, else status.default) is expressible
		// as a plain AltPaths row. DEFAULT/DYNAMIC/LOCKED remain their own
		// columns regardless: each is a single bool field kubectl doesn't
		// show, not something the merge folds away.
		Key: "feature", Name: "feature", Aliases: []string{"features"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "features"},
		Policy: NSNone, K8sKind: "Feature", Scope: Management, Deletable: false, Sensitive: true,
		NoDeleteReason: "a Rancher setting or feature flag is changed with patch, not deleted",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns: []Column{
			// VALUE is the merged effective value: spec.value (*bool,
			// omitempty - absent means "follow default") falling through to
			// status.default when absent, via the typed leaf ladder's
			// bool-typed AltPaths fallback.
			{Header: "VALUE", Path: []string{"spec", "value"}, AltPaths: [][]string{{"status", "default"}}},
			{Header: "DEFAULT", Path: []string{"status", "default"}},
			{Header: "DYNAMIC", Path: []string{"status", "dynamic"}},
			{Header: "LOCKED", Path: []string{"status", "lockedValue"}},
			// PRIME-ONLY renders the wire's status.prime - the feature's
			// classification, never server-edition detection (a
			// prime-classified feature reads effective-false on BOTH
			// community and Prime-with-it-disabled). No Bool: an older
			// server's absent field renders blank (unknown), never false.
			{Header: "PRIME-ONLY", Path: []string{"status", "prime"}},
			{Header: "DESCRIPTION", Path: []string{"status", "description"}},
		},
	},
	{
		Key: "user", Name: "user", Aliases: []string{"users"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "users"},
		Policy: NSNone, K8sKind: "User", Scope: Management, Deletable: true, Completable: true, Sensitive: true,

		Handle: &Handle{Arg: "id", Lookup: []HandleField{
			{Path: []string{"username"}, Label: "username"},
			{Path: []string{"displayName"}, Label: "display name"},
		}},
		ArgDesc: "user id (the NAME column of rancher get users), not the username or display name",

		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		// The Norman v3 user store hides any user whose principalIds carry a
		// system:// entry from every API response (user_store.go, "filter
		// system users out of the api") - this row reads the management CRD
		// directly and would otherwise leak what the product hides.
		// Deliberately NARROWER than engine.IsSystemPrincipal ("system:"):
		// that predicate carries RBAC subject semantics; this one mirrors
		// the user store's exact system:// check, so the two differing is
		// intent, not drift.
		SystemFilter: &PrefixFilter{Path: []string{"principalIds"}, Prefix: "system://", Label: "system account"},
		// The v3 User type carries a root-level password field, write-only
		// on the CLI's side but echoed back verbatim in a create response -
		// a server never returns it on a later read, only on the create
		// that set it.
		RedactPaths: [][]string{{"password"}},
		// PRINCIPALS renders every principal raw, comma-joined in server
		// order (local://m-x, system://local, okta_user://…): the auth
		// provider and the system-account marker exactly as the server
		// states them, with no derivation and no ordering assumption -
		// Rancher's own code treats principal order as meaningless (every
		// consumer scans the whole slice). The id prefix carries no type
		// signal at all: EnsureUser creates u-* ids for external-SSO users
		// and system accounts alike, and locally-seeded users get m-* ids.
		// ENABLED: v3 User.enabled is an omitempty *bool whose absence
		// means enabled - Default carries that.
		Columns: []Column{
			{Header: "DISPLAY NAME", Path: []string{"displayName"}},
			{Header: "USERNAME", Path: []string{"username"}},
			{Header: "ENABLED", Path: []string{"enabled"}, Default: "true"},
			{Header: "PRINCIPALS", Path: []string{"principalIds"}, Join: true},
		},
	},
	{
		Key: "roletemplate", Name: "roletemplate", Aliases: []string{"roletemplates"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "roletemplates"},
		Policy: NSNone, K8sKind: "RoleTemplate", Scope: Management, Deletable: false, Completable: true, Sensitive: true,

		Handle:  &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"displayName"}, Label: "display name"}}},
		ArgDesc: "role template id (the NAME column of rancher get roletemplates), not the display name",

		NoDeleteReason: "role templates and global roles are the roles that grant gives and revoke takes away",
		Binding:        safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		Columns:        []Column{{Header: "CONTEXT", Path: []string{"context"}}, {Header: "DISPLAY NAME", Path: []string{"displayName"}}},
	},
	{
		Key: "globalrole", Name: "globalrole", Aliases: []string{"globalroles"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalroles"},
		Policy: NSNone, K8sKind: "GlobalRole", Scope: Management, Deletable: false, Completable: true, Sensitive: true,

		Handle:  &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"displayName"}, Label: "display name"}}},
		ArgDesc: "global role id (the NAME column of rancher get globalroles), not the display name",

		NoDeleteReason: "role templates and global roles are the roles that grant gives and revoke takes away",
		Binding:        safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
	},
	{
		Key: "globalrolebinding", Name: "globalrolebinding", Aliases: []string{"globalrolebindings"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "globalrolebindings"},
		Policy: NSNone, K8sKind: "GlobalRoleBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
	},
	{
		Key: "clusterroletemplatebinding", Name: "clusterroletemplatebinding", Aliases: []string{"clusterroletemplatebindings", "crtb"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusterroletemplatebindings"},
		Policy: NSCluster, K8sKind: "ClusterRoleTemplateBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		// MEMBER column prefers userName → groupPrincipalName → userPrincipalName:
		// the plain bare userName is most useful for operators scanning the table.
		// inspect cluster/project cards deliberately invert this priority
		// (userPrincipalName first) because display-name resolution joins on
		// principalIds. See internal/engine/inspect/members.go collectPrincipalRefs.
		Columns: []Column{
			{Header: "MEMBER", Path: []string{"userName"}, AltPaths: [][]string{{"groupPrincipalName"}, {"userPrincipalName"}}},
			{Header: "ROLE", Path: []string{"roleTemplateName"}},
			{Header: "CLUSTER", Path: []string{"clusterName"}},
		},
	},
	{
		// The one Management + NSPlain kind: --namespace is a project id, not a
		// cluster id - reflected in the generated flag help.
		Key: "projectroletemplatebinding", Name: "projectroletemplatebinding", Aliases: []string{"projectroletemplatebindings", "prtb"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projectroletemplatebindings"},
		Policy: NSPlain, K8sKind: "ProjectRoleTemplateBinding", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		// MEMBER column prefers userName → groupPrincipalName → userPrincipalName:
		// same deliberate divergence as crtb above; see that entry for rationale.
		Columns: []Column{
			{Header: "MEMBER", Path: []string{"userName"}, AltPaths: [][]string{{"groupPrincipalName"}, {"userPrincipalName"}}},
			{Header: "ROLE", Path: []string{"roleTemplateName"}},
			{Header: "PROJECT", Path: []string{"projectName"}},
		},
	},
	{
		// OIDCClient (management.cattle.io/v3, cluster-scoped) is the server
		// side of Rancher's embedded OIDC provider (feature "oidc-provider") -
		// the first kind row to carry a Capability requirement. Sensitive: true escalates
		// generic patch to --yes: patch can set the same lifecycle annotations
		// (create/regenerate/remove a client secret) the curated oidc-client
		// verbs gate behind confirmation, waiting, and one-time delivery - the
		// nodedriver precedent for a generic path that reaches a Rancherism the
		// curated commands exist to hide. Both TTL spec fields are CRD-required
		// (no server defaulting); the curated create command supplies its own
		// 3600/86400 defaults. status.clientSecrets' createdAt/lastUsedAt are
		// unix-seconds STRINGS with no epoch rendering in this table layer - a
		// LAST-USED column is deliberately cut rather than shown raw.
		// DeleteCascadeWarning: the backing Secret carries an OwnerReference to
		// this object (Kubernetes GC, not the oidc-provider controller), so
		// deleting the client invalidates every live secret even with the
		// feature off.
		Key: "oidcclient", Name: "oidcclient", Aliases: []string{"oidcclients"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "oidcclients"},
		Policy: NSNone, K8sKind: "OIDCClient", Scope: Management, Deletable: true, Sensitive: true,
		DeleteCascadeWarning: "deleting this OIDC client also deletes its Secret, which invalidates every client secret; every application that uses one loses access",
		Completable:          true,
		Binding:              safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Capability:           runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "oidc-provider"},
		Columns: []Column{
			{Header: "CLIENT-ID", Path: []string{"status", "clientID"}},
			{Header: "SECRETS", Count: []string{"status", "clientSecrets"}},
			{Header: "TOKEN-EXPIRATION", Path: []string{"spec", "tokenExpirationSeconds"}},
			{Header: "REFRESH-EXPIRATION", Path: []string{"spec", "refreshTokenExpirationSeconds"}},
		},
	},

	{
		Key: "provisioningcluster", Name: "provisioning-cluster", Aliases: []string{"provisioningclusters", "provisioning-clusters"},
		GVR:    schema.GroupVersionResource{Group: "provisioning.cattle.io", Version: "v1", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, Completable: true, Sensitive: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		// The CRD's additionalPrinterColumns declares status.ready's type as
		// "string", so a plain Path is correct - the typed leaf ladder
		// renders a real bool leaf identically if the wire ever carries one.
		Columns: []Column{
			{Header: "VERSION", Path: []string{"spec", "kubernetesVersion"}},
			{Header: "CLUSTER-NAME", Path: []string{"status", "clusterName"}},
			{Header: "READY", Path: []string{"status", "ready"}},
		},
	},
	{
		// Empty GVR version: cluster.x-k8s.io is resolved via discovery at call
		// time (the served version varies by Rancher release).
		Key: "capicluster", Name: "capi-cluster", Aliases: []string{"capiclusters", "capi-clusters"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "clusters"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Cluster", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		NoDeleteReason: "a controller creates and manages it; delete the object it was created from",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "capimachine", Name: "machine", Aliases: []string{"machines"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machines"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Machine", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		NoDeleteReason: "a controller creates and manages it; to delete a machine of a machine pool, change the machine pool's number of nodes with cluster scale",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "capimachineset", Name: "machineset", Aliases: []string{"machinesets"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machinesets"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "MachineSet", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		NoDeleteReason: "a controller creates and manages it from a machine pool of a provisioning cluster; change the machine pool's number of nodes with cluster scale",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "capimachinedeployment", Name: "machinedeployment", Aliases: []string{"machinedeployments"},
		GVR:    schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "", Resource: "machinedeployments"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "MachineDeployment", Scope: Management, Deletable: false, WaitDefault: "condition=Ready",
		NoDeleteReason: "a controller creates and manages it from a machine pool of a provisioning cluster; change the machine pool's number of nodes with cluster scale",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "k3kcluster", Name: "k3k-cluster", Aliases: []string{"k3kclusters", "k3k-clusters"},
		GVR:    schema.GroupVersionResource{Group: "k3k.io", Version: "v1beta1", Resource: "clusters"},
		Policy: NSPlain, K8sKind: "Cluster", Scope: Downstream, Deletable: true, WaitDefault: "condition=Ready",
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		// Empty GVR version: ext.cattle.io is resolved via discovery at call
		// time; group absence means an older Rancher without the aggregated
		// imperative API (both kinds shipped in v2.12; the CLI's own
		// supported floor is now 2.14).
		Key: "token", Name: "token", Aliases: []string{"tokens"},
		GVR:    schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "tokens"},
		Policy: NSNone, K8sKind: "Token", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		Columns: []Column{
			// USER/KIND/EXPIRES/DESCRIPTION are plain strings on the wire;
			// there is still no EXPIRED bool column - EXPIRES carries the
			// signal.
			{Header: "USER", Path: []string{"spec", "userID"}},
			{Header: "KIND", Path: []string{"spec", "kind"}},
			{Header: "EXPIRES", Path: []string{"status", "expiresAt"}},
			{Header: "DESCRIPTION", Path: []string{"spec", "description"}},
		},
		// The create answer carries the token's value once, in status.bearerToken
		// and status.value; no other answer of the object does.
		RedactPaths: [][]string{{"status", "bearerToken"}, {"status", "value"}},
		// A manifest cannot create a token: `create token` shows the value
		// once under the once-only output rules.
		NoCreateViaManifest: "create tokens with '" + clierr.Rancher + " create token' (it shows the token's value once)",
	},
	{
		// Not on the Rancher Kubernetes Public API list; deprecated in favor of
		// ext.cattle.io Token; still serving UI session tokens and derived tokens
		// in Rancher 2.15. The v3 Token type (pkg/apis/management.cattle.io/v3)
		// has its fields at the object root, not under spec/status.
		Key: "managementtoken", Name: "management-token", Aliases: []string{"managementtokens", "management-tokens", "v3token", "v3tokens"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "tokens"},
		Policy: NSNone, K8sKind: "Token", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Deprecated},
		Columns: []Column{
			// Plain strings, root-level fields on the v3 Token type (userId,
			// authProvider, expiresAt, description).
			{Header: "USER", Path: []string{"userId"}},
			{Header: "AUTH PROVIDER", Path: []string{"authProvider"}},
			{Header: "EXPIRES", Path: []string{"expiresAt"}},
			{Header: "DESCRIPTION", Path: []string{"description"}},
		},
		// management.cattle.io/v3 Token carries the raw credential at root-level
		// .token (writeOnly in Norman; plaintext when Rancher's token-hashing
		// feature is off, which is the default). Scrub it from every serialized
		// representation - get/list JSON/YAML output and patch diff entries.
		RedactPaths: [][]string{{"token"}},
		// A manifest cannot create a token: `create token` (the ext.cattle.io
		// kind) applies the key generation, TTL clamp and identity derivation
		// a new token needs.
		NoCreateViaManifest: "create tokens with '" + clierr.Rancher + " create token' (it creates an ext.cattle.io Token)",
	},
	{
		Key: "kubeconfig", Name: "kubeconfig", Aliases: []string{"kubeconfigs"},
		GVR:    schema.GroupVersionResource{Group: "ext.cattle.io", Version: "", Resource: "kubeconfigs"},
		Policy: NSNone, K8sKind: "Kubeconfig", Scope: Management, Deletable: true, Sensitive: true,
		Binding: safety.Binding{Contract: safety.ContractPublic, Stability: safety.Stable}, // Rancher Kubernetes Public API (semi-official list, 2026-07-30)
		Columns: []Column{
			// TTL and TOKENS/CLUSTERS are omitted: their exact wire paths
			// are not verified against a live server - not a renderer
			// limit. Add them once a server confirms the paths
			// (TOKENS/CLUSTERS would be Count-expressible).
			{Header: "SUMMARY", Path: []string{"status", "summary"}},
			// DESCRIPTION is wide-only: kubectl's own printer treats it (and
			// USER/CLUSTERS) as wide-only material too.
			{Header: "DESCRIPTION", Path: []string{"spec", "description"}, Wide: true},
		},
		// The create answer carries the kubeconfig once, in status.value.
		RedactPaths: [][]string{{"status", "value"}},
		// A manifest cannot create a kubeconfig: `create kubeconfig` prints it
		// once under the once-only output rules.
		NoCreateViaManifest: "create kubeconfigs with '" + clierr.Rancher + " create kubeconfig' (it prints the kubeconfig once)",
	},

	{
		// K8sKind casing: derived from rancher's convert.Capitalize(driverName +
		// "Config") rule (dynamicschema.go:66-84,304-337; driver names from
		// machinedriver_data.go:24-39). "Amazonec2Config" is literal-confirmed in
		// rancher's own tests (status_test.go:70); the other four machine-config
		// kinds below follow the same capitalization rule but are not individually
		// literal-confirmed - verify against a real server before relying on the
		// exact casing.
		// Sensitive: false deliberately - these CONFIGS carry instance parameters
		// (region, size, image) for the node driver; credentials live in the
		// driver's cc-* Secret, not on this resource.
		Key: "amazonec2config", Name: "amazonec2config", Aliases: []string{"amazonec2configs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "amazonec2configs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "Amazonec2Config", Scope: Management, Deletable: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "azureconfig", Name: "azureconfig", Aliases: []string{"azureconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "azureconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "AzureConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		// K8sKind "DigitaloceanConfig" confirmed against
		// rancher's convert.Capitalize(driverName+"Config") rule
		// (dynamicschema.go addConfigSchema) during the create
		// machine-config field-curation pass; field vocabulary lives in
		// machineconfigfields.go.
		Key: "digitaloceanconfig", Name: "digitaloceanconfig", Aliases: []string{"digitaloceanconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "digitaloceanconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "DigitaloceanConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "harvesterconfig", Name: "harvesterconfig", Aliases: []string{"harvesterconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "harvesterconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "HarvesterConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},
	{
		Key: "vmwarevsphereconfig", Name: "vmwarevsphereconfig", Aliases: []string{"vmwarevsphereconfigs"},
		GVR:    schema.GroupVersionResource{Group: MachineConfigGroup, Version: "v1", Resource: "vmwarevsphereconfigs"},
		Policy: NSWorkspace, DefaultNamespace: "fleet-default", K8sKind: "VmwarevsphereConfig", Scope: Management, Deletable: true,
		Binding: safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
	},

	{
		// The server-side registry of node drivers (management.cattle.io/v3
		// NodeDriver) - the discovery surface behind `create cluster`'s
		// node-driver mode. Read surface only for now: Deletable: false, and
		// Sensitive: true so the generated patch (toggling spec.active flips
		// driver availability platform-wide, like a setting/feature) escalates
		// to --yes. Column field paths follow rancher's NodeDriverSpec
		// (machine_types.go): displayName is a string; active and builtin are
		// BOOLS (Bool: true), rendered via the table renderer's bool fallback.
		// builtin is `json:"builtin,omitempty"` upstream, so a non-builtin
		// driver omits the field - Bool makes the absent field render "false"
		// instead of blank (active, `json:"active"`, is always present, but
		// is marked too: it is boolean-typed all the same).
		Key: "nodedriver", Name: "nodedriver", Aliases: []string{"nodedrivers"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "nodedrivers"},
		Policy: NSNone, K8sKind: "NodeDriver", Scope: Management, Deletable: false, Sensitive: true,

		Handle:  &Handle{Arg: "id", Lookup: []HandleField{{Path: []string{"spec", "displayName"}, Label: "display name"}}},
		ArgDesc: "node driver id (the NAME column of rancher get nodedrivers), not the display name",

		NoDeleteReason: "a node driver is deactivated by setting spec.active to false, not deleted; Rancher manages the built-in drivers",
		Completable:    true,
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns: []Column{
			{Header: "DISPLAY NAME", Path: []string{"spec", "displayName"}},
			{Header: "ACTIVE", Path: []string{"spec", "active"}, Bool: true},
			{Header: "BUILTIN", Path: []string{"spec", "builtin"}, Bool: true},
			{Header: "URL", Path: []string{"spec", "url"}, Wide: true},
		},
	},

	{
		// Namespace = the cluster's management namespace; NSPlain gives the
		// --namespace flag. Deletable: false - the generic verb surface does not
		// delete CRTs. This row carries identity only: its get is
		// overridden to a Steve read and its patch is refused.
		Key: "clusterregistrationtoken", Name: "clusterregistrationtoken", Aliases: []string{"clusterregistrationtokens", "crt"},
		GVR:    schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusterregistrationtokens"},
		Policy: NSPlain, K8sKind: "ClusterRegistrationToken", Scope: Management, Deletable: false,
		NoDeleteReason: "this CLI does not delete cluster registration tokens",
		Binding:        safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta},
		Columns:        []Column{{Header: "CLUSTER", Path: []string{"spec", "clusterName"}}},
	},
}

func All() []Kind { out := make([]Kind, len(table)); copy(out, table); return out }

// Bindings returns the contract bindings for generated generic ops: "get <Name>"
// and "patch <Name>" for every kind, and "delete <Name>" for deletable kinds.
// BuildRoot composes these with the bespoke resolver (single source per op).
func Bindings() map[string]safety.Binding {
	out := map[string]safety.Binding{}
	for _, k := range table {
		out["get "+k.Name] = k.Binding
		out["patch "+k.Name] = k.Binding
		if k.Deletable {
			out["delete "+k.Name] = k.Binding
		}
	}
	return out
}

// ProjectScopable reports whether --project applies to k: a
// Downstream NSPlain kind validates/filters by project membership, plus the
// one carve-out - the namespace kind itself (NSNone/Downstream), which
// converts an unnamed --project into a label-selector list rather than a
// rejection (Route's default branch). Every other kind rejects a non-empty
// project outright. This is the single source of the rule; Route and the
// generated-command descriptor builders both consume it.
func ProjectScopable(k Kind) bool {
	if k.Policy == NSPlain && k.Scope == Downstream {
		return true
	}
	return k.Name == "namespace" && k.Policy == NSNone && k.Scope == Downstream
}

// LookupGVK resolves a manifest's apiVersion+kind to a table row. Group,
// VERSION, and K8sKind must all match - four rows share K8sKind "Cluster"
// across groups, and a version mismatch (apps/v1beta1 Deployment) must be
// rejected, never silently dispatched on the table's fixed GVR. Rows with an
// empty GVR.Version (CAPI) match any version of their group.
func LookupGVK(apiVersion, kind string) (Kind, bool) {
	group, version := "", apiVersion
	if before, after, ok := strings.Cut(apiVersion, "/"); ok {
		group, version = before, after
	}
	for _, k := range table {
		if k.K8sKind != kind || k.GVR.Group != group {
			continue
		}
		if wildcardVersionMatch(k.GVR.Version, version) {
			return k, true
		}
	}
	return Kind{}, false
}

// ByKey resolves a canonical key alone to a Kind: a Name or alias does
// not match, so a table keyed by Key cannot be satisfied by a spelling
// the surface accepts from an operator.
func ByKey(key string) (Kind, bool) {
	for _, k := range table {
		if k.Key == key {
			return k, true
		}
	}
	return Kind{}, false
}

// Lookup resolves a CLI-facing name, alias, or canonical key to a Kind.
func Lookup(q string) (Kind, bool) {
	for _, k := range table {
		if q == k.Name || q == k.Key {
			return k, true
		}
		if slices.Contains(k.Aliases, q) {
			return k, true
		}
	}
	return Kind{}, false
}

// Validate rejects Key duplicates and Name/Alias/Key collisions across kinds
// (BuildRoot calls it).
func Validate() error { return validateKinds(table) }

func validateKinds(ts []Kind) error {
	for _, k := range ts {
		if k.K8sKind == "" {
			return fmt.Errorf("kind %q has empty K8sKind", k.Name)
		}
		// Name is every generic create/patch/delete Effect's DisplayKind
		// (engine.PlanCreate/PlanUpdate/resources.PlanFromFetched pass it
		// straight through) - an empty value here would pass unnoticed
		// until checkPlan rejects the resulting Effect as a command bug on
		// the kind's first real mutation.
		if k.Name == "" {
			return fmt.Errorf("kind %q has empty Name", k.Key)
		}
	}
	// Key uniqueness is its own pass: the self-Key exemption in the collision
	// pass below would otherwise mask two kinds sharing a Key.
	seenKeys := map[string]bool{}
	for _, k := range ts {
		if seenKeys[k.Key] {
			return fmt.Errorf("duplicate kind Key %q", k.Key)
		}
		seenKeys[k.Key] = true
	}
	// Checks run per kind in table order; the FIRST defect anywhere wins.
	// Identifier-collision and column-condition checks share this loop so a
	// single kind can trigger at most one error, and an earlier kind's defect
	// always takes precedence over a later kind's defect of a different type.
	seen := map[string]string{}
	for _, k := range ts {
		if ce := runtime.ValidCapabilityShape(k.Capability, k.Name); ce != nil {
			return ce
		}
		for _, id := range append([]string{k.Name, k.Key}, k.Aliases...) {
			if prev, dup := seen[id]; dup && prev != k.Key {
				return fmt.Errorf("kind identifier %q claimed by both %q and %q", id, prev, k.Key)
			}
			seen[id] = k.Key
		}
		if k.ServerTable {
			if len(k.Columns) > 0 {
				return fmt.Errorf("kind %q sets ServerTable together with Columns (the server's table has its own columns)", k.Key)
			}
			if k.SystemFilter != nil {
				return fmt.Errorf("kind %q sets ServerTable together with SystemFilter (the server's table is shown as it is)", k.Key)
			}
			if len(k.RedactPaths) > 0 {
				return fmt.Errorf("kind %q sets ServerTable together with RedactPaths (the server's table is shown as it is)", k.Key)
			}
		}
		for _, col := range k.Columns {
			if col.Condition != "" && (len(col.Path) > 0 || len(col.AltPaths) > 0 || col.Bool) {
				return fmt.Errorf("kind %q column %q sets Condition together with Path/AltPaths/Bool", k.Key, col.Header)
			}
			if col.Condition != "" && !unicode.IsUpper(rune(col.Condition[0])) {
				return fmt.Errorf("kind %q column %q: Condition type %q must start with an uppercase letter (k8s CamelCase convention)", k.Key, col.Header, col.Condition)
			}
			if col.Join && (col.Bool || col.Condition != "" || len(col.AltPaths) > 0) {
				return fmt.Errorf("kind %q column %q sets Join together with Bool/Condition/AltPaths", k.Key, col.Header)
			}
			if col.Join && len(col.Path) == 0 {
				return fmt.Errorf("kind %q column %q sets Join without a Path", k.Key, col.Header)
			}
			if col.Default != "" && col.Bool {
				return fmt.Errorf("kind %q column %q sets Default together with Bool (both are absent-fallbacks)", k.Key, col.Header)
			}
			if len(col.Count) > 0 && (len(col.Path) > 0 || len(col.AltPaths) > 0 || col.Bool || col.Default != "" || col.Join || col.Condition != "") {
				return fmt.Errorf("kind %q column %q sets Count together with Path/AltPaths/Bool/Default/Join/Condition", k.Key, col.Header)
			}
		}
		if k.SystemFilter != nil {
			if len(k.SystemFilter.Path) == 0 {
				return fmt.Errorf("kind %q SystemFilter has an empty Path", k.Key)
			}
			if slices.Contains(k.SystemFilter.Path, "") {
				return fmt.Errorf("kind %q SystemFilter Path has an empty segment", k.Key)
			}
			if k.SystemFilter.Prefix == "" {
				return fmt.Errorf("kind %q SystemFilter has an empty Prefix", k.Key)
			}
			if k.SystemFilter.Label == "" {
				return fmt.Errorf("kind %q SystemFilter has an empty Label", k.Key)
			}
		}
		if err := validateHandle(k); err != nil {
			return err
		}
		// Plural-first alias invariant: every row must register at least one
		// alias and Aliases[0] must equal plural(strings.ReplaceAll(k.Name,
		// "-", "")). hintToken relies on this to produce a resolvable CLI
		// token for the NOT_FOUND hint.
		if len(k.Aliases) == 0 {
			return fmt.Errorf("kind %q has no aliases; Aliases[0] must be the plural of the hyphen-stripped name", k.Name)
		}
		expected := Plural(strings.ReplaceAll(k.Name, "-", ""))
		if k.Aliases[0] != expected {
			return fmt.Errorf("kind %q: Aliases[0] must be the plural of the hyphen-stripped name (%q), got %q", k.Name, expected, k.Aliases[0])
		}
	}
	// Group+Resource uniqueness: ByGVR must resolve unambiguously. Two rows may
	// share a Resource name across different Groups (e.g. four "clusters" kinds
	// across management.cattle.io, provisioning.cattle.io, cluster.x-k8s.io, and
	// fleet.cattle.io), but no two rows may share the same Group+Resource - that
	// would make ByGVR return the wrong kind for whichever row comes second.
	// This check runs after the per-kind loop so that a per-kind defect (column,
	// alias) always takes precedence in the first-defect-wins ordering.
	seenGR := map[groupResource]string{}
	for _, k := range ts {
		gr := groupResource{k.GVR.Group, k.GVR.Resource}
		if prev, dup := seenGR[gr]; dup {
			return fmt.Errorf("kind rows %q and %q share Group+Resource {%q, %q}: ByGVR cannot distinguish them", prev, k.Key, k.GVR.Group, k.GVR.Resource)
		}
		seenGR[gr] = k.Key
	}
	return nil
}

// validateHandle rejects a Handle the generated verbs could not use: an
// argument name that is not a DNS-1123 label, a missing ArgDesc, and a lookup
// that names no field, an empty path or segment, or two fields with one
// label.
func validateHandle(k Kind) error {
	h := k.Handle
	if h == nil {
		return nil
	}
	if h.Arg == "" || len(validation.IsDNS1123Label(h.Arg)) > 0 {
		return fmt.Errorf("kind %q Handle.Arg %q must be a DNS-1123 word", k.Key, h.Arg)
	}
	if k.ArgDesc == "" {
		return fmt.Errorf("kind %q has a Handle and an empty ArgDesc", k.Key)
	}
	if len(h.Lookup) == 0 {
		return fmt.Errorf("kind %q Handle has an empty Lookup", k.Key)
	}
	labels := map[string]bool{}
	for _, f := range h.Lookup {
		if len(f.Path) == 0 {
			return fmt.Errorf("kind %q Handle.Lookup has an empty Path", k.Key)
		}
		if slices.Contains(f.Path, "") {
			return fmt.Errorf("kind %q Handle.Lookup path %v has an empty segment", k.Key, f.Path)
		}
		if f.Label == "" {
			return fmt.Errorf("kind %q Handle.Lookup path %v has an empty Label", k.Key, f.Path)
		}
		if labels[f.Label] {
			return fmt.Errorf("kind %q Handle.Lookup repeats the label %q", k.Key, f.Label)
		}
		labels[f.Label] = true
	}
	return nil
}
