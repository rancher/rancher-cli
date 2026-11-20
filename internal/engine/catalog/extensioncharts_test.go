package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extensionIndex holds one chart for each position of the ui-component
// annotation: on the first version, on a later version, on the only version,
// on none, and with another value.
const extensionIndex = `{"entries":{
  "first":  [{"name":"first","version":"2.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}},{"name":"first","version":"1.0.0"}],
  "later":  [{"name":"later","version":"2.0.0"},{"name":"later","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}}],
  "only":   [{"name":"only","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}}],
  "none":   [{"name":"none","version":"1.0.0","annotations":{"a":"b"}}],
  "other":  [{"name":"other","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"dashboard"}}],
  "dep":    [{"name":"dep","version":"2.0.0","deprecated":true,"annotations":{"catalog.cattle.io/ui-component":"plugins"}},{"name":"dep","version":"1.0.0"}]
}}`

func chartNames(t *testing.T, items chartList) []string {
	t.Helper()
	var names []string
	for _, it := range items {
		names = append(names, it.Repository+"/"+it.Name)
	}
	return names
}

// TestChartsHideExtensionCharts asserts charts leaves out a chart when its
// first, a later or its only version carries ui-component plugins, with and
// without --all-versions and --include-deprecated, with --repo and without,
// and keeps a chart with the annotation on no version or with another value.
func TestChartsHideExtensionCharts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		in    map[string]string
		bools map[string]bool
	}{
		{"default", nil, nil},
		{"all versions", nil, map[string]bool{"all-versions": true}},
		{"include deprecated", nil, map[string]bool{"include-deprecated": true}},
		{"both", nil, map[string]bool{"all-versions": true, "include-deprecated": true}},
	} {
		for _, withRepo := range []bool{true, false} {
			name := tc.name + " without --repo"
			if withRepo {
				name = tc.name + " with --repo"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s := map[string]string{"cluster": "local"}
				if withRepo {
					s["repo"] = "r1"
				}
				cluster := &fakeCluster{items: nil}
				cluster.items = append(cluster.items, repoObject("r1", nil))
				root := newFakeRoot(map[string]rootAnswer{localIndexPrefix + "r1?link=index": {status: 200, body: extensionIndex}})
				res, err := runCharts(t, &fakeClients{cluster: cluster}, root, input{s: s, b: tc.bools})
				require.NoError(t, err)
				names := chartNames(t, resultItems(t, res))
				assert.Contains(t, names, "r1/none")
				assert.Contains(t, names, "r1/other")
				for _, hidden := range []string{"r1/first", "r1/later", "r1/only", "r1/dep"} {
					assert.NotContains(t, names, hidden)
				}
			})
		}
	}
}

// TestChartsRepoOfOnlyExtensionChartsIsEmpty asserts --repo of a repository
// that holds only extension charts is an empty list, not an error.
func TestChartsRepoOfOnlyExtensionChartsIsEmpty(t *testing.T) {
	t.Parallel()
	body := `{"entries":{"only":[{"name":"only","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins"}}]}}`
	cluster := &fakeCluster{items: nil}
	cluster.items = append(cluster.items, repoObject("r1", nil))
	root := newFakeRoot(map[string]rootAnswer{localIndexPrefix + "r1?link=index": {status: 200, body: body}})
	res, err := runCharts(t, &fakeClients{cluster: cluster}, root, input{s: map[string]string{"cluster": "local", "repo": "r1"}})
	require.NoError(t, err)
	assert.Empty(t, resultItems(t, res))
	assert.Empty(t, res.Warnings)
}
