package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	repoObjectPath = "/k8s/clusters/c-x/apis/catalog.cattle.io/v1/clusterrepos/rancher-charts"
	repoIndexPath  = "/k8s/clusters/c-x/v1/catalog.cattle.io.clusterrepos/rancher-charts"
	enabledRepo    = `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":true}}`
)

// chartIndex has one chart per rule the --chart source applies and one chart
// with versions of every kind for the --version source.
const chartIndex = `{"entries":{
  "plain":      [{"version":"1.0.0"}],
  "hidden":     [{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":"true"}}],
  "empty-hidden": [{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":""}}],
  "auto":       [{"version":"1.0.0","annotations":{"catalog.cattle.io/auto-install":"crd=1.0.0"}}],
  "auto-gvr":   [{"version":"1.0.0","annotations":{"catalog.cattle.io/auto-install-gvr":"foo.bar"}}],
  "empty-auto": [{"version":"1.0.0","annotations":{"catalog.cattle.io/auto-install":" , "}}],
  "fixed-ns":   [{"version":"1.0.0","annotations":{"catalog.cattle.io/namespace":"cattle-x"}}],
  "fixed-rel":  [{"version":"1.0.0","annotations":{"catalog.cattle.io/release-name":"the-release"}}],
  "all-deprecated": [{"version":"1.0.0","deprecated":true},{"version":"0.9.0","annotations":{"catalog.cattle.io/deprecated":"true"}}],
  "one-hidden-version": [{"version":"2.0.0"},{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":"true"}}],
  "mixed": [{"version":"3.0.0-rc.1"},{"version":"2.0.0","deprecated":true},{"version":"1.1.0"},{"version":"1.0.0","annotations":{"catalog.cattle.io/deprecated":"true"}},{"version":"0.9.0","annotations":{"catalog.cattle.io/deprecated":"false"}}]
}}`

// InstallCommand is the registered name of app install, the command the
// chart sources answer for.
const InstallCommand = "app install"

func chartServer(t *testing.T, repoObject string) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{repoObjectPath: repoObject, repoIndexPath: chartIndex})
}

func installRequest(flag string, tail ...string) completion.Request {
	return completion.Request{Command: InstallCommand, Flag: flag, ClusterID: "c-x", Tail: tail}
}

// TestChartAndVersionSourcesClaim asserts each source claims only its flag of
// app install, as two reads of the typed cluster, and declines every other
// site and every other command.
func TestChartAndVersionSourcesClaim(t *testing.T) {
	t.Parallel()
	want := completion.Requirements{Cluster: completion.ClusterResolved, Credential: completion.CredentialAny, Allowance: completion.AllowanceTwo}
	for flag, src := range map[string]completion.Source{"chart": ChartSource(InstallCommand), "version": VersionSource(InstallCommand)} {
		reqs, ok := src.Claim(completion.Request{Command: InstallCommand, Flag: flag})
		require.True(t, ok, flag)
		assert.Equal(t, want, reqs, flag)
		for _, other := range []string{"", "cluster", "repo", "namespace", "values", "chart-x"} {
			if other == flag {
				continue
			}
			_, ok := src.Claim(completion.Request{Command: InstallCommand, Flag: other})
			assert.False(t, ok, "%s source, flag %q", flag, other)
		}
		for _, command := range []string{"", "charts", "app uninstall", "install"} {
			_, ok := src.Claim(completion.Request{Command: command, Flag: flag})
			assert.False(t, ok, "%s source, command %q", flag, command)
		}
	}
}

// TestInstallSourceRoutesByFlag asserts the combined source answers --repo,
// --chart and --version of app install through the matching source and
// declines every other site.
func TestInstallSourceRoutesByFlag(t *testing.T) {
	t.Parallel()
	src := InstallSource(InstallCommand)
	for _, flag := range []string{"repo", "chart", "version"} {
		_, ok := src.Claim(completion.Request{Command: InstallCommand, Flag: flag})
		assert.True(t, ok, flag)
	}
	for _, flag := range []string{"", "cluster", "namespace", "values", "helm-timeout", "description", "system-default-registry"} {
		_, ok := src.Claim(completion.Request{Command: InstallCommand, Flag: flag})
		assert.False(t, ok, "flag %q", flag)
	}
	ls := chartServer(t, enabledRepo)
	got := src.Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts"))
	assert.Contains(t, got.Names, "plain")
	assert.Empty(t, src.Complete(t.Context(), ls.Conn(2), installRequest("namespace", "--repo", "rancher-charts")).Names)
}

// TestChartSourceOffersTheChartsTheIndexAllows asserts a hidden chart, a chart
// with an auto-install or auto-install-gvr annotation, a chart with only
// deprecated versions and a chart with one version that would be refused are
// not offered, and an empty hidden or auto-install annotation counts as none.
func TestChartSourceOffersTheChartsTheIndexAllows(t *testing.T) {
	t.Parallel()
	ls := chartServer(t, enabledRepo)
	got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts"))
	assert.ElementsMatch(t, []string{"plain", "empty-hidden", "empty-auto", "fixed-ns", "fixed-rel", "mixed"}, got.Names)
	assert.Equal(t, []string{repoObjectPath, repoIndexPath + "?link=index"}, ls.Requested())
}

// TestChartSourceChecksTheTypedNamespaceAndRelease asserts a fixed
// catalog.cattle.io/namespace that differs from the typed --namespace, or a
// fixed release-name that differs from the typed <release>, is not offered,
// one that matches is, and a word not typed yet filters nothing.
func TestChartSourceChecksTheTypedNamespaceAndRelease(t *testing.T) {
	t.Parallel()
	base := []string{"plain", "empty-hidden", "empty-auto", "mixed"}
	tests := []struct {
		name string
		req  completion.Request
		want []string
	}{
		{"nothing typed", installRequest("chart", "--repo", "rancher-charts"), append([]string{"fixed-ns", "fixed-rel"}, base...)},
		{"other namespace", installRequest("chart", "--repo", "rancher-charts", "--namespace", "web"), append([]string{"fixed-rel"}, base...)},
		{"the fixed namespace", installRequest("chart", "--repo", "rancher-charts", "--namespace=cattle-x"), append([]string{"fixed-ns", "fixed-rel"}, base...)},
		{"other release", withPositionals(installRequest("chart", "--repo", "rancher-charts"), "my-app"), append([]string{"fixed-ns"}, base...)},
		{"the fixed release", withPositionals(installRequest("chart", "--repo", "rancher-charts"), "the-release"), append([]string{"fixed-ns", "fixed-rel"}, base...)},
		{"both differ", withPositionals(installRequest("chart", "--repo", "rancher-charts", "--namespace", "web"), "my-app"), base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := chartServer(t, enabledRepo)
			got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), tt.req)
			assert.ElementsMatch(t, tt.want, got.Names)
		})
	}
}

func withPositionals(req completion.Request, positionals ...string) completion.Request {
	req.Positionals = positionals
	return req
}

// TestChartSourceChecksTheTypedVersion asserts a typed --version decides the
// chart on that exact version: a version that is deprecated is checked as any
// other, and a chart without it is not offered.
func TestChartSourceChecksTheTypedVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    []string
	}{
		{"1.0.0", []string{"plain", "empty-hidden", "empty-auto", "fixed-ns", "fixed-rel", "all-deprecated", "mixed"}},
		{"0.9.0", []string{"all-deprecated", "mixed"}},
		{"2.0.0", []string{"mixed", "one-hidden-version"}},
		{"9.9.9", nil},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			ls := chartServer(t, enabledRepo)
			got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts", "--version", tt.version))
			assert.ElementsMatch(t, tt.want, got.Names)
		})
	}
}

// TestChartSourceOffersNothingWithoutWhatItNeeds asserts no names and no
// index read without --repo or a cluster, none for a disabled repository
// (after one read of the ClusterRepo), and none when a read fails.
func TestChartSourceOffersNothingWithoutWhatItNeeds(t *testing.T) {
	t.Parallel()
	t.Run("no repo", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, enabledRepo)
		got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart"))
		assert.Empty(t, got.Names)
		assert.Empty(t, ls.Requested())
	})
	t.Run("no cluster", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, enabledRepo)
		req := installRequest("chart", "--repo", "rancher-charts")
		req.ClusterID = ""
		assert.Empty(t, ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), req).Names)
		assert.Empty(t, ls.Requested())
	})
	t.Run("a repo that is no path segment", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, enabledRepo)
		assert.Empty(t, ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "a/b")).Names)
		assert.Empty(t, ls.Requested())
	})
	for _, repo := range []string{"A", "a_b", "a."} {
		t.Run("a repo that is no ClusterRepo name "+repo, func(t *testing.T) {
			t.Parallel()
			for name, src := range map[string]completion.Source{"chart": ChartSource(InstallCommand), "version": VersionSource(InstallCommand)} {
				ls := chartServer(t, enabledRepo)
				got := src.Complete(t.Context(), ls.Conn(2), installRequest(name, "--repo", repo, "--chart", "plain"))
				assert.Empty(t, got.Names, name)
				assert.Empty(t, ls.Requested(), name)
			}
		})
	}
	t.Run("disabled repository", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, `{"metadata":{"name":"rancher-charts"},"spec":{"enabled":false}}`)
		got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts"))
		assert.Empty(t, got.Names)
		assert.Equal(t, []string{repoObjectPath}, ls.Requested())
	})
	t.Run("repository without spec.enabled", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, `{"metadata":{"name":"rancher-charts"}}`)
		got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts"))
		assert.NotEmpty(t, got.Names)
	})
	t.Run("unknown repository", func(t *testing.T) {
		t.Parallel()
		ls := chartServer(t, enabledRepo)
		got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "missing"))
		assert.Empty(t, got.Names)
	})
	t.Run("index read fails", func(t *testing.T) {
		t.Parallel()
		ls := completiontest.New(t, map[string]string{repoObjectPath: enabledRepo})
		got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("chart", "--repo", "rancher-charts"))
		assert.Empty(t, got.Names)
	})
}

// TestChartSourceReadsTheIndexOfTheLocalClusterAtTheRoot asserts the index of
// the local cluster is read at /v1/..., as the command reads it.
func TestChartSourceReadsTheIndexOfTheLocalClusterAtTheRoot(t *testing.T) {
	t.Parallel()
	ls := completiontest.New(t, map[string]string{
		"/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos/rancher-charts": enabledRepo,
		"/v1/catalog.cattle.io.clusterrepos/rancher-charts":                         chartIndex,
	})
	req := installRequest("chart", "--repo", "rancher-charts")
	req.ClusterID = "local"
	got := ChartSource(InstallCommand).Complete(t.Context(), ls.Conn(2), req)
	assert.NotEmpty(t, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/catalog.cattle.io/v1/clusterrepos/rancher-charts", "/v1/catalog.cattle.io.clusterrepos/rancher-charts?link=index"}, ls.Requested())
}

// TestChartSourceStaysWithinItsAllowance asserts one TAB sends two requests at
// most, whatever the index holds.
func TestChartSourceStaysWithinItsAllowance(t *testing.T) {
	t.Parallel()
	ls := chartServer(t, enabledRepo)
	conn := ls.Conn(2)
	ChartSource(InstallCommand).Complete(t.Context(), conn, installRequest("chart", "--repo", "rancher-charts"))
	assert.Equal(t, 2, conn.Used())
	assert.False(t, conn.Exceeded())
}

// TestVersionSourceKeepsTheServedOrder asserts the versions come in the order
// Rancher serves them, a prerelease stays in its place, and a deprecated
// version (the field or the annotation "true") is left out while the
// annotation "false" does not hide one. It asserts the source's list: the
// completion surface sorts every source's names, so the shell shows them
// sorted.
func TestVersionSourceKeepsTheServedOrder(t *testing.T) {
	t.Parallel()
	ls := chartServer(t, enabledRepo)
	got := VersionSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("version", "--repo", "rancher-charts", "--chart", "mixed"))
	assert.Equal(t, []string{"3.0.0-rc.1", "1.1.0", "0.9.0"}, got.Names)
	assert.Equal(t, []string{repoObjectPath, repoIndexPath + "?link=index"}, ls.Requested())
}

// TestVersionSourceOffersNothingWithoutWhatItNeeds asserts no names without
// --repo or --chart, for a chart the index lacks, for a chart with only
// deprecated versions and for a disabled repository.
func TestVersionSourceOffersNothingWithoutWhatItNeeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		repo string
		tail []string
	}{
		{"no repo", enabledRepo, []string{"--chart", "mixed"}},
		{"no chart", enabledRepo, []string{"--repo", "rancher-charts"}},
		{"neither", enabledRepo, nil},
		{"unknown chart", enabledRepo, []string{"--repo", "rancher-charts", "--chart", "nope"}},
		{"only deprecated versions", enabledRepo, []string{"--repo", "rancher-charts", "--chart", "all-deprecated"}},
		{"disabled repository", `{"spec":{"enabled":false}}`, []string{"--repo", "rancher-charts", "--chart", "mixed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := chartServer(t, tt.repo)
			got := VersionSource(InstallCommand).Complete(t.Context(), ls.Conn(2), installRequest("version", tt.tail...))
			assert.Empty(t, got.Names)
		})
	}
}
