package projects

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// quotaKeys is ResourceQuotaLimit's 13 fixed fields (rancher/rancher
// resource_quota_types.go) — the CRD is structural, so the API server
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

func quotaKeysHint() string {
	keys := make([]string, 0, len(quotaKeys))
	for k := range quotaKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return "allowed keys: " + strings.Join(keys, ", ")
}

// parseQuotaPairs parses a comma-separated k=v list into a quota limit map,
// validating each pair against quotaKeys.
func parseQuotaPairs(s string) (map[string]any, error) {
	limit := map[string]any{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" || v == "" {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("malformed quota pair %q", pair)).
				WithHint("pass key=value pairs, e.g. limitsCpu=2,limitsMemory=4Gi")
		}
		if !quotaKeys[k] {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown quota key %q", k)).
				WithHint(quotaKeysHint())
		}
		limit[k] = v
	}
	return limit, nil
}

// quotaKeySetDiff returns the sorted union of keys present in exactly one of
// a, b — the symmetric difference named in the mismatch error.
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
// (mandatory-together, matching key sets — webhook-verified constraints
// enforced client-side per spec PC-8) and returns the two limit maps, both
// nil if neither flag was given.
func buildQuotaSpecs(in runtime.Input) (limit, nsDefault map[string]any, err error) {
	limitStr := in.String("quota-limit")
	nsDefaultStr := in.String("namespace-default-quota")
	if limitStr == "" && nsDefaultStr == "" {
		return nil, nil, nil
	}
	if limitStr == "" || nsDefaultStr == "" {
		return nil, nil, clierr.New(clierr.CodeValidation,
			"--quota-limit and --namespace-default-quota must be given together with matching keys").
			WithHint("pass both flags with the same set of keys")
	}
	limit, err = parseQuotaPairs(limitStr)
	if err != nil {
		return nil, nil, err
	}
	nsDefault, err = parseQuotaPairs(nsDefaultStr)
	if err != nil {
		return nil, nil, err
	}
	if diff := quotaKeySetDiff(limit, nsDefault); len(diff) > 0 {
		return nil, nil, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("--quota-limit and --namespace-default-quota key sets differ: %s", strings.Join(diff, ", "))).
			WithHint("pass the same set of keys to both flags")
	}
	return limit, nsDefault, nil
}

var gvr = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}

type createCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreate builds `create project <name> --cluster <id>` (mutating). The Project
// is a management.cattle.io/v3 resource in the cluster-id namespace on the
// management cluster (proxy "local"). Uses an explicit metadata.name (not
// generateName) so Plan's dry-run and Execute address the same object.
func NewCreate(clients engine.ClusterClientFactory) runtime.Command {
	return &createCmd{clients: clients}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create project",
		Usage:       "Create a project in a cluster",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true},
			{Name: "description", Type: "string"},
			{Name: "quota-limit", Type: "string"},
			{Name: "namespace-default-quota", Type: "string"},
		},
		Examples:    []string{"rancher create project web --cluster c-x --dry-run", "rancher create project web --cluster c-x --apply"},
		Destructive: "false",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// build validates input and constructs the Project object + namespace.
func (createCmd) build(in runtime.Input) (*unstructured.Unstructured, string, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, "", clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	cluster := in.String("cluster")
	if cluster == "" {
		return nil, "", clierr.New(clierr.CodeValidation, "--cluster is required").WithHint("pass --cluster <id>")
	}
	name := args[0]
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Project",
		"metadata":   map[string]any{"name": name, "namespace": cluster},
		"spec":       map[string]any{"clusterName": cluster, "displayName": name},
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
	return obj, cluster, nil
}

func (c createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	obj, cluster, err := c.build(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// --cluster accepts a display name or id uniformly (spec §3); resolve to
	// the id before it becomes the namespace, spec.clusterName, and Effect
	// target — build validated it is non-empty.
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
	return engine.PlanCreate(ctx, client, gvr, cluster, obj, cluster, "Project")
}
