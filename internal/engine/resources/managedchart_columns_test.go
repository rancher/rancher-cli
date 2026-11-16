package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestManagedChartColumnsRenderTheirPaths asserts the columns of the
// managedchart row: CHART, VERSION and REPO show the spec fields,
// BUNDLEDEPLOYMENTS-READY shows status.display.readyClusters, READY shows the
// Ready condition, an object with no status renders the
// BUNDLEDEPLOYMENTS-READY and READY cells blank, and one with neither spec nor
// status renders the CHART, VERSION, REPO, BUNDLEDEPLOYMENTS-READY and READY
// cells blank.
func TestManagedChartColumnsRenderTheirPaths(t *testing.T) {
	t.Parallel()

	ready := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "monitoring", "namespace": "fleet-default"},
		"spec":     map[string]any{"chart": "rancher-monitoring", "version": "104.1.0", "repoName": "rancher-charts"},
		"status": map[string]any{
			"display": map[string]any{"readyClusters": "1/2"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
			},
		},
	}}
	noStatus := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "logging", "namespace": "fleet-default"},
		"spec":     map[string]any{"chart": "rancher-logging", "version": "104.0.0", "repoName": "rancher-charts"},
	}}
	bare := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "bare", "namespace": "fleet-default"},
	}}
	row := catalogRow(t, "managedchart")

	tbl := newTable(row, []unstructured.Unstructured{ready, noStatus, bare}, false)
	assert.Equal(t, []string{"NAME", "CHART", "VERSION", "REPO", "BUNDLEDEPLOYMENTS-READY", "READY", "AGE"}, tbl.Columns())
	rows := tbl.Rows()
	require.Len(t, rows, 3)
	assert.Equal(t, []string{"monitoring", "rancher-monitoring", "104.1.0", "rancher-charts", "1/2", "Ready"}, rows[0][:6])
	assert.Equal(t, []string{"logging", "rancher-logging", "104.0.0", "rancher-charts", "", ""}, rows[1][:6],
		"an absent status renders the BUNDLEDEPLOYMENTS-READY and READY cells blank")
	assert.Equal(t, []string{"bare", "", "", "", "", ""}, rows[2][:6],
		"an object with no spec and no status renders the CHART, VERSION, REPO, BUNDLEDEPLOYMENTS-READY and READY cells blank")
}
