package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

// extensionIndex has an extension chart on each position of the annotation
// and ordinary charts beside them.
const extensionIndex = `{"entries":{
  "plain": [{"version":"1.0.0"}],
  "first": [{"version":"2.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}},{"version":"1.0.0"}],
  "later": [{"version":"2.0.0"},{"version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}}],
  "only":  [{"version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}}],
  "other": [{"version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"dashboard"}}]
}}`

func extensionServer(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{repoObjectPath: enabledRepo, repoIndexPath: extensionIndex})
}

// TestChartSourceLeavesOutExtensionCharts asserts the --chart source never
// offers a chart that app install refuses as a Rancher UI extension, with and
// without --version typed, and still offers an ordinary chart of the index.
func TestChartSourceLeavesOutExtensionCharts(t *testing.T) {
	t.Parallel()
	got := ChartSource(InstallCommand).Complete(t.Context(), extensionServer(t).Conn(2), installRequest("chart", "--repo", "rancher-charts"))
	assert.ElementsMatch(t, []string{"plain", "other"}, got.Names)

	got = ChartSource(InstallCommand).Complete(t.Context(), extensionServer(t).Conn(2), installRequest("chart", "--repo", "rancher-charts", "--version", "1.0.0"))
	assert.ElementsMatch(t, []string{"plain", "other"}, got.Names)
}

// TestVersionSourceOffersNothingForAnExtensionChart asserts the --version
// source offers no version of an extension chart typed in --chart, and still
// offers the versions of an ordinary chart.
func TestVersionSourceOffersNothingForAnExtensionChart(t *testing.T) {
	t.Parallel()
	for _, chart := range []string{"first", "later", "only"} {
		got := VersionSource(InstallCommand).Complete(t.Context(), extensionServer(t).Conn(2), installRequest("version", "--repo", "rancher-charts", "--chart", chart))
		assert.Empty(t, got.Names, chart)
	}
	for _, chart := range []string{"plain", "other"} {
		got := VersionSource(InstallCommand).Complete(t.Context(), extensionServer(t).Conn(2), installRequest("version", "--repo", "rancher-charts", "--chart", chart))
		assert.Equal(t, []string{"1.0.0"}, got.Names, chart)
	}
}
