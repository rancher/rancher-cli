// createmachineconfig.go implements `create machine-config <kind> <name>`
// (spec MC-1..MC-13): guided authoring of the rke-machine-config.cattle.io
// CR that `create cluster --machine-config <kind>/<name>` references.
// Driver fields sit at the CR ROOT alongside apiVersion/kind/metadata —
// machine-config resources have NO spec wrapper (rancher's
// dynamicschema.addConfigSchema publishes driver fields as top-level
// ResourceFields).
package provisioning

import (
	"context"
	"encoding/json"
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

// doKind is the one machine-config kind with curated flags (MC-5/MC-7a).
const doKind = "digitaloceanconfig"

// doCuratedFlag maps one curated CLI flag to its DigitaloceanConfig driver
// field. isBool marks the tri-state booleans (MC-5a): written only when
// the flag is present, so the server/driver default stays authoritative
// when omitted. Strings (including the comma-separated tags list) are
// written verbatim.
type doCuratedFlag struct {
	flag   string
	field  string
	isBool bool
}

// doCuratedFlags — field names source-confirmed against the DigitalOcean
// machine driver (kinds.MachineConfigFieldsFor's file doc names the exact
// upstream files); the Curated set in kinds/machineconfigfields.go pins
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
// name, never generateName (MC-3); fixed fleet-default namespace, no
// --namespace flag (MC-4). Mirrors create cluster's build/Plan split.
func NewCreateMachineConfig(clients engine.ClusterClientFactory) runtime.Command {
	return &createMachineConfigCmd{clients: clients}
}

func (createMachineConfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create machine-config",
		Usage: "Create a machine config for a node-driver cluster pool",
		Desc: "Creates a standalone rke-machine-config.cattle.io CR in fleet-default; " +
			"reference it afterwards with `create cluster --machine-config <kind>/<name>` " +
			"(the two-step node-driver flow). Driver fields sit at the CR root — there is " +
			"no spec wrapper. digitaloceanconfig has curated flags; amazonec2config, " +
			"azureconfig, harvesterconfig, and vmwarevsphereconfig take their fields via " +
			"--set key=value. Credential-carried fields are rejected: credentials belong " +
			"on a cloud credential (cc-* secret), never on a machine config.",
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
			{Name: "set", Type: "stringSlice", Desc: "driver `field=value`, repeatable; true and [\"a\"] parse as JSON, numbers and everything else submit as strings (Rancher publishes numeric driver fields as strings)"},
		},
		Examples: []string{
			"rancher create machine-config digitaloceanconfig web-pool --region nyc3 --size s-2vcpu-4gb --monitoring --dry-run",
			"rancher create machine-config amazonec2config web-pool --set region=us-east-1 --set instanceType=t3a.medium --apply",
		},
		Destructive: "false",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// build validates input and assembles the machine-config CR. Pure — no
// ctx, no network (MC-10). Validation order: kind (the runtime layer's
// Enum already guards it once registered; build re-checks defensively),
// --set key=value shape, Kubernetes envelope-key rejection (MC-6:
// apiVersion/kind/metadata/status are object identity, not driver fields
// — refused before any overlay), duplicates, curated collisions and
// kind-scoping (MC-7/MC-7a), credential-field rejection (MC-8).
func (createMachineConfigCmd) build(in runtime.Input) (*unstructured.Unstructured, kinds.Kind, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, "argument <kind> is required").
			WithHint("one of: " + machineConfigKindsHint())
	}
	if len(args) < 2 || args[1] == "" {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, "argument <name> is required").
			WithHint("provide <name>")
	}
	kindName, name := args[0], args[1]

	k, found := kinds.Lookup(kindName)
	if !found || k.GVR.Group != kinds.MachineConfigGroup {
		return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("argument <kind>=%q is not a machine-config kind", kindName)).
			WithHint("one of: " + machineConfigKindsHint())
	}

	// Credential rejection (MC-8) folds case — lowercased key → the
	// canonical casing from the metadata — so no casing of a credential
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

	// --set entries: shape, envelope-key rejection, and duplicates first
	// (MC-10 order), values held raw until the collision/credential checks
	// pass.
	type setEntry struct {
		key string
		raw string
	}
	var entries []setEntry
	seen := map[string]bool{}
	for i, entry := range in.Strings("set") {
		// Shape errors identify the occurrence by 1-based index only —
		// never by content: a malformed entry may be a pasted secret, and
		// reflecting it would echo it to stderr and into shell history/CI
		// logs.
		key, raw, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set argument #%d must be <field>=<value>", i+1)).
				WithHint("e.g. --set region=nyc3")
		}
		if key == "" {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set argument #%d has an empty field name", i+1)).
				WithHint("e.g. --set region=nyc3")
		}
		if key == "apiVersion" || key == "kind" || key == "metadata" || key == "status" {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set %q is a Kubernetes envelope key, not a driver field", key)).
				WithHint("apiVersion/kind/metadata are set by the CLI and status by the server; pass driver fields only")
		}
		if seen[key] {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set field %q is given more than once", key)).
				WithHint("pass each field exactly once")
		}
		seen[key] = true
		entries = append(entries, setEntry{key: key, raw: raw})
	}

	// Curated flags: kind-scoping (MC-7a) by PRESENCE — a set flag on a
	// foreign kind is refused, never ignored or written to a foreign CR.
	fields := map[string]any{}
	for _, cf := range doCuratedFlags {
		if !in.IsSet(cf.flag) {
			continue
		}
		if k.Name != doKind {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--%s applies to %s, not %s", cf.flag, doKind, k.Name)).
				WithHint(fmt.Sprintf("pass --set %s=<value> to set %s %s driver field", cf.field, articleFor(k.Name), k.Name))
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
					fmt.Sprintf("--set %s collides with the curated --%s flag", e.key, flag)).
					WithHint(fmt.Sprintf("pass --%s instead of --set %s=...", flag, e.key))
			}
		}
		if canonical, isCredential := credential[strings.ToLower(e.key)]; isCredential {
			msg := fmt.Sprintf("%q is a credential field and never belongs on a machine config", canonical)
			if e.key != canonical {
				msg = fmt.Sprintf("%q is the credential field %q and never belongs on a machine config", e.key, canonical)
			}
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation, msg).
				WithHint("credentials live on a cloud credential (cc-* secret); reference it via create cluster --cloud-credential")
		}
		fields[e.key] = parseSetValue(e.raw)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kinds.MachineConfigGroup + "/v1",
		"kind":       k.K8sKind,
		"metadata":   map[string]any{"name": name, "namespace": k.DefaultNamespace},
	}}
	for key, v := range fields {
		obj.Object[key] = v
	}
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
	eff, dryRun, exec, err := engine.PlanCreateObserved(ctx, client, k.GVR, k.DefaultNamespace, obj, engine.LocalCluster, k.K8sKind, false)
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
// Kubernetes envelope: apiVersion, kind, metadata, status) against the
// dry-run response's top-level keys and names every key the server
// silently dropped — rke-machine-config CRDs may prune unknown fields, so
// a typo surfaces at preview time rather than as a silently thinner CR
// (MC-12). Presence, not value, decides: a key submitted with a null
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
	return fmt.Sprintf("field(s) pruned by the server: %s — unknown field?", strings.Join(pruned, ", "))
}

// fallthroughWarning returns a warning when the kind has curated VALUE fields
// (non-bool) that were left unset. Bool fields (monitoring, ipv6,
// privateNetworking) carry tri-state semantics and can't go stale like
// value defaults — they are excluded. Returns "" when all value fields are
// set or the kind has none. The unset field names are sorted for a stable
// message.
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
		// Skip curated fields that have no entry in the doCuratedFlags map —
		// a future kind with its own Curated set but no doCuratedFlags entries
		// would produce an empty-string flag name, making IsSet meaningless and
		// every such field appear spuriously unset. Presence in the flag map is
		// the gate; TestDoCuratedFlagsMatchKindsCurated pins that doCuratedFlags
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
	return "unset field(s) inherit the node driver's declared defaults, which may be stale: " +
		strings.Join(unset, ", ") +
		" — set them explicitly (a stale default, such as a retired image, makes provisioning fail)"
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
// array, or null is taken in its decoded form — BoolFlag publishes a
// "boolean" field, StringSliceFlag an "array[string]" (Nullable) field —
// but a JSON NUMBER stays the literal string, because IntFlag fields are
// published as "string" (a backward-compat choice upstream) and a typed
// CRD rejects a JSON number for them. Anything that is not valid JSON is
// the raw string (MC-6). The decoded forms (bool, string, []any,
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
