package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The registry flags of app install.
const (
	flagSystemDefaultRegistry   = "system-default-registry"
	flagNoSystemDefaultRegistry = "no-system-default-registry"
)

// The Settings app install reads, through steve on the Rancher server.
const (
	settingType                  = "management.cattle.io.settings"
	settingServerURL             = "server-url"
	settingSystemDefaultRegistry = "system-default-registry"
)

// valueSource is the server read a cluster value comes from.
type valueSource int

const (
	sourceNone valueSource = iota
	sourceCluster
	sourceServerURL
	sourceProjects
)

// clusterSources holds what the reads of one install found: the cluster
// id, the management Cluster (nil when the caller cannot read it), the
// server-url Setting and the System project's name.
type clusterSources struct {
	id            string
	cluster       *unstructured.Unstructured
	serverURL     string
	systemProject string
}

// clusterValue is one value the Rancher UI adds to a chart's values when it
// installs the chart: its path, the read it comes from, and the value, with
// false when nothing is sent.
type clusterValue struct {
	path   valuePath
	source valueSource
	value  func(clusterSources) (any, bool)
}

// clusterValues are the values app install adds to every chart, each where
// the values files hold no value on its path. The composition reads this
// list and nothing else.
var clusterValues = []clusterValue{
	{valuePath{"global", "cattle", "clusterId"}, sourceNone, func(s clusterSources) (any, bool) { return s.id, true }},
	{valuePath{"global", "cattle", "clusterName"}, sourceCluster, func(s clusterSources) (any, bool) {
		if name, _, _ := unstructured.NestedString(s.cluster.Object, "spec", "displayName"); name != "" {
			return name, true
		}
		return s.cluster.GetName(), true
	}},
	{valuePath{"global", "cattle", "systemProjectId"}, sourceProjects, func(s clusterSources) (any, bool) { return s.systemProject, true }},
	{valuePath{"global", "cattle", "url"}, sourceServerURL, func(s clusterSources) (any, bool) { return s.serverURL, true }},
	{valuePath{"global", "cattle", "rkePathPrefix"}, sourceCluster, func(s clusterSources) (any, bool) {
		v, _, _ := unstructured.NestedString(s.cluster.Object, "spec", "rancherKubernetesEngineConfig", "prefixPath")
		return v, true
	}},
	{valuePath{"global", "cattle", "rkeWindowsPathPrefix"}, sourceCluster, func(s clusterSources) (any, bool) {
		v, _, _ := unstructured.NestedString(s.cluster.Object, "spec", "rancherKubernetesEngineConfig", "winPrefixPath")
		return v, true
	}},
	{valuePath{"global", "cattle", "windows", "enabled"}, sourceCluster, func(s clusterSources) (any, bool) {
		if slices.Contains(workerOSes(s.cluster), "windows") {
			return true, true
		}
		return nil, false
	}},
}

// registryPaths are the two values that name the registry, set together to
// one value or not at all. The values files' value at the first wins over
// the second.
var registryPaths = []valuePath{
	{"global", "cattle", "systemDefaultRegistry"},
	{"global", "systemDefaultRegistry"},
}

// declaresRegistry reports whether a chart's default values declare either
// registry path, with any value.
func declaresRegistry(defaults map[string]any) bool {
	for _, p := range registryPaths {
		if _, st, _ := lookup(defaults, p); st == pathPresent {
			return true
		}
	}
	return false
}

// registryFlags are the registry flags as typed.
type registryFlags struct {
	host string
	none bool
}

// composition is what the composition of an install's values produced: the
// values sent, the leaves the CLI set, the paths whose string value the
// preview shows (the registry paths of a chart that declares one), and the
// warnings, in order.
type composition struct {
	sent     Values
	added    []string
	shown    []valuePath
	warnings []string
}

// notSentVerb ends a warning that lists the paths left out.
func notSentVerb(paths []string) string {
	if len(paths) == 1 {
		return " was not sent"
	}
	return " were not sent"
}

// registryValues sets both registry paths to host, over the files' values.
func registryValues(host string) []addedValue {
	out := make([]addedValue, 0, len(registryPaths))
	for _, path := range registryPaths {
		out = append(out, addedValue{path: path, value: host, replace: true})
	}
	return out
}

// clusterRegistry is the registry a management Cluster names:
// spec.importedConfig.privateRegistryURL, else
// spec.clusterSecrets.privateRegistryURL, which Rancher sets for an RKE2 or
// K3s cluster it provisions.
func clusterRegistry(cluster *unstructured.Unstructured) string {
	if v, _, _ := unstructured.NestedString(cluster.Object, "spec", "importedConfig", "privateRegistryURL"); v != "" {
		return v
	}
	v, _, _ := unstructured.NestedString(cluster.Object, "spec", "clusterSecrets", "privateRegistryURL")
	return v
}

// readSetting reads a Setting through steve on the Rancher server, never
// through a cluster's Kubernetes API: its value, else its default. A 404 is
// "", and denied is true for a 403; any other failure is the error.
func readSetting(ctx context.Context, root engine.RootClient, name string) (value string, denied bool, err error) {
	r := steve.Request{Type: settingType, Name: name}
	body, err := root.GetJSON(ctx, r.Path())
	switch {
	case err != nil && forbidden(err):
		return "", true, nil
	case err != nil && notFound(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &obj.Object); err != nil || obj.Object == nil {
		return "", false, clierr.New(clierr.CodeGeneric, "GET "+r.Path()+" on the Rancher server did not answer a JSON object")
	}
	v, _ := engine.SettingValue(obj)
	return v, false, nil
}

// systemProject is the name of the cluster's System project among the
// projects steve lists for the caller, or "" when none is listed; with two,
// the lowest name. A 404 is "", and denied is true for a 403; any other
// failure is the error.
func systemProject(ctx context.Context, root engine.RootClient, cluster string) (name string, denied bool, err error) {
	list, err := steve.Projects(ctx, steve.RootGetter(root), cluster)
	if err != nil {
		st, ok := clierr.HTTPStatus(err)
		switch {
		case ok && st == http.StatusForbidden:
			return "", true, nil
		case ok && st == http.StatusNotFound:
			return "", false, nil
		}
		return "", false, err
	}
	var names []string
	for _, pr := range list {
		if pr.System {
			names = append(names, pr.ID)
		}
	}
	if len(names) == 0 {
		return "", false, nil
	}
	return slices.Min(names), false, nil
}
