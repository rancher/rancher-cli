// createmachineconfig.go implements `create machine-config <kind> <name>`:
// guided authoring of the rke-machine-config.cattle.io
// CR that `create cluster --machine-config <kind>/<name>` references.
// Driver fields sit at the CR ROOT alongside apiVersion/kind/metadata -
// machine-config resources have NO spec wrapper (rancher's
// dynamicschema.addConfigSchema publishes driver fields as top-level
// ResourceFields).
package provisioning

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// doKind is the one machine config kind with its own flags.
const doKind = "digitaloceanconfig"

// doCuratedFlag maps one curated CLI flag to its DigitaloceanConfig driver
// field. isBool marks the tri-state booleans: written only when
// the flag is present, so the server/driver default stays authoritative
// when omitted. Strings (including the comma-separated tags list) are
// written verbatim.
type doCuratedFlag struct {
	flag   string
	field  string
	isBool bool
}

// doCuratedFlags - field names source-confirmed against the DigitalOcean
// machine driver (kinds.MachineConfigFieldsFor's file doc names the exact
// upstream files); the Curated set in kinds/machineconfigfields.go asserts
// the same seven fields.
var doCuratedFlags = []doCuratedFlag{
	{flag: "region", field: "region"},
	{flag: "size", field: "size"},
	{flag: "image", field: "image"},
	{flag: "monitoring", field: "monitoring", isBool: true},
	{flag: "ipv6", field: "ipv6", isBool: true},
	{flag: "private-networking", field: "privateNetworking", isBool: true},
	{flag: "tags", field: "tags"},
}

type createMachineConfigCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateMachineConfig builds `create machine-config <kind> <name>`
// (mutating): a standalone machine-config CR in fleet-default, referenced
// afterwards by `create cluster --machine-config <kind>/<name>`. Explicit
// name, never generateName; fixed fleet-default namespace, no
// --namespace flag. Mirrors create cluster's build/Plan split.
func NewCreateMachineConfig(clients engine.ClusterClientFactory) runtime.Command {
	return &createMachineConfigCmd{clients: clients}
}

func (createMachineConfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create machine-config",
		Usage: "Create a machine config for the machine pool of a node-driver cluster",
		Desc: "Creates a machine config (an rke-machine-config.cattle.io custom resource) " +
			"in the fleet-default namespace. Pass it to " +
			"`create cluster --machine-config <kind>/<name>` afterwards. Driver fields " +
			"are top-level fields of the object, not under spec. digitaloceanconfig " +
			"has its own flags for common fields; amazonec2config, azureconfig, " +
			"harvesterconfig and vmwarevsphereconfig take their fields with " +
			"--set key=value. A field that holds a credential is rejected: put " +
			"credentials in a cloud credential (a cc-* Secret), never in a machine " +
			"config.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "kind", Type: "string", Required: true, Enum: machineConfigKindNames()},
			{Name: "name", Type: "string", Required: true},
		},
		Flags: []runtime.ArgSpec{
			{Name: "region", Type: "string", Desc: "digitaloceanconfig: droplet `region` slug (e.g. nyc3)"},
			{Name: "size", Type: "string", Desc: "digitaloceanconfig: droplet `size` slug (e.g. s-2vcpu-4gb)"},
			{Name: "image", Type: "string", Desc: "digitaloceanconfig: droplet `image` slug"},
			{Name: "monitoring", Type: "bool", Desc: "digitaloceanconfig: enable monitoring (omit to keep the driver default)"},
			{Name: "ipv6", Type: "bool", Desc: "digitaloceanconfig: enable IPv6 (omit to keep the driver default)"},
			{Name: "private-networking", Type: "bool", Desc: "digitaloceanconfig: enable private networking (omit to keep the driver default)"},
			{Name: "tags", Type: "string", Desc: "digitaloceanconfig: comma-separated droplet `tags`"},
			{Name: "set", Type: "stringSlice", Desc: "driver `field=value`; repeat the flag for more fields. Values such as true and [\"a\"] are read as JSON; numbers and other values are sent as strings (Rancher's schema types numeric driver fields as strings)"},
		},
		Examples: []string{
			"rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --monitoring --dry-run",
			"rancher create machine-config amazonec2config web-pool --set region=us-east-1 --set instanceType=t3a.medium --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// build validates input and assembles the machine-config CR. Pure - no
// ctx, no network. The runtime layer has already checked that both
// arguments are present. Validation order: kind (the runtime layer's
// Enum already guards it once registered; build re-checks defensively),
// --set key=value shape, rejection of top-level Kubernetes object fields
// (apiVersion/kind/metadata/status are object identity, not driver fields -
// refused before any overlay), duplicates, fields that a flag sets and
// kind-scoping, credential-field rejection.
func (createMachineConfigCmd) build(in runtime.Input) (*unstructured.Unstructured, kinds.Kind, error) {
	args := in.Args()
	kindName, name := args[0], args[1]

	k, found := kinds.Lookup(kindName)
	if !found || k.GVR.Group != kinds.MachineConfigGroup {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("argument <kind>=%q is not a machine config kind; one of: %s", kindName, machineConfigKindsHint()))
	}

	// Credential rejection folds case - lowercased key → the
	// canonical casing from the metadata - so no casing of a credential
	// field slips past and persists a secret as a plaintext CR field.
	meta, _ := kinds.MachineConfigFieldsFor(k.Name)
	credential := map[string]string{}
	for _, c := range meta.Credential {
		credential[strings.ToLower(c)] = c
	}
	curatedFieldFlag := map[string]string{}
	for _, cf := range doCuratedFlags {
		curatedFieldFlag[cf.field] = cf.flag
	}

	// --set entries: shape, rejection of top-level Kubernetes object
	// fields, and duplicates first (in that order), values held raw until
	// the collision/credential checks pass.
	type setEntry struct {
		key string
		raw string
	}
	var entries []setEntry
	seen := map[string]bool{}
	for i, entry := range in.Strings("set") {
		// Shape errors identify the occurrence by 1-based index only -
		// never by content: a malformed entry may be a pasted secret, and
		// reflecting it would echo it to stderr and into shell history/CI
		// logs.
		key, raw, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set argument #%d must be <field>=<value>, e.g. --set region=nyc3", i+1))
		}
		if key == "" {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set argument #%d has an empty field name, e.g. --set region=nyc3", i+1))
		}
		if key == "apiVersion" || key == "kind" || key == "metadata" || key == "status" {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set %q names a top-level Kubernetes object field, not a driver field; "+
					"the CLI sets apiVersion, kind and metadata, and the server sets status; pass driver fields only", key))
		}
		if seen[key] {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set field %q is given more than once; pass each field exactly once", key))
		}
		seen[key] = true
		entries = append(entries, setEntry{key: key, raw: raw})
	}

	// Curated flags: kind-scoping by PRESENCE - a set flag on a
	// foreign kind is refused, never ignored or written to a foreign CR.
	fields := map[string]any{}
	for _, cf := range doCuratedFlags {
		if !in.IsSet(cf.flag) {
			continue
		}
		if k.Name != doKind {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s applies to %s, not %s; pass --set %s=<value> to set %s %s driver field",
					cf.flag, doKind, k.Name, cf.field, articleFor(k.Name), k.Name))
		}
		if cf.isBool {
			fields[cf.field] = in.Bool(cf.flag)
		} else {
			fields[cf.field] = in.String(cf.flag)
		}
	}

	for _, e := range entries {
		if k.Name == doKind {
			if flag, collides := curatedFieldFlag[e.key]; collides {
				return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--set %s sets the same field as --%s; pass --%s instead", e.key, flag, flag))
			}
		}
		if canonical, isCredential := credential[strings.ToLower(e.key)]; isCredential {
			msg := fmt.Sprintf("%q holds a credential and cannot be set on a machine config", canonical)
			if e.key != canonical {
				msg = fmt.Sprintf("%q is the credential field %q and cannot be set on a machine config", e.key, canonical)
			}
			msg += "; put credentials in a cloud credential (a cc-* Secret) and pass that cloud credential to create cluster with --cloud-credential"
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, msg)
		}
		fields[e.key] = parseSetValue(e.raw)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kinds.MachineConfigGroup + "/v1",
		"kind":       k.K8sKind,
		"metadata":   map[string]any{"name": name, "namespace": k.DefaultNamespace},
	}}
	maps.Copy(obj.Object, fields)
	return obj, k, nil
}

func (c createMachineConfigCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, k, err := c.build(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, dryRun, exec, err := engine.PlanCreateObserved(ctx, client, k.GVR, k.DefaultNamespace, obj, engine.LocalCluster, k.Name, k.K8sKind, false)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if w := prunedKeyWarning(obj.Object, dryRun.Object); w != "" {
		eff.Warnings = append(eff.Warnings, w)
	}
	if w := fallthroughWarning(k.Name, in); w != "" {
		eff.Warnings = append(eff.Warnings, w)
	}
	return eff, exec, nil
}

// prunedKeyWarning compares the submitted TOP-LEVEL driver keys (minus the
// Kubernetes object fields: apiVersion, kind, metadata, status) against the
// dry-run response's top-level keys and names every key the server
// silently dropped - rke-machine-config CRDs may prune unknown fields, so
// a typo surfaces at preview time rather than as a silently thinner CR.
// Presence, not value, decides: a key submitted with a null
// value that vanishes server-side is pruned like any other. Returns ""
// when nothing was pruned.
func prunedKeyWarning(submitted, dryRun map[string]any) string {
	envelope := map[string]bool{"apiVersion": true, "kind": true, "metadata": true, "status": true}
	var pruned []string
	for key := range submitted {
		if envelope[key] {
			continue
		}
		if _, ok := dryRun[key]; !ok {
			pruned = append(pruned, key)
		}
	}
	if len(pruned) == 0 {
		return ""
	}
	sort.Strings(pruned)
	return fmt.Sprintf("the server removed these fields, possibly because it does not recognize them: %s; check the field names", strings.Join(pruned, ", "))
}

// fallthroughWarning returns a warning when the kind has curated VALUE fields
// (non-bool) that were left unset. Bool fields (monitoring, ipv6,
// privateNetworking) carry tri-state semantics and cannot go out of date
// like value defaults - they are excluded. Returns "" when all value
// fields are set or the kind has none. The unset field names are sorted
// for a stable message.
func fallthroughWarning(kindName string, in runtime.Input) string {
	meta, ok := kinds.MachineConfigFieldsFor(kindName)
	if !ok || len(meta.Curated) == 0 {
		return ""
	}
	fieldToFlag := make(map[string]string, len(doCuratedFlags))
	fieldIsBool := make(map[string]bool, len(doCuratedFlags))
	for _, cf := range doCuratedFlags {
		fieldToFlag[cf.field] = cf.flag
		fieldIsBool[cf.field] = cf.isBool
	}
	var unset []string
	for _, field := range meta.Curated {
		if fieldIsBool[field] {
			continue
		}
		// Skip curated fields that have no entry in the doCuratedFlags map -
		// a future kind with its own Curated set but no doCuratedFlags entries
		// would produce an empty-string flag name, making IsSet meaningless and
		// every such field appear spuriously unset. Presence in the flag map is
		// the gate; TestDoCuratedFlagsMatchKindsCurated asserts that doCuratedFlags
		// and MachineConfigFields.Curated stay in sync for the DO kind.
		flag, known := fieldToFlag[field]
		if !known {
			continue
		}
		if !in.IsSet(flag) {
			unset = append(unset, field)
		}
	}
	if len(unset) == 0 {
		return ""
	}
	sort.Strings(unset)
	return "fields you did not set use the node driver's default values, which may be out of date: " +
		strings.Join(unset, ", ") +
		"; set them yourself (an out-of-date default, such as an image that no longer exists, makes provisioning fail)"
}

// articleFor returns "an" when s starts with a vowel, "a" otherwise.
func articleFor(s string) string {
	if len(s) > 0 && strings.ContainsRune("aeiouAEIOU", rune(s[0])) {
		return "an"
	}
	return "a"
}

var _ runtime.MutatingCommand = (*createMachineConfigCmd)(nil)

// parseSetValue decodes a --set value against Rancher's node-driver
// schema (FlagToField in rancher's
// pkg/controllers/management/drivers/nodedriver/utils.go): a JSON bool,
// array, or null is taken in its decoded form - BoolFlag publishes a
// "boolean" field, StringSliceFlag an "array[string]" (Nullable) field -
// but a JSON NUMBER stays the literal string, because IntFlag fields are
// published as "string" (a backward-compat choice upstream) and a typed
// CRD rejects a JSON number for them. Anything that is not valid JSON is
// the raw string. The decoded forms (bool, string, []any,
// map[string]any, nil) are all JSON-safe for apimachinery's deep copies.
func parseSetValue(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	if _, isNumber := v.(float64); isNumber {
		return raw
	}
	return v
}
