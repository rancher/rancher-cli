// createmachineconfig.go implements `create machine-config <kind> <name>`
// (spec MC-1..MC-13): guided authoring of the rke-machine-config.cattle.io
// CR that `create cluster --machine-config <kind>/<name>` references.
// Driver fields sit at the CR ROOT alongside apiVersion/kind/metadata —
// machine-config resources have NO spec wrapper (rancher's
// dynamicschema.addConfigSchema publishes driver fields as top-level
// ResourceFields).
package provisioning

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
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

	meta, _ := kinds.MachineConfigFieldsFor(k.Name)
	credential := map[string]bool{}
	for _, c := range meta.Credential {
		credential[c] = true
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
	for _, entry := range in.Strings("set") {
		key, raw, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--set %q must be <field>=<value>", entry)).
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
				WithHint(fmt.Sprintf("pass --set %s=<value> to set a %s driver field", cf.field, k.Name))
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
		if credential[e.key] {
			return nil, kinds.Kind{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%q is a credential field and never belongs on a machine config", e.key)).
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
