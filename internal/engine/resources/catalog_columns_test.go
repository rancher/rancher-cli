package resources

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func catalogRow(t *testing.T, key string) kinds.Kind {
	t.Helper()
	k, ok := kinds.ByKey(key)
	require.True(t, ok, key)
	return k
}

// TestCatalogColumnsRenderTheirPaths asserts the columns of the clusterrepo,
// app and operation rows: each renders the field its path names, ENABLED is
// blank when the field is absent, and COMMAND joins the list with commas.
func TestCatalogColumnsRenderTheirPaths(t *testing.T) {
	t.Parallel()

	repo := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "r1"},
		"spec":     map[string]any{"url": "https://charts.example.test", "enabled": true, "gitRepo": "https://git.example.test/c", "gitBranch": "main"},
		"status":   map[string]any{"downloadTime": "2026-01-02T03:04:05Z"},
	}}
	bare := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "r2"},
		"spec":     map[string]any{"url": "https://other.example.test"},
	}}
	tbl := newTable(catalogRow(t, "clusterrepo"), []unstructured.Unstructured{repo, bare}, false)
	assert.Equal(t, []string{"NAME", "HTTP-URL", "ENABLED", "GIT-REPO", "GIT-BRANCH", "DOWNLOAD-TIME", "AGE"}, tbl.Columns())
	rows := tbl.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"r1", "https://charts.example.test", "true", "https://git.example.test/c", "main", "2026-01-02T03:04:05Z"}, rows[0][:6])
	assert.Equal(t, []string{"r2", "https://other.example.test", "", "", "", ""}, rows[1][:6], "an absent field renders blank, ENABLED included")

	app := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "a1", "namespace": "default"},
		"spec": map[string]any{
			"name": "rel", "version": int64(3),
			"chart": map[string]any{"metadata": map[string]any{"name": "chart", "version": "1.2.3"}},
			"info":  map[string]any{"status": "deployed"},
		},
	}}
	tbl = newTable(catalogRow(t, "app"), []unstructured.Unstructured{app}, false)
	assert.Equal(t, []string{"NAME", "CHART", "VERSION", "RELEASE-NAME", "RELEASE-VERSION", "STATUS", "AGE"}, tbl.Columns())
	assert.Equal(t, []string{"a1", "chart", "1.2.3", "rel", "3", "deployed"}, tbl.Rows()[0][:6])

	op := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "o1", "namespace": "default"},
		"status":   map[string]any{"podNamespace": "cattle-system", "command": []any{"upgrade", "rancher-webhook"}},
	}}
	noCommand := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "o2", "namespace": "default"},
	}}
	tbl = newTable(catalogRow(t, "operation"), []unstructured.Unstructured{op, noCommand}, false)
	assert.Equal(t, []string{"NAME", "TARGET-NAMESPACE", "COMMAND", "AGE"}, tbl.Columns())
	rows = tbl.Rows()
	assert.Equal(t, []string{"o1", "cattle-system", "upgrade,rancher-webhook"}, rows[0][:3])
	assert.Equal(t, []string{"o2", "", ""}, rows[1][:3])
}
