package extension

import (
	"cmp"
	"slices"
)

// Version is one version of an extension chart in a repository index.
type Version struct {
	Version     string
	AppVersion  string
	Deprecated  bool
	Annotations map[string]string
}

// Chart is an extension chart in one repository, its versions in the order
// the server lists them.
type Chart struct {
	Repo     string
	Name     string
	Versions []Version
}

// Plugin is a UIPlugin object.
type Plugin struct {
	Namespace  string
	Name       string
	PluginName string
	Version    string
	CacheState string
	Error      string
}

// App is an App in the extension namespace. Repo is the ClusterRepo the App
// came from: the annotation catalog.cattle.io/ui-source-repo of its chart, else
// its label catalog.cattle.io/cluster-repo-name; empty when it has neither.
type App struct {
	Name string
	Repo string
}

// Entry is one extension as the Rancher UI's Extensions page shows it.
type Entry struct {
	// Name is the extension's name: the chart's, or the UIPlugin's plugin name.
	Name string
	// Chart is the chart that offers the extension; nil when no repository
	// the command reads offers it.
	Chart *Chart
	// Plugin is the UIPlugin of the extension; nil when it is not installed.
	Plugin *Plugin
	// App is the App of the extension; nil when there is none.
	App *App
}

// Installed reports whether the extension is installed: it has a UIPlugin. An
// App without a UIPlugin is not installed.
func (e Entry) Installed() bool { return e.Plugin != nil }

// Pair joins charts, UIPlugins and Apps into the extensions the Rancher UI
// lists, sorted by name and then repository.
//
// A UIPlugin pairs with the App of the same name, and with a chart of that
// name in the App's repository (pairByChartName false, the Rancher UI of
// Rancher v2.15 and later). With pairByChartName true (Rancher v2.14) the UI
// pairs it with the first chart of the name in any repository, taking the
// repositories in name order, and an App pairs the same way. A UIPlugin with no
// chart is an installed extension with no chart; an App with no UIPlugin is
// listed with its chart and does not make the extension installed. When two
// UIPlugins carry one plugin name, the first of them counts.
func Pair(charts []Chart, plugins []Plugin, apps []App, pairByChartName bool) []Entry {
	charts = slices.SortedStableFunc(slices.Values(charts), func(a, b Chart) int {
		return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Name, b.Name))
	})
	appByName := map[string]*App{}
	for i := range apps {
		if _, dup := appByName[apps[i].Name]; !dup {
			appByName[apps[i].Name] = &apps[i]
		}
	}
	pluginByName := map[string]*Plugin{}
	for i := range plugins {
		if _, dup := pluginByName[plugins[i].PluginName]; !dup {
			pluginByName[plugins[i].PluginName] = &plugins[i]
		}
	}

	// find returns the chart a name and an App's repository select.
	find := func(name string, app *App) *Chart {
		for i := range charts {
			if charts[i].Name != name {
				continue
			}
			if pairByChartName || (app != nil && app.Repo != "" && charts[i].Repo == app.Repo) {
				return &charts[i]
			}
		}
		return nil
	}

	names := map[string]bool{}
	for n := range appByName {
		names[n] = true
	}
	for n := range pluginByName {
		names[n] = true
	}
	paired := map[*Chart]bool{}
	var entries []Entry
	for name := range names {
		chart := find(name, appByName[name])
		if pluginByName[name] == nil && chart == nil {
			continue
		}
		if chart != nil {
			paired[chart] = true
		}
		entries = append(entries, Entry{Name: name, Chart: chart, Plugin: pluginByName[name], App: appByName[name]})
	}
	for i := range charts {
		if !paired[&charts[i]] {
			entries = append(entries, Entry{Name: charts[i].Name, Chart: &charts[i]})
		}
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(repoOf(a), repoOf(b)))
	})
	return entries
}

func repoOf(e Entry) string {
	if e.Chart == nil {
		return ""
	}
	return e.Chart.Repo
}
