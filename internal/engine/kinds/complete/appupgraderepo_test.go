package complete

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
)

// TestUpgradeVersionSourceKeepsTheVersionOnlyInTheAppsSourceRepo asserts the
// version the app runs is exempt from the namespace and release-name checks,
// and offered when the index does not list it, only for the ClusterRepo the
// app came from: with another --repo typed, the version must be in that
// repository's index and passes every check.
func TestUpgradeVersionSourceKeepsTheVersionOnlyInTheAppsSourceRepo(t *testing.T) {
	t.Parallel()
	const otherObject = "/k8s/clusters/c-x/apis/catalog.cattle.io/v1/clusterrepos/other-charts"
	otherIndex := strings.Replace(repoIndexPath, "rancher-charts", "other-charts", 1)
	tests := []struct {
		name  string
		index string
		want  []string
	}{
		{"the index hides the version the app runs", `{"entries":{"up-chart":[{"version":"3.0.0"}]}}`, []string{"3.0.0"}},
		{"the index lists it with a namespace annotation of another", `{"entries":{"up-chart":[{"version":"3.0.0"},` +
			`{"version":"1.2.0","annotations":{"catalog.cattle.io/namespace":"cattle-x"}}]}}`, []string{"3.0.0"}},
		{"the index lists it with a release-name annotation of another", `{"entries":{"up-chart":[{"version":"3.0.0"},` +
			`{"version":"1.2.0","annotations":{"catalog.cattle.io/release-name":"fleet"}}]}}`, []string{"3.0.0"}},
		{"the index lists it plain", `{"entries":{"up-chart":[{"version":"3.0.0"},{"version":"1.2.0"}]}}`, []string{"3.0.0", "1.2.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ls := completiontest.New(t, map[string]string{
				upgradeAppPath: upgradeApp,
				otherObject:    `{"metadata":{"name":"other-charts"},"spec":{"enabled":true}}`,
				otherIndex:     tt.index,
			})
			got := UpgradeSource(upgradeCommand).Complete(t.Context(), ls.Conn(3), upgradeRequest("version", "--repo", "other-charts"))
			assert.Equal(t, tt.want, got.Names)
		})
	}
}
