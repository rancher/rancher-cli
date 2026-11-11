package catalog

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	flagRepo              = "repo"
	flagAllVersions       = "all-versions"
	flagIncludeDeprecated = "include-deprecated"

	// hiddenAnnotation marks a chart the Rancher UI does not list: present
	// and not empty, whatever the value.
	hiddenAnnotation = "catalog.cattle.io/hidden"
)

// IsHidden reports whether the Rancher UI does not list v: its annotation
// catalog.cattle.io/hidden is present and not empty, whatever the value.
func IsHidden(v ChartVersion) bool { return v.Annotations[hiddenAnnotation] != "" }

// chartItem is one line of the list.
type chartItem struct {
	Repository  string `json:"repository"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	AppVersion  string `json:"appVersion"`
	Description string `json:"description"`
	Hidden      bool   `json:"hidden"`
	Deprecated  bool   `json:"deprecated"`
}

// chartList is the command's payload: the lines in repository, chart and
// version order. It is never nil: JSON shows an empty list as [].
type chartList []chartItem

func (chartList) Columns() []string {
	return []string{"REPOSITORY", "NAME", "VERSION", "APP VERSION", "DESCRIPTION"}
}

func (l chartList) Rows() [][]string {
	rows := make([][]string, 0, len(l))
	for _, it := range l {
		rows = append(rows, []string{it.Repository, it.Name, it.Version, it.AppVersion, it.Description})
	}
	return rows
}

// deprecatedChartList is chartList with a DEPRECATED column, the payload of a
// run with --include-deprecated.
type deprecatedChartList chartList

func (deprecatedChartList) Columns() []string {
	return append(chartList(nil).Columns(), "DEPRECATED")
}

func (l deprecatedChartList) Rows() [][]string {
	rows := chartList(l).Rows()
	for i, it := range l {
		rows[i] = append(rows[i], strconv.FormatBool(it.Deprecated))
	}
	return rows
}

type chartsCmd struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
}

// NewCharts builds `charts --cluster <c> [--repo <name>] [--all-versions]
// [--include-deprecated]`: the charts and versions the ClusterRepos of one
// cluster offer.
func NewCharts(clients engine.ClusterClientFactory, roots engine.RootClientFactory) runtime.Command {
	return &chartsCmd{clients: clients, roots: roots}
}

func (*chartsCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "charts",
		Usage: "List the charts and versions the repositories of a cluster offer",
		Desc: "Lists the charts of the ClusterRepos of --cluster, one line per chart. " +
			"The list comes from the Helm repository index Rancher serves for each ClusterRepo " +
			"(\"index\" below) and holds the versions Rancher lists for the cluster's Kubernetes and Rancher versions. " +
			"A line shows the newest version that is not a prerelease, which is the first such version " +
			"Rancher serves (it serves each chart's versions newest first), " +
			"or the first version when the chart has only prereleases. " +
			"--all-versions lists every version of each chart instead, in the order Rancher serves them. " +
			"A deprecated version (the index entry's deprecated field is true, " +
			"or its annotation catalog.cattle.io/deprecated is \"true\") is left out, and so is a chart with only deprecated versions. " +
			"--include-deprecated lists them, with a DEPRECATED column and deprecated in JSON; " +
			"it only widens the list and is not the gate flag --allow-deprecated of the commands that change things. " +
			"--repo reads one ClusterRepo, and its failure is the command's error. " +
			"--repo naming a disabled ClusterRepo (spec.enabled is false) is refused with VALIDATION. " +
			"Without --repo every ClusterRepo whose spec.enabled is not false is read, one after another in name order. " +
			"A disabled ClusterRepo (spec.enabled is false) is skipped, " +
			"and one whose index cannot be read is left out with a warning that names it; the exit code stays 0. " +
			"Charts the Rancher UI does not list (the annotation catalog.cattle.io/hidden is present and not empty) " +
			"are listed, with hidden true in JSON; the table has no column for it. " +
			"-o name prints the NAME column; a chart name repeats across repositories and, with --all-versions, across versions.",
		CommandType: runtime.ReadOnly,
		Flags: []runtime.ArgSpec{
			engine.ClusterFlag(true, false),
			{Name: flagRepo, Type: "string", Desc: "list only the ClusterRepo `name`"},
			{Name: flagAllVersions, Type: "bool", Desc: "list every version of each chart, not only the newest"},
			{Name: flagIncludeDeprecated, Type: "bool", Desc: "also list deprecated versions"},
		},
		Examples: []string{
			"rancher charts --cluster prod",
			"rancher charts --cluster prod --repo rancher-charts --all-versions -o json",
		},
		Auth:  true,
		Shape: runtime.ShapeTabular,
	}
}

func (c *chartsCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	clusterID, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, false)
	if err != nil {
		return runtime.Result{}, err
	}
	root, err := c.roots.Root()
	if err != nil {
		return runtime.Result{}, err
	}
	all := in.Bool(flagAllVersions)
	deprecated := in.Bool(flagIncludeDeprecated)

	if repo := in.String(flagRepo); repo != "" {
		client, err := c.clients.Cluster(ctx, clusterID)
		if err != nil {
			return runtime.Result{}, err
		}
		if _, err := readClusterRepo(ctx, client, repo, in.String(engine.FlagCluster)); err != nil {
			return runtime.Result{}, err
		}
		idx, err := ReadIndex(ctx, root, clusterID, repo)
		if err != nil {
			return runtime.Result{}, withRepoRemedy(err, in.String(engine.FlagCluster))
		}
		return runtime.Result{Kind: "ChartList", Data: payload(itemsOf(repo, idx, all, deprecated), deprecated)}, nil
	}

	repos, err := enabledRepos(ctx, c.clients, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	items := chartList{}
	var warnings []string
	for _, repo := range repos {
		idx, err := ReadIndex(ctx, root, clusterID, repo)
		if err != nil {
			if ctx.Err() != nil {
				return runtime.Result{}, err
			}
			warnings = append(warnings, "skipped "+clierr.From(err).Message)
			continue
		}
		items = append(items, itemsOf(repo, idx, all, deprecated)...)
	}
	return runtime.Result{Kind: "ChartList", Data: payload(items, deprecated), Warnings: warnings}, nil
}

// payload is the list with a DEPRECATED column when deprecated versions are
// included.
func payload(items chartList, includeDeprecated bool) any {
	if includeDeprecated {
		return deprecatedChartList(items)
	}
	return items
}

// withRepoRemedy gives a NOT_FOUND answer the command that lists the
// ClusterRepos of the cluster the user typed.
func withRepoRemedy(err error, typedCluster string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeNotFound {
		return err
	}
	return ce.WithHint(clierr.Rancher + " get clusterrepos --cluster " + clierr.ShellQuote(typedCluster))
}

// enabledRepos lists the names of the cluster's ClusterRepos whose
// spec.enabled is not false, in name order. A missing spec.enabled counts as
// enabled.
func enabledRepos(ctx context.Context, clients engine.ClusterClientFactory, clusterID string) ([]string, error) {
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	k, _ := kinds.ByKey("clusterrepo")
	list, err := client.List(ctx, k.GVR, "", "")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, obj := range list.Items {
		if enabled, found, _ := unstructured.NestedBool(obj.Object, "spec", "enabled"); found && !enabled {
			continue
		}
		names = append(names, obj.GetName())
	}
	slices.Sort(names)
	return names, nil
}

// itemsOf turns an index into list lines: charts in name order and, per
// chart, its default version or, with all, every version in the order Rancher
// serves them. Deprecated versions are left out unless includeDeprecated, before
// the default version is picked; a chart left with no version has no line.
func itemsOf(repo string, idx Index, all, includeDeprecated bool) chartList {
	items := chartList{}
	for _, name := range slices.Sorted(maps.Keys(idx.Entries)) {
		versions := idx.Entries[name]
		if !includeDeprecated {
			versions = WithoutDeprecated(versions)
		}
		if !all {
			v, ok := DefaultVersion(versions)
			if !ok {
				continue
			}
			versions = []ChartVersion{v}
		}
		for _, v := range versions {
			items = append(items, chartItem{
				Repository:  repo,
				Name:        name,
				Version:     v.Version,
				AppVersion:  v.AppVersion,
				Description: v.Description,
				Hidden:      IsHidden(v),
				Deprecated:  IsDeprecated(v),
			})
		}
	}
	return items
}
