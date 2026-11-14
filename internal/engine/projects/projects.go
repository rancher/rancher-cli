package projects

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// quotaKeys is ResourceQuotaLimit's 13 fixed fields (rancher/rancher
// resource_quota_types.go) - the CRD is structural, so the API server
// silently PRUNES any other key rather than rejecting it; VALIDATION with
// this list in the hint beats a silent no-op. "extended" (arbitrary resource
// names) is out of scope for this flag.
var quotaKeys = map[string]bool{
	"pods": true, "services": true, "replicationControllers": true,
	"secrets": true, "configMaps": true, "persistentVolumeClaims": true,
	"servicesNodePorts": true, "servicesLoadBalancers": true,
	"requestsCpu": true, "requestsMemory": true, "requestsStorage": true,
	"limitsCpu": true, "limitsMemory": true,
}

func quotaKeysText() string {
	keys := make([]string, 0, len(quotaKeys))
	for k := range quotaKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "allowed keys: " + strings.Join(keys, ", ")
}

// containerLimitKeys is ContainerResourceLimit's four fields (rancher/rancher
// resource_quota_types.go), the type of spec.containerDefaultResourceLimit.
// Like quotaKeys, any other key would be pruned silently by the structural
// CRD, so it is refused instead.
var containerLimitKeys = map[string]bool{
	"requestsCpu": true, "requestsMemory": true,
	"limitsCpu": true, "limitsMemory": true,
}

// ContainerDefaultResourceLimitKeys returns the keys
// --container-default-resource-limit accepts, sorted; shell completion
// offers exactly these.
func ContainerDefaultResourceLimitKeys() []string {
	return slices.Sorted(maps.Keys(containerLimitKeys))
}

// parsePairs is the shared comma-separated k=v parser behind --quota-limit,
// --namespace-default-quota, --container-default-resource-limit and
// --from-literal: split on ",", then split each pair on the first "=".
// allowEmptyValue distinguishes quota pairs (v="" is malformed) from
// literal pairs (v="" is a legitimate blank entry); malformed builds the
// caller-specific error for a pair missing "=" or with an empty key. A key
// given twice is refused, naming flag: only one of its values could be
// kept, and keeping either silently would hide a typo.
func parsePairs(s, flag string, allowEmptyValue bool, malformed func(pair string) error) (map[string]any, error) {
	data := map[string]any{}
	for pair := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" || (!allowEmptyValue && v == "") {
			return nil, malformed(pair)
		}
		if _, seen := data[k]; seen {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("the key %q appears more than once in --%s; give each key once", k, flag))
		}
		data[k] = v
	}
	return data, nil
}

// parseQuotaPairs parses flag's comma-separated k=v list into a quota limit
// map, validating each pair against quotaKeys.
func parseQuotaPairs(s, flag string) (map[string]any, error) {
	limit, err := parsePairs(s, flag, false, func(pair string) error {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("malformed quota pair %q; pass key=value pairs, e.g. limitsCpu=2,limitsMemory=4Gi", pair))
	})
	if err != nil {
		return nil, err
	}
	for k := range limit {
		if !quotaKeys[k] {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("unknown quota key %q; %s", k, quotaKeysText()))
		}
	}
	return limit, nil
}

// parseContainerLimitPairs parses --container-default-resource-limit into
// the ContainerResourceLimit map, validating each key against
// containerLimitKeys. The values are Kubernetes quantities the server
// validates.
func parseContainerLimitPairs(s string) (map[string]any, error) {
	limit, err := parsePairs(s, "container-default-resource-limit", false, func(pair string) error {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("malformed --container-default-resource-limit pair %q; pass key=value pairs, e.g. limitsCpu=500m,limitsMemory=512Mi", pair))
	})
	if err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(limit)) {
		if !containerLimitKeys[k] {
			return nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("unknown --container-default-resource-limit key %q; allowed keys: %s", k, strings.Join(ContainerDefaultResourceLimitKeys(), ", ")))
		}
	}
	return limit, nil
}

// quotaKeySetDiff returns the sorted union of keys present in exactly one of
// a, b - the symmetric difference named in the mismatch error.
func quotaKeySetDiff(a, b map[string]any) []string {
	var diff []string
	for k := range a {
		if _, ok := b[k]; !ok {
			diff = append(diff, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			diff = append(diff, k)
		}
	}
	sort.Strings(diff)
	return diff
}

// buildQuotaSpecs validates and parses --quota-limit/--namespace-default-quota
// (mandatory-together, matching key sets - webhook-verified constraints
// enforced client-side) and returns the two limit maps, both
// nil if neither flag was given.
func buildQuotaSpecs(in runtime.Input) (limit, nsDefault map[string]any, err error) {
	limitStr := in.String("quota-limit")
	nsDefaultStr := in.String("namespace-default-quota")
	if limitStr == "" && nsDefaultStr == "" {
		return nil, nil, nil
	}
	if limitStr == "" || nsDefaultStr == "" {
		return nil, nil, clierr.New(clierr.CodeValidation,
			"--quota-limit and --namespace-default-quota must be given together with matching keys")
	}
	limit, err = parseQuotaPairs(limitStr, "quota-limit")
	if err != nil {
		return nil, nil, err
	}
	nsDefault, err = parseQuotaPairs(nsDefaultStr, "namespace-default-quota")
	if err != nil {
		return nil, nil, err
	}
	if diff := quotaKeySetDiff(limit, nsDefault); len(diff) > 0 {
		return nil, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--quota-limit and --namespace-default-quota key sets differ: %s; pass the same set of keys to both flags", strings.Join(diff, ", ")))
	}
	return limit, nsDefault, nil
}

var gvr = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

type createCmd struct {
	clients engine.ClusterClientFactory
}

// projectGenerateNamePrefix is the metadata.generateName of a project created
// without an id, and the prefix of the preview name p-*.
const projectGenerateNamePrefix = "p-"

// NewCreate builds `create project [<id>] --cluster <id>` (mutating). The
// Project is a management.cattle.io/v3 resource in the cluster-id namespace
// on the management cluster (proxy "local"). With an id the Project gets that
// metadata.name; without one it gets metadata.generateName p- and Rancher
// generates the name. The real create generates a different name from the
// dry-run; the preview shows p-*, and the --apply result shows the real one.
// A create carries no resourceVersion precondition.
func NewCreate(clients engine.ClusterClientFactory) runtime.Command {
	return &createCmd{clients: clients}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create project",
		Usage: "Create a project in a cluster",
		Desc: "With an id, the project's metadata.name is that id, and --display-name defaults to it. " +
			"The id must be a DNS-1123 label (lowercase letters, digits and '-', starting and ending with a " +
			"letter or digit, at most 63 characters), because Rancher names the project's backing namespace " +
			"after it; an id that is not valid is refused before any request. Rancher refuses an id that a " +
			"project in the cluster already has, and the dry-run fails for it too. It also refuses an id whose " +
			"backing namespace <cluster>-<id> already exists, but only the real create checks that; the " +
			"dry-run passes. Without an id the command sends metadata.generateName p- and requires " +
			"--display-name: Rancher generates the id. The real create generates a different id from the " +
			"dry-run, which is why the preview shows p-* in place of the id; the --apply result shows the " +
			"generated id. " +
			"--container-default-resource-limit sets spec.containerDefaultResourceLimit, the default " +
			"CPU and memory requests and limits for each container in the project's namespaces, as " +
			"comma-separated key=value pairs; keys: " + strings.Join(ContainerDefaultResourceLimitKeys(), ", ") +
			"; e.g. limitsCpu=500m,limitsMemory=512Mi. Each key may appear once. " +
			"Before the dry-run the command gets your user ID with a SelfUser request (ext.cattle.io) and sets the " +
			"field.cattle.io/creatorId annotation to it; Rancher then gives you the project's creator default roles " +
			"(Project Owner unless an administrator changed them) shortly after the create. The preview lists the " +
			"annotation. If the command cannot get your user ID, it warns \"creatorId annotation not set\" and " +
			"continues without the annotation, and Rancher then adds no owner binding for you.",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "id", Type: "string", Desc: "project `id` (lowercase letters, digits and '-', at most 63 characters); leave it out and pass --display-name, and Rancher generates an id that starts with p-"}},
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, false),
			{Name: "display-name", Type: "string", Desc: "display `name` of the project (default: the id; required when no id is given)"},
			{Name: "description", Type: "string", Desc: "short `description` for the project"},
			{Name: "quota-limit", Type: "string", Once: true, Desc: "project resource quota limit as `key=value` pairs, e.g. limitsCpu=2,limitsMemory=4Gi"},
			{Name: "namespace-default-quota", Type: "string", Once: true, Desc: "default resource quota for each namespace as `key=value` pairs (same keys as --quota-limit)"},
			{Name: "container-default-resource-limit", Type: "string", Once: true, Desc: "default container resource requests and limits as `key=value` pairs"},
		},
		Examples: []string{
			"rancher create project --cluster c-x --display-name 'Web tier' --dry-run",
			"rancher create project --cluster c-x --display-name 'Web tier' --apply",
			"rancher create project web --cluster c-x --apply",
		},
		Destructive: "false",
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

// build validates input and constructs the Project object + namespace.
func (createCmd) build(in runtime.Input) (*unstructured.Unstructured, string, error) {
	args := in.Args()
	var id string
	if len(args) > 0 {
		id = args[0]
		if id == "" {
			return nil, "", clierr.New(clierr.CodeValidation,
				"the project id is empty; pass an id, or leave it out and pass --display-name")
		}
		// The check is stricter than metadata.name: Rancher builds the
		// backing namespace <cluster>-<lowercased id>, a namespace name
		// must be a DNS-1123 label, and the id is also written as a label
		// value (63 characters at most).
		if validation.IsDNS1123Label(id) != nil {
			return nil, "", clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"project id %s is not valid: use lowercase letters, digits and '-', starting and ending with a letter or digit, at most 63 characters; to let Rancher generate the id, leave it out and pass --display-name",
				clierr.ShellQuote(clierr.IDText(id))))
		}
	}
	displayName := in.String("display-name")
	if len(args) == 0 && displayName == "" {
		return nil, "", clierr.New(clierr.CodeValidation,
			"--display-name is required when no project id is given; pass --display-name <name>, or pass the project id as the argument")
	}
	cluster := in.String("cluster")
	if cluster == "" {
		return nil, "", engine.ClusterRequiredError(engine.ClusterRequiredPlainMessage)
	}
	if displayName == "" {
		displayName = id
	}
	metadata := map[string]any{"namespace": cluster}
	if id != "" {
		metadata["name"] = id
	} else {
		metadata["generateName"] = projectGenerateNamePrefix
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Project",
		"metadata":   metadata,
		"spec":       map[string]any{"clusterName": cluster, "displayName": displayName},
	}}
	if desc := in.String("description"); desc != "" {
		_ = unstructured.SetNestedField(obj.Object, desc, "spec", "description")
	}
	limit, nsDefault, qerr := buildQuotaSpecs(in)
	if qerr != nil {
		return nil, "", qerr
	}
	if limit != nil {
		_ = unstructured.SetNestedMap(obj.Object, limit, "spec", "resourceQuota", "limit")
		_ = unstructured.SetNestedMap(obj.Object, nsDefault, "spec", "namespaceDefaultResourceQuota", "limit")
	}
	if raw := in.String("container-default-resource-limit"); raw != "" {
		containerLimit, err := parseContainerLimitPairs(raw)
		if err != nil {
			return nil, "", err
		}
		_ = unstructured.SetNestedMap(obj.Object, containerLimit, "spec", "containerDefaultResourceLimit")
	}
	return obj, cluster, nil
}

func (c createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, cluster, err := c.build(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// --cluster accepts a display name or id uniformly (spec §3); resolve to
	// the id before it becomes the namespace, spec.clusterName, and Effect
	// target - build validated it is non-empty.
	cluster, err = c.clients.ResolveCluster(ctx, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	obj.SetNamespace(cluster)
	_ = unstructured.SetNestedField(obj.Object, cluster, "spec", "clusterName")
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// The write goes through the management proxy (LocalCluster) because
	// Project is a management.cattle.io resource namespaced by cluster ID,
	// but the Effect targets the downstream cluster the project belongs to.
	projectKind, ok := kinds.Lookup("project")
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeGeneric, "internal error: no kind row for project")
	}
	warning, err := SetCreatorID(ctx, client, obj)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, exec, err := engine.PlanCreate(ctx, client, gvr, cluster, obj, projectKind.Redaction(), cluster, "project", "Project", false)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if obj.GetName() == "" {
		// Without an id the name in the dry-run response is generated for
		// the dry-run only; the real create gets another one. The preview
		// shows p-*, and the --apply result shows the real name.
		eff.Target = gvr.Resource + "/" + projectGenerateNamePrefix + "*"
		eff.DisplayName = projectGenerateNamePrefix + "*"
	}
	if warning != "" {
		eff.Warnings = append(eff.Warnings, warning)
	}
	return eff, exec, nil
}
