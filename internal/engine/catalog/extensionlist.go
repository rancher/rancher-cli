package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/catalog/extension"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagShow = "show"

	showAll       = "all"
	showInstalled = "installed"
	showAvailable = "available"

	// hiddenRepoAnnotation marks a ClusterRepo the Rancher UI does not read.
	hiddenRepoAnnotation = "clusterrepo.cattle.io/hidden"
	// stateActive is the steve state of a ClusterRepo the Rancher UI reads.
	stateActive = "active"
	// pairByChartNameBelow is the first Rancher minor whose UI pairs an
	// installed extension with a chart through the repository of its App.
	pairByChartNameBelow = "2.15"
)

// extensionVersion is one version of an extension chart in the JSON of the list.
type extensionVersion struct {
	Version     string            `json:"version"`
	AppVersion  string            `json:"appVersion"`
	Installable bool              `json:"installable"`
	Reason      string            `json:"reason"`
	Checks      []extension.Check `json:"checks"`
}

// extensionItem is one extension: one line of the list.
type extensionItem struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"displayName"`
	Repo        string             `json:"repo"`
	Installed   string             `json:"installed"`
	Installable string             `json:"installable"`
	Upgrade     string             `json:"upgrade"`
	PrimeOnly   bool               `json:"primeOnly"`
	State       string             `json:"state"`
	Versions    []extensionVersion `json:"versions"`
}

// extensionList is the command's payload: the extensions in name and
// repository order. It is never nil: JSON shows an empty list as [].
type extensionList []extensionItem

func (extensionList) Columns() []string {
	return []string{"NAME", "DISPLAY-NAME", "REPO", "INSTALLED", "INSTALLABLE", "UPGRADE", "PRIME-ONLY", "STATE"}
}

func (l extensionList) Rows() [][]string {
	rows := make([][]string, 0, len(l))
	for _, it := range l {
		primeOnly := ""
		if it.PrimeOnly {
			primeOnly = "yes"
		}
		rows = append(rows, []string{it.Name, it.DisplayName, it.Repo, it.Installed, it.Installable, it.Upgrade, primeOnly, it.State})
	}
	return rows
}

type extensionListCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewExtensionList builds `extension list [--show all|installed|available]`: the
// extensions the Rancher UI's Extensions page shows, with what is installed,
// what can be upgraded and why a version does not fit the server.
func NewExtensionList(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &extensionListCmd{clients: clients, roots: roots}
}

func (*extensionListCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "extension list",
		Usage: "List the Rancher UI extensions the repositories offer and the installed ones",
		Desc: "Lists the extensions the Rancher UI's Extensions page shows, one line per extension. " +
			"An extension is a chart of a ClusterRepo that the Rancher UI reads (not hidden, spec.enabled not false, in the state active) " +
			"with a version that has the annotation catalog.cattle.io/ui-component set to plugins, " +
			"and an extension is installed when a UIPlugin of any namespace has its plugin name. " +
			"INSTALLED is the UIPlugin's spec.plugin.version, INSTALLABLE the newest version that fits this server, " +
			"and UPGRADE the version to move to when it differs from the installed one. " +
			"A version fits the server when none of the checks of the Rancher UI fails, in its order: " +
			"kube-version (against the local cluster's Kubernetes version), rancher-version, ui-version, ui-extensions-version " +
			"(against the extensions API version of the Rancher UI of this Rancher release, from a table; a release not in the table is not checked and a warning names the ranges), " +
			"ui-extensions-host (a version that sets it does not install, as in the Rancher UI) and prime-only. " +
			"-o json lists every version with its checks and the reason a check failed. " +
			"A ClusterRepo that is not active, or whose index cannot be read, is left out with a warning that names it; the exit code stays 0. " +
			"--show installed lists the extensions with a UIPlugin and --show available the others. " +
			"The command is refused when the feature uiextension is turned off or the namespace cattle-ui-plugin-system does not exist.",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			{Name: flagShow, Type: "string", Enum: []string{showAll, showInstalled, showAvailable}, Desc: "list `all` extensions, the installed ones or the available ones"},
		},
		Examples: []string{
			"rancher extension list",
			"rancher extension list --show installed -o json",
		},
		Auth:  true,
		Shape: runtime.ShapeTabular,
	}
}

// repoRow is the part of a ClusterRepo the Rancher UI decides on.
type repoRow struct {
	Name    string
	State   string
	Hidden  bool
	Enabled bool
}

// decodeRepoRow decodes one item of the steve ClusterRepo collection.
func decodeRepoRow(path string, index int, raw json.RawMessage) (string, repoRow, error) {
	var item struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
			State       struct {
				Name string `json:"name"`
			} `json:"state"`
		} `json:"metadata"`
		Spec struct {
			Enabled *bool `json:"enabled"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &item); err != nil || item.Metadata.Name == "" {
		return "", repoRow{}, clierr.New(clierr.CodeGeneric, fmt.Sprintf("%s: item %d is not a ClusterRepo", path, index))
	}
	return item.Metadata.Name, repoRow{
		Name:    item.Metadata.Name,
		State:   item.Metadata.State.Name,
		Hidden:  item.Metadata.Annotations[hiddenRepoAnnotation] == "true",
		Enabled: item.Spec.Enabled == nil || *item.Spec.Enabled,
	}, nil
}

func (c *extensionListCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	show := in.String(flagShow)
	if show == "" {
		show = showAll
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	if err := checkExtensionsAvailable(ctx, client); err != nil {
		return runtime.Result{}, err
	}
	root, err := c.roots.Root()
	if err != nil {
		return runtime.Result{}, err
	}
	get := steve.RootGetter(root)

	rancherVersion := ""
	if versions, ok := c.clients.(engine.ServerVersionProvider); ok {
		if raw, err := versions.ServerVersion(ctx); err == nil {
			rancherVersion = raw
		}
	}
	edition := extension.EditionUnknown
	if provider, ok := c.clients.(engine.ServerEditionProvider); ok {
		if got, err := provider.ServerEdition(ctx); err == nil {
			edition = leafEdition(got)
		}
	}
	kubeVersion := ""
	if cluster, err := steve.GetCluster(ctx, get, engine.LocalCluster); err == nil {
		info, _, _ := unstructured.NestedString(cluster.Object, "status", "info", "kubernetesVersion")
		git, _, _ := unstructured.NestedString(cluster.Object, "status", "version", "gitVersion")
		if v, ok := extension.KubernetesVersion(info, git); ok {
			kubeVersion = v
		}
	} else if ctx.Err() != nil {
		return runtime.Result{}, err
	}
	api, apiKnown := extension.ExtensionsAPIVersion(rancherVersion)
	inputs := extension.Inputs{RancherVersion: rancherVersion, KubernetesVersion: kubeVersion, Edition: edition}
	if apiKnown {
		inputs.ExtensionsAPIVersion = api
	}
	below, known := config.VersionBelow(rancherVersion, pairByChartNameBelow)

	repos, err := steve.ReadCollection(ctx, get, steve.Request{Type: ClusterRepoType}, decodeRepoRow)
	if err != nil {
		return runtime.Result{}, err
	}
	slices.SortFunc(repos, func(a, b repoRow) int { return strings.Compare(a.Name, b.Name) })

	var warnings []string
	var charts []extension.Chart
	var existing []extension.ClusterRepo
	for _, repo := range repos {
		if repo.Hidden || !repo.Enabled {
			continue
		}
		if repo.State != stateActive {
			warnings = append(warnings, fmt.Sprintf("skipped ClusterRepo %s: its state is %s", clierr.ShellQuote(repo.Name), stateText(repo.State)))
			continue
		}
		idx, err := ReadIndex(ctx, root, engine.LocalCluster, repo.Name)
		if err != nil {
			if ctx.Err() != nil {
				return runtime.Result{}, err
			}
			if status, ok := clierr.HTTPStatus(err); ok && status == http.StatusServiceUnavailable {
				warnings = append(warnings, fmt.Sprintf("skipped ClusterRepo %s: Rancher is still downloading it", clierr.ShellQuote(repo.Name)))
				continue
			}
			warnings = append(warnings, "skipped "+clierr.From(err).Message)
			continue
		}
		charts = append(charts, extensionCharts(repo.Name, idx)...)
	}

	allRepos, err := client.List(ctx, kind("clusterrepo").GVR, "", "")
	if err == nil {
		for i := range allRepos.Items {
			existing = append(existing, clusterRepoOf(&allRepos.Items[i]))
		}
	}
	plugins, err := listUIPlugins(ctx, client)
	if err != nil {
		return runtime.Result{}, err
	}
	apps, err := listExtensionApps(ctx, client)
	if err != nil {
		return runtime.Result{}, err
	}

	items := extensionItems(extension.Pair(charts, plugins, apps, below && known), inputs)
	list := extensionList{}
	for _, it := range items {
		installed := it.Installed != ""
		if (show == showInstalled && !installed) || (show == showAvailable && installed) {
			continue
		}
		list = append(list, it.extensionItem)
	}
	warnings = append(warnings, extensionWarnings(items, inputs, rancherVersion, apiKnown)...)
	if len(plugins) == 0 {
		if _, added := extension.AlreadyAdded(existing, extension.Repositories()[0].URL); !added {
			if _, added := extension.AlreadyAdded(existing, extension.Repositories()[1].URL); !added {
				warnings = append(warnings, noRepositoryWarning())
			}
		}
	}
	return runtime.Result{Kind: "ExtensionList", Data: list, Warnings: warnings}, nil
}

// listItem is an extension item with what the warnings need.
type listItem struct {
	extensionItem
	versions []extension.Version
	outcomes []extension.Outcome
}

// extensionCharts returns the charts of the index that are extensions the Rancher
// UI lists: an extension chart with no version hidden.
func extensionCharts(repo string, idx Index) []extension.Chart {
	var out []extension.Chart
	for _, name := range sortedKeys(idx.Entries) {
		versions := idx.Entries[name]
		annotations := make([]map[string]string, 0, len(versions))
		for _, v := range versions {
			annotations = append(annotations, v.Annotations)
		}
		if !extension.IsExtensionChart(annotations) || extension.IsHiddenExtension(annotations) {
			continue
		}
		chart := extension.Chart{Repo: repo, Name: name}
		for _, v := range versions {
			chart.Versions = append(chart.Versions, extension.Version{Version: v.Version, AppVersion: v.AppVersion, Deprecated: v.Deprecated, Annotations: v.Annotations})
		}
		out = append(out, chart)
	}
	return out
}

func sortedKeys(m map[string][]ChartVersion) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// extensionItems turns the paired extensions into list items, running the
// Rancher UI's checks on each version of a chart.
func extensionItems(entries []extension.Entry, inputs extension.Inputs) []listItem {
	var out []listItem
	for _, e := range entries {
		it := listItem{Name: e.Name, DisplayName: e.Name, Versions: []extensionVersion{}}
		if e.Chart != nil {
			it.Repo = e.Chart.Repo
			if len(e.Chart.Versions) > 0 {
				if name := e.Chart.Versions[0].Annotations[extension.AnnotationDisplayName]; name != "" {
					it.DisplayName = name
				}
			}
			newest := -1
			for i, v := range e.Chart.Versions {
				outcome := extension.Compatibility(v.Annotations, inputs)
				if outcome.Installable && newest < 0 {
					newest = i
				}
				if v.Annotations[extension.AnnotationPrimeOnly] == "true" {
					it.PrimeOnly = true
				}
				checks := outcome.Checks
				if checks == nil {
					checks = []extension.Check{}
				}
				it.Versions = append(it.Versions, extensionVersion{Version: v.Version, AppVersion: v.AppVersion, Installable: outcome.Installable, Reason: outcome.Reason, Checks: checks})
				it.versions = append(it.versions, v)
				it.outcomes = append(it.outcomes, outcome)
			}
			if newest >= 0 {
				v := e.Chart.Versions[newest]
				it.Installable = extension.VersionLabel(v.Version, v.AppVersion)
				if e.Plugin != nil && e.Plugin.Version != extension.PluginVersion(v.Version, v.AppVersion) {
					it.Upgrade = it.Installable
				}
			}
		}
		switch {
		case e.Plugin != nil:
			it.Installed = e.Plugin.Version
			it.State = e.Plugin.CacheState
			if e.Plugin.Error != "" {
				it.State = "error"
			}
		case e.App != nil:
			it.State = "app without a UIPlugin"
		}
		out = append(out, it)
	}
	return out
}

// extensionWarnings are the warnings that depend on the versions listed: the
// Kubernetes version of the local cluster could not be read while a version
// asks for one, and the extensions API version of the Rancher release is not
// known while a version asks for one.
func extensionWarnings(items []listItem, inputs extension.Inputs, rancherVersion string, apiKnown bool) []string {
	var warnings []string
	kubeAsked := false
	var ranges []string
	for _, it := range items {
		for _, outcome := range it.outcomes {
			for _, c := range outcome.Checks {
				if c.Check == extension.CheckKubeVersion {
					kubeAsked = true
				}
				if c.Check == extension.CheckExtensionsVersion && c.Result == extension.ResultNotChecked && !slices.Contains(ranges, c.Range) {
					ranges = append(ranges, c.Range)
				}
			}
		}
	}
	if kubeAsked && inputs.KubernetesVersion == "" {
		warnings = append(warnings, "the Kubernetes version of the local cluster could not be read; versions that set kube-version are not installable")
	}
	if len(ranges) > 0 && !apiKnown {
		slices.Sort(ranges)
		warnings = append(warnings, fmt.Sprintf("the extensions API version of Rancher %s is not known; ui-extensions-version %s was not checked; "+
			"if the browser loads another UI build (ui-offline-preferred, ui-dashboard-index) the check may differ",
			rancherVersion, strings.Join(ranges, ", ")))
	}
	return warnings
}

// noRepositoryWarning says that no Rancher extension repository is added and
// nothing is installed.
func noRepositoryWarning() string {
	return clierr.Text("no Rancher extension repository is added; the repositories are partner-extensions and, on Rancher Prime, rancher-ui-plugins",
		clierr.Lead("; add one with: "), clierr.NewCommand("extension add-repository").Placeholder("<name>").Flag("--dry-run"))
}

func stateText(state string) string {
	if state == "" {
		return "unknown"
	}
	return state
}

// leafEdition maps the engine's edition to the extension package's.
func leafEdition(e engine.Edition) extension.Edition {
	switch e {
	case engine.EditionPrime:
		return extension.EditionPrime
	case engine.EditionCommunity:
		return extension.EditionCommunity
	}
	return extension.EditionUnknown
}

// listUIPlugins lists the UIPlugins of every namespace.
func listUIPlugins(ctx context.Context, client engine.ClusterClient) ([]extension.Plugin, error) {
	list, err := client.List(ctx, kind("uiplugin").GVR, "", "")
	if err != nil {
		return nil, err
	}
	plugins := make([]extension.Plugin, 0, len(list.Items))
	for _, obj := range list.Items {
		name, _, _ := unstructured.NestedString(obj.Object, "spec", "plugin", "name")
		version, _, _ := unstructured.NestedString(obj.Object, "spec", "plugin", "version")
		cacheState, _, _ := unstructured.NestedString(obj.Object, "status", "cacheState")
		errText, _, _ := unstructured.NestedString(obj.Object, "status", "error")
		plugins = append(plugins, extension.Plugin{Namespace: obj.GetNamespace(), Name: obj.GetName(), PluginName: name, Version: version, CacheState: cacheState, Error: errText})
	}
	return plugins, nil
}

// listExtensionApps lists the Apps in the extension namespace with the
// ClusterRepo each came from: the annotation catalog.cattle.io/ui-source-repo
// of its chart, else its label catalog.cattle.io/cluster-repo-name.
func listExtensionApps(ctx context.Context, client engine.ClusterClient) ([]extension.App, error) {
	list, err := client.List(ctx, kind("app").GVR, extension.Namespace, "")
	if err != nil {
		return nil, err
	}
	apps := make([]extension.App, 0, len(list.Items))
	for _, obj := range list.Items {
		annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "chart", "metadata", "annotations")
		repo := annotations[extension.AnnotationSourceRepo]
		if repo == "" {
			repo = obj.GetLabels()[extension.AnnotationClusterRepoName]
		}
		apps = append(apps, extension.App{Name: obj.GetName(), Repo: repo})
	}
	return apps, nil
}
