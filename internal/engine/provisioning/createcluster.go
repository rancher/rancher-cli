// createcluster.go implements `create cluster`: an
// imported, custom, or node-driver provisioning.cattle.io/v1 Cluster.
package provisioning

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// provisioningClusterGVR is the provisioning.cattle.io/v1 Cluster GVR
// `create cluster` writes, read from the kind table's provisioningcluster
// row - the same live anchor derivedOpKinds relies on (internal/cli), so a
// row rename breaks both by the same key instead of leaving a hand-copied
// GVR silently compiling. The init-time panic guards a static-table miss
// that any test run or binary startup surfaces immediately; there is no
// runtime error path because none can occur (BuildRoot validates the table
// before any command runs).
var provisioningClusterGVR = func() schema.GroupVersionResource {
	row, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		panic("provisioningcluster kind row not found")
	}
	return row.GVR
}()

// provisioningClusterDisplayKind is the same row's canonical CLI kind name
// ("provisioning-cluster") - the display identity `create cluster`'s
// default-format text and dry-run preview use, matching the wording
// `delete provisioning-cluster` already uses for the same resource.
var provisioningClusterDisplayKind = func() string {
	row, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		panic("provisioningcluster kind row not found")
	}
	return row.Name
}()

// creatorIDAnnotation is the annotation rancher-webhook uses for the user
// who created a cluster. The mutating webhook sets it from userInfo on real
// creates but skips dry-run, while the validating webhook requires the match
// on dry-run too - so the CLI sets it first to keep server-side dry-run
// honest. On a real create the mutating webhook overwrites it with the same
// value.
const creatorIDAnnotation = "field.cattle.io/creatorId"

// stampWarningPrefix is the shared prefix of every warning for a creatorId
// annotation the command could not set. Test assertions and the e2e txtar
// substrings are anchored by this const on the code side, so a rename
// propagates everywhere without a silent golden drift.
const stampWarningPrefix = "creatorId annotation not set"

// validUserIDRe is DNS-1123-ish: lowercase alphanumerics, dots, and
// hyphens, starting with a lowercase alphanumeric - the shape of Rancher
// User object names stored in status.userID.
var validUserIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// clusterNamespace is the fixed fleet workspace every provisioning Cluster
// lands in - kinds.go's provisioningcluster row DefaultNamespace; `create
// cluster` has no --namespace flag, unlike the generic NSWorkspace commands.
const clusterNamespace = "fleet-default"

// machineConfigKinds derives the closed set of machine-config kind keys
// `--machine-config <kind>/<name>` accepts from kinds.All() - a strict
// subset of the kind table, so an otherwise-valid kind key (e.g. "pod") is
// still rejected here. Recomputed on every call: kinds.All() is a cheap
// slice copy over a handful of rows, and nothing else in this codebase
// memoizes a derived kind table (kinds.Bindings() itself recomputes fresh on
// every call). kinds.MachineConfigGroup is the single source every
// machine-config kind row shares in kinds.go, so a future machine-config
// driver row added there is picked up here automatically.
func machineConfigKinds() map[string]bool {
	out := map[string]bool{}
	for _, k := range kinds.All() {
		if k.GVR.Group == kinds.MachineConfigGroup {
			out[k.Name] = true
		}
	}
	return out
}

// machineConfigKindNames renders machineConfigKinds sorted - the single
// source for create cluster's VALIDATION hint, create machine-config's
// <kind> Enum, and both schema descs.
func machineConfigKindNames() []string {
	set := machineConfigKinds()
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// machineConfigKindsHint renders the closed kind set for flag descriptions
// and VALIDATION hints - one derivation shared by every mention, so the
// spellings cannot drift.
func machineConfigKindsHint() string {
	return strings.Join(machineConfigKindNames(), ", ")
}

// parseQuantity validates --quantity: empty defaults to 1, anything else
// must be a positive integer (same validation shape as inspect/pod.go's
// parseTail).
func parseQuantity(s string) (int, *clierr.Error) {
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--quantity %q is not a positive integer; must be >= 1", s))
	}
	return n, nil
}

// clusterMode is the three mutually-exclusive shapes build recognizes.
type clusterMode int

const (
	modeImported clusterMode = iota
	modeCustom
	modeNodeDriver
)

type createClusterCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewCreateCluster builds `create cluster <name>` (mutating): an imported
// cluster omits spec.rkeConfig entirely (rancher cluster/controller.go:
// 493,666 - NormalizeCluster(newCluster, cluster.Spec.RKEConfig == nil)
// treats a nil RKEConfig as "not managed by RKE"; nil, not {}, is the
// discriminator); a custom cluster sets spec.rkeConfig: {} with no machine
// pools (nodes register out of band); a node-driver cluster carries exactly
// one machine pool backed by a resolved cloud credential and a
// machine-config CR. Mirrors `create project`'s Plan/build split
// (projects.go). IsImported (imported.go) is the reader-side predicate for
// this shape.
func NewCreateCluster(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &createClusterCmd{clients: clients, roots: roots}
}

func (createClusterCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create cluster",
		Usage: "Create a provisioning cluster (--import, --custom, or --machine-config for a node-driver cluster)",
		Desc: "A node-driver cluster is created with one machine pool; the machine pool " +
			"flags cannot be repeated yet. For a cluster with several machine pools, " +
			"use `create resource -f` with a full manifest. Before the server-side " +
			"dry-run, the command gets your user ID with a SelfUser request " +
			"(ext.cattle.io) and sets the field.cattle.io/creatorId annotation to it: " +
			"the rancher-webhook validating webhook requires the annotation on a " +
			"dry-run, and its mutating webhook sets it only on a real create. If the " +
			"command cannot set the annotation (the request fails or returns no valid " +
			"user ID), it warns \"" + stampWarningPrefix + "\" and continues without " +
			"the annotation. rancher-webhook may then reject the dry-run, and a " +
			"rejected dry-run also stops --apply, which runs the same dry-run first. " +
			"Log in again, or check that you may create selfusers.ext.cattle.io, then " +
			"retry.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "import", Type: "bool", Desc: "imported cluster (no RKE config)"},
			{Name: "custom", Type: "bool", Desc: "custom cluster; nodes join separately via a registration command"},
			// The closed kind set is derived from the SAME single source the
			// VALIDATION hint uses (machineConfigKindNames over
			// kinds.MachineConfigGroup), so schema and hint can never drift.
			// Deliberately NOT ArgSpec.Enum: the flag's values are kind/name
			// composites, so the runtime enum validator would reject them all.
			{Name: "machine-config", Type: "string", Desc: "machine config `kind/name` for the machine pool of a node-driver cluster; kinds: " + machineConfigKindsHint()},
			{Name: "cloud-credential", Type: "string", Desc: "cloud credential `id|display name` for a node-driver cluster"},
			{Name: "k8s-version", Type: "string", Desc: "Kubernetes `version`; required for --custom and for a node-driver cluster, optional for --import"},
			{Name: "pool-name", Type: "string", Desc: "machine pool name (default: pool1)"},
			{Name: "etcd", Type: "bool", Desc: "machine pool: etcd role"},
			{Name: "controlplane", Type: "bool", Desc: "machine pool: control-plane role"},
			{Name: "worker", Type: "bool", Desc: "machine pool: worker role"},
			{Name: "quantity", Type: "string", Desc: "number of nodes in the machine pool (default: 1)"},
		},
		Examples: []string{
			"rancher create cluster imported-1 --import --apply",
			"rancher create cluster custom-1 --custom --k8s-version v1.30.5+rke2r1 --apply",
			"rancher create cluster node-1 --machine-config amazonec2config/mycfg --cloud-credential aws-prod " +
				"--k8s-version v1.30.5+rke2r1 --etcd --controlplane --worker --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// build validates input and constructs the Cluster object for every mode.
// It is pure (no ctx, no network): credential resolution and version
// discovery are Plan's job, so mode-exclusivity/shape validation always
// runs first and never pays for a network round-trip it can't use.
func (createClusterCmd) build(in runtime.Input) (*unstructured.Unstructured, clusterMode, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, 0, clierr.New(clierr.CodeValidation, "argument <name> is required")
	}
	name := args[0]

	importFlag := in.Bool("import")
	customFlag := in.Bool("custom")
	machineConfig := in.String("machine-config")
	cloudCred := in.String("cloud-credential")
	nodeDriverFlags := machineConfig != "" || cloudCred != ""

	if importFlag && customFlag {
		return nil, 0, clierr.New(clierr.CodeValidation,
			"--import and --custom cannot be used together; pass exactly one of --import, --custom, or --machine-config with --cloud-credential")
	}
	if (importFlag || customFlag) && nodeDriverFlags {
		return nil, 0, clierr.New(clierr.CodeValidation,
			"--import or --custom cannot be used with --machine-config or --cloud-credential; "+
				"pass exactly one of --import, --custom, or --machine-config with --cloud-credential")
	}

	var mode clusterMode
	switch {
	case importFlag:
		mode = modeImported
	case customFlag:
		mode = modeCustom
	default:
		mode = modeNodeDriver
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": name, "namespace": clusterNamespace},
		"spec":       map[string]any{},
	}}

	k8sVersion := in.String("k8s-version")

	switch mode {
	case modeImported:
		if k8sVersion != "" {
			_ = unstructured.SetNestedField(obj.Object, k8sVersion, "spec", "kubernetesVersion")
		}
		// spec.rkeConfig is deliberately never touched here - nil, never {}.

	case modeCustom:
		if k8sVersion == "" {
			return nil, 0, clierr.New(clierr.CodeValidation, "--custom requires --k8s-version")
		}
		_ = unstructured.SetNestedField(obj.Object, k8sVersion, "spec", "kubernetesVersion")
		_ = unstructured.SetNestedMap(obj.Object, map[string]any{}, "spec", "rkeConfig")

	case modeNodeDriver:
		if err := buildNodeDriverSpec(obj, in, machineConfig, cloudCred, k8sVersion); err != nil {
			return nil, 0, err
		}
	}

	return obj, mode, nil
}

// buildNodeDriverSpec validates and fills spec.kubernetesVersion and the
// single machine pool (spec.rkeConfig.machinePools) for node-driver mode.
// spec.cloudCredentialSecretName is filled in by Plan after resolving
// --cloud-credential over the network.
func buildNodeDriverSpec(obj *unstructured.Unstructured, in runtime.Input, machineConfig, cloudCred, k8sVersion string) error {
	if machineConfig == "" {
		return clierr.New(clierr.CodeValidation,
			"--machine-config is required for a node-driver cluster; pass --machine-config <kind>/<name>, e.g. amazonec2config/mycfg")
	}
	if cloudCred == "" {
		return clierr.New(clierr.CodeValidation, "--cloud-credential is required for a node-driver cluster")
	}
	if k8sVersion == "" {
		return clierr.New(clierr.CodeValidation, "--k8s-version is required for a node-driver cluster")
	}

	cfgKind, cfgName, ok := strings.Cut(machineConfig, "/")
	if !ok || cfgKind == "" || cfgName == "" {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--machine-config %q must be <kind>/<name>, e.g. amazonec2config/mycfg", machineConfig))
	}
	if !machineConfigKinds()[cfgKind] {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--machine-config kind %q is not a recognized machine config kind; one of: %s", cfgKind, machineConfigKindsHint()))
	}
	// machineConfigKinds() is a strict subset of the kind table (every row
	// whose GVR.Group is rke-machine-config.cattle.io), so this lookup always
	// succeeds - it exists only to read K8sKind's exact casing (kinds.go's
	// own source-ref: rancher convert.Capitalize(driverName+"Config"),
	// dynamicschema.go:66-84,304-337) rather than duplicating that rule here.
	cfgTableKind, found := kinds.Lookup(cfgKind)
	if !found {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--machine-config kind %q is not a recognized machine config kind; one of: %s", cfgKind, machineConfigKindsHint()))
	}

	poolName := in.String("pool-name")
	if poolName == "" {
		poolName = "pool1"
	}
	etcd, controlplane, worker := in.Bool("etcd"), in.Bool("controlplane"), in.Bool("worker")
	// Per-pool validation (rancher template.go:328-342): a pool needs at
	// least one role. NO cluster-wide all-three-roles rule - that check is
	// meaningless with exactly one pool by design (see Describe's Usage);
	// the duplicate-pool-name check template.go also runs is vacuous for
	// the same reason and is left for the future multi-pool path.
	if !etcd && !controlplane && !worker {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("machine pool %q must have at least one role (--etcd/--controlplane/--worker)", poolName))
	}

	quantity, qerr := parseQuantity(in.String("quantity"))
	if qerr != nil {
		return qerr
	}

	_ = unstructured.SetNestedField(obj.Object, k8sVersion, "spec", "kubernetesVersion")
	// machineConfigRef's APIVersion is left unset: an empty ObjectReference
	// APIVersion defaults server-side to rke-machine-config.cattle.io/v1
	// (rancher capr/common.go:106) - never set it explicitly here.
	pool := map[string]any{
		"name":             poolName,
		"etcdRole":         etcd,
		"controlPlaneRole": controlplane,
		"workerRole":       worker,
		"quantity":         int64(quantity),
		"machineConfigRef": map[string]any{
			"kind": cfgTableKind.K8sKind,
			"name": cfgName,
		},
	}
	return unstructured.SetNestedSlice(obj.Object, []any{pool}, "spec", "rkeConfig", "machinePools")
}

// stampCreatorID gets the user ID with a SelfUser request and sets the
// creatorId annotation on obj to it. It returns "" on success, or a
// warning - setting the annotation is advisory: failure never blocks the
// command. The SelfUser request is a real (non-dry-run) create - the
// resource is a create-only "whoami" virtual object - making this the
// file's one deliberate network write during Plan, before any --apply
// gate.
func stampCreatorID(ctx context.Context, client engine.ClusterClient, obj *unstructured.Unstructured) string {
	probeResult, err := client.Create(ctx, selfuser.GVR, "", selfuser.New(), false)
	if err != nil {
		return stampContext(err.Error())
	}
	userID := selfuser.UserID(probeResult)
	if userID == "" {
		return stampContext("the SelfUser response has no user ID")
	}
	if !validUserIDRe.MatchString(userID) {
		return stampContext("the SelfUser response has an invalid user ID")
	}
	if err := unstructured.SetNestedField(obj.Object, userID, "metadata", "annotations", creatorIDAnnotation); err != nil {
		return stampContext("could not add it to the cluster object: " + err.Error())
	}
	return ""
}

// stampContext is the warning for a creatorId annotation the command could
// not set. It states the consequence exactly: rancher-webhook may reject
// the dry-run for the missing annotation, and --apply runs the same
// dry-run first. The create cannot proceed until the annotation is set -
// there is no path on which a real create reaches the server past a
// rejected dry-run.
func stampContext(cause string) string {
	return stampWarningPrefix + " (" + cause + "): rancher-webhook may reject the dry-run because the " +
		creatorIDAnnotation + " annotation is missing, and --apply runs the same dry-run first; " +
		"log in again, or check that you may create selfusers.ext.cattle.io, then retry"
}

func (c createClusterCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, mode, err := c.build(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	var credWarning string
	if mode == modeNodeDriver {
		ref, warning, rerr := ResolveCloudCredential(ctx, c.clients, in.String("cloud-credential"))
		if rerr != nil {
			return safety.Effect{}, nil, rerr
		}
		credWarning = warning
		_ = unstructured.SetNestedField(obj.Object, ref.ID, "spec", "cloudCredentialSecretName")
	}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	stampWarning := stampCreatorID(ctx, client, obj)

	// Capture the user ID set on the annotation before the dry-run for
	// post-apply verification. An empty stampWarning means the annotation
	// was set.
	var stampedUserID string
	if stampWarning == "" {
		ann := obj.GetAnnotations()
		stampedUserID = ann[creatorIDAnnotation]
	}

	eff, exec, err := engine.PlanCreate(ctx, client, provisioningClusterGVR, clusterNamespace, obj, engine.LocalCluster, provisioningClusterDisplayKind, "Cluster", false)
	if err != nil {
		if stampWarning != "" {
			// The pipeline's error normalization unwraps to the innermost
			// *clierr.Error and renders ONLY its own fields - a fmt.Errorf
			// wrapper's text never reaches the envelope. Real client errors
			// are always clierr-mapped, so the context of the unset
			// annotation rides the error's warnings (the envelope's
			// top-level warnings), never
			// the hint, which is reserved for a paste-valid command; the
			// string wrapper remains only for plain errors (test fakes,
			// unexpected paths).
			if ce, ok := errors.AsType[*clierr.Error](err); ok {
				return safety.Effect{}, nil, ce.WithWarnings(stampWarning)
			}
			return safety.Effect{}, nil, fmt.Errorf("%w (%s)", err, stampWarning)
		}
		return safety.Effect{}, nil, err
	}

	// Wrap exec for post-apply creatorId verification: if the server returns
	// a different id, rancher-webhook may not be mutating creates as expected.
	if stampedUserID != "" {
		wantID := stampedUserID
		inner := exec
		exec = func(ctx context.Context) (runtime.Result, error) {
			result, err := inner(ctx)
			if err != nil {
				return result, err
			}
			if dataMap, ok := result.Data.(map[string]any); ok {
				gotID, _, _ := unstructured.NestedString(dataMap, "metadata", "annotations", creatorIDAnnotation)
				// An empty gotID means the response carried no annotations -
				// a trimmed create response is indistinguishable from a
				// dropped annotation, so skip (can't verify) rather than
				// emit a false mismatch.
				if gotID != "" && gotID != wantID {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("the creatorId annotation on the created cluster (%s) differs from your user ID (%s): check that the rancher-webhook mutating webhook runs on create requests", gotID, wantID))
				}
			}
			return result, nil
		}
	}

	eff.Warnings = c.versionWarnings(ctx, in.String("k8s-version"))
	if credWarning != "" {
		eff.Warnings = append(eff.Warnings, credWarning)
	}
	if stampWarning != "" {
		eff.Warnings = append(eff.Warnings, stampWarning)
	}
	return eff, exec, nil
}

// versionWarnings runs FetchVersions for a WARN-ONLY note: a version absent
// from the merged rke2/k3s discovery list appends one warning naming the distro
// defaults, appended to Effect.Warnings BEFORE confirmation. Any FetchVersions
// error (server unreachable, older server) degrades to no warning at all,
// silently - the server is the authority on whether the version is real;
// discovery here is advisory only and must never turn into a hard failure of an
// otherwise valid create.
func (c createClusterCmd) versionWarnings(ctx context.Context, k8sVersion string) []string {
	if k8sVersion == "" {
		return nil
	}
	versions, err := FetchVersions(ctx, c.roots, c.clients)
	if err != nil {
		return nil
	}
	var defaults []string
	for _, v := range versions {
		if v.Version == k8sVersion {
			return nil
		}
		if v.Default {
			defaults = append(defaults, v.Distro+" "+v.Version)
		}
	}
	msg := fmt.Sprintf("Kubernetes version %q is not among the rke2 and k3s releases the server lists", k8sVersion)
	if len(defaults) > 0 {
		msg += " (default versions: " + strings.Join(defaults, ", ") + ")"
	}
	return []string{msg}
}

var _ runtime.MutatingCommand = (*createClusterCmd)(nil)
