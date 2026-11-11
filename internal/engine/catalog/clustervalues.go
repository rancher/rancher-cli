package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

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

// checkRegistryFlags refuses a registry flag that cannot apply: either flag
// on a chart whose default values declare neither registry path, and
// --system-default-registry where the values files set a parent of a
// registry path to anything but a mapping.
func (p installPlan) checkRegistryFlags(reg registryFlags, info Info) error {
	flag := ""
	switch {
	case reg.host != "":
		flag = flagSystemDefaultRegistry
	case reg.none:
		flag = flagNoSystemDefaultRegistry
	default:
		return nil
	}
	if !declaresRegistry(info.Values) {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"chart %s %s declares neither global.cattle.systemDefaultRegistry nor global.systemDefaultRegistry in its default values; --%s applies only to a chart that declares one; run the command without it",
			clierr.ShellQuote(p.chart), clierr.ShellQuote(p.version), flag))
	}
	if reg.host == "" {
		return nil
	}
	for _, path := range registryPaths {
		if st, parent := p.values.state(path); st == pathBlocked {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"--%s cannot set %s: the values files set %s to a value that is not a mapping",
				flagSystemDefaultRegistry, path, parent))
		}
	}
	return nil
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

// composeValues adds the cluster values and the registry values to the
// files' values. cluster is the management Cluster, nil when the caller
// cannot read it. A source the caller cannot read leaves its values out
// with a warning; a Setting or the project list that is not found counts
// as "".
func (p installPlan) composeValues(ctx context.Context, defaults map[string]any, cluster *unstructured.Unstructured, reg registryFlags) (composition, error) {
	src := clusterSources{id: p.cluster, cluster: cluster}
	var rows []clusterValue
	need := map[valueSource]bool{}
	for _, row := range clusterValues {
		if st, _ := p.values.state(row.path); st == pathAbsent {
			rows = append(rows, row)
			need[row.source] = true
		}
	}
	denied := map[valueSource]bool{sourceCluster: cluster == nil}
	var err error
	if need[sourceServerURL] {
		if src.serverURL, denied[sourceServerURL], err = readSetting(ctx, p.root, settingServerURL); err != nil {
			return composition{}, err
		}
	}
	if need[sourceProjects] {
		if src.systemProject, denied[sourceProjects], err = systemProject(ctx, p.root, p.cluster); err != nil {
			return composition{}, err
		}
	}
	var add []addedValue
	notSent := map[valueSource][]string{}
	for _, row := range rows {
		if denied[row.source] {
			notSent[row.source] = append(notSent[row.source], row.path.String())
			continue
		}
		if v, ok := row.value(src); ok {
			add = append(add, addedValue{path: row.path, value: v})
		}
	}

	var drop []valuePath
	clusterRegistryNotRead, settingDenied := false, false
	switch {
	case reg.none:
		drop = registryPaths
	case reg.host != "":
		add = append(add, registryValues(reg.host)...)
	case declaresRegistry(defaults) && !p.registryBlocked():
		host := ""
		for _, path := range registryPaths {
			if host = p.values.mergedString(defaults, path); host != "" {
				break
			}
		}
		if host == "" && cluster == nil {
			clusterRegistryNotRead = true
		}
		if host == "" && cluster != nil {
			host = clusterRegistry(cluster)
		}
		if host == "" {
			if host, settingDenied, err = readSetting(ctx, p.root, settingSystemDefaultRegistry); err != nil {
				return composition{}, err
			}
		}
		add = append(add, registryValues(host)...)
	}

	sent, added, dropped := p.values.compose(add, drop)
	c := composition{sent: sent, added: added}
	if declaresRegistry(defaults) {
		c.shown = registryPaths
	}
	if len(added) > 0 {
		c.warnings = append(c.warnings, "rancher-cli adds the values the Rancher UI adds when it installs a chart: "+strings.Join(added, ", "))
	}
	typed := clierr.ShellQuote(p.target.typedCluster)
	if paths := notSent[sourceCluster]; len(paths) > 0 || clusterRegistryNotRead {
		w := "cannot read cluster " + typed + "; "
		if len(paths) > 0 {
			w += strings.Join(paths, ", ") + notSentVerb(paths)
			if clusterRegistryNotRead {
				w += ", and "
			}
		}
		if clusterRegistryNotRead {
			w += "the cluster's registry was not read"
		}
		c.warnings = append(c.warnings, w)
	}
	if paths := notSent[sourceServerURL]; len(paths) > 0 {
		c.warnings = append(c.warnings, "cannot read the server-url setting; "+strings.Join(paths, ", ")+notSentVerb(paths))
	}
	if paths := notSent[sourceProjects]; len(paths) > 0 {
		c.warnings = append(c.warnings, "cannot list the projects of cluster "+typed+"; "+strings.Join(paths, ", ")+notSentVerb(paths))
	}
	if settingDenied {
		c.warnings = append(c.warnings, "cannot read the system-default-registry setting; the registry values are sent empty; name a registry with --"+flagSystemDefaultRegistry)
	}
	if len(dropped) > 0 {
		c.warnings = append(c.warnings, "--"+flagNoSystemDefaultRegistry+" leaves out "+strings.Join(dropped, ", ")+" set in the values files")
	}
	return c, nil
}

// notSentVerb ends a warning that lists the paths left out.
func notSentVerb(paths []string) string {
	if len(paths) == 1 {
		return " was not sent"
	}
	return " were not sent"
}

// registryBlocked reports whether the values files set a parent of either
// registry path to anything but a mapping: the CLI then adds neither.
func (p installPlan) registryBlocked() bool {
	for _, path := range registryPaths {
		if st, _ := p.values.state(path); st == pathBlocked {
			return true
		}
	}
	return false
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
