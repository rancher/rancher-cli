package extension

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chart(repo, name string) Chart {
	return Chart{Repo: repo, Name: name, Versions: []Version{{Version: "1.0.0"}}}
}

func summary(es []Entry) []string {
	var out []string
	for _, e := range es {
		s := e.Name + " repo="
		if e.Chart != nil {
			s += e.Chart.Repo
		}
		if e.Plugin != nil {
			s += " plugin"
		}
		if e.App != nil {
			s += " app"
		}
		if e.Installed() {
			s += " installed"
		}
		out = append(out, s)
	}
	return out
}

// TestPair asserts the pairing of a UIPlugin with its App and chart by the
// Rancher v2.15 rule: through the App's repository.
func TestPair(t *testing.T) {
	t.Parallel()
	charts := []Chart{chart("partner-extensions", "alpha"), chart("partner-extensions", "beta"), chart("rancher-ui-plugins", "alpha")}
	tests := []struct {
		name    string
		plugins []Plugin
		apps    []App
		want    []string
	}{
		{"nothing installed", nil, nil, []string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo=partner-extensions"}},
		{"plugin and app", []Plugin{{PluginName: "beta"}}, []App{{Name: "beta", Repo: "partner-extensions"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo=partner-extensions plugin app installed"}},
		{"app without a plugin is not installed", nil, []App{{Name: "beta", Repo: "partner-extensions"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo=partner-extensions app"}},
		{"plugin without an app is installed with no repository", []Plugin{{PluginName: "beta"}}, nil,
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo= plugin installed", "beta repo=partner-extensions"}},
		{"two repositories offer the name, the app's repository pairs", []Plugin{{PluginName: "alpha"}}, []App{{Name: "alpha", Repo: "rancher-ui-plugins"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins plugin app installed", "beta repo=partner-extensions"}},
		{"app in a repository that no longer offers the chart", []Plugin{{PluginName: "beta"}}, []App{{Name: "beta", Repo: "gone"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo= plugin app installed", "beta repo=partner-extensions"}},
		{"plugin with no chart at all", []Plugin{{PluginName: "gamma"}}, []App{{Name: "gamma", Repo: "partner-extensions"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo=partner-extensions", "gamma repo= plugin app installed"}},
		{"app with no chart and no plugin is not listed", nil, []App{{Name: "gamma", Repo: "partner-extensions"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo=partner-extensions"}},
		{"app with no repository does not pair with a chart", []Plugin{{PluginName: "beta"}}, []App{{Name: "beta"}},
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo= plugin app installed", "beta repo=partner-extensions"}},
		{"the first of two plugins with one name counts", []Plugin{{Namespace: "a", PluginName: "beta"}, {Namespace: "b", PluginName: "beta"}}, nil,
			[]string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins", "beta repo= plugin installed", "beta repo=partner-extensions"}},
	}
	for _, tt := range tests {
		got := Pair(charts, tt.plugins, tt.apps, false)
		assert.Equal(t, tt.want, summary(got), tt.name)
	}
}

// TestPairByChartName asserts the Rancher v2.14 rule: a UIPlugin pairs with the
// first chart of its name in any repository, repositories in name order,
// whatever repository its App names.
func TestPairByChartName(t *testing.T) {
	t.Parallel()
	charts := []Chart{chart("rancher-ui-plugins", "alpha"), chart("partner-extensions", "alpha")}
	got := Pair(charts, []Plugin{{PluginName: "alpha"}}, []App{{Name: "alpha", Repo: "rancher-ui-plugins"}}, true)
	assert.Equal(t, []string{"alpha repo=partner-extensions plugin app installed", "alpha repo=rancher-ui-plugins"}, summary(got))

	got = Pair(charts, []Plugin{{PluginName: "alpha"}}, nil, true)
	assert.Equal(t, []string{"alpha repo=partner-extensions plugin installed", "alpha repo=rancher-ui-plugins"}, summary(got))

	got = Pair(charts, nil, []App{{Name: "alpha", Repo: "rancher-ui-plugins"}}, true)
	assert.Equal(t, []string{"alpha repo=partner-extensions app", "alpha repo=rancher-ui-plugins"}, summary(got))

	got = Pair(charts, []Plugin{{PluginName: "alpha"}}, []App{{Name: "alpha", Repo: "rancher-ui-plugins"}}, false)
	assert.Equal(t, []string{"alpha repo=partner-extensions", "alpha repo=rancher-ui-plugins plugin app installed"}, summary(got))
}

// TestPairKeepsTheInput asserts an entry has the versions of its chart as
// given.
func TestPairKeepsTheInput(t *testing.T) {
	t.Parallel()
	charts := []Chart{{Repo: "r", Name: "alpha", Versions: []Version{{Version: "1.0.0", AppVersion: "2.0.0"}}}}
	got := Pair(charts, nil, nil, false)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Chart)
	assert.Equal(t, "2.0.0", got[0].Chart.Versions[0].AppVersion)
}
