package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestInstallRefusesAnExtensionChart asserts that app install refuses a chart
// any of whose versions is a Rancher UI extension, with and without
// --version, before it reads the chart's values or sends a request, and that
// an ordinary chart of the same index installs as before.
func TestInstallRefusesAnExtensionChart(t *testing.T) {
	t.Parallel()
	ext := map[string]string{"catalog.cattle.io/ui-component": "plugins"}
	cases := []struct {
		name     string
		versions []ChartVersion
		version  string
	}{
		{name: "annotation on the first version", versions: []ChartVersion{{Version: "2.0.0", Annotations: ext}, {Version: "1.0.0"}}},
		{name: "annotation on a later version", versions: []ChartVersion{{Version: "2.0.0"}, {Version: "1.0.0", Annotations: ext}}},
		{name: "a typed version", versions: []ChartVersion{{Version: "2.0.0", Annotations: ext}, {Version: "1.0.0", Annotations: ext}}, version: "1.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: tc.versions})}
			in := installInput(func(in *appInput) {
				if tc.version != "" {
					in.s[flagVersion] = tc.version
				}
			})
			_, _, err := newTestInstall(f, nil).Plan(t.Context(), in)
			require.ErrorContains(t, err, "chart 'nginx' in ClusterRepo 'repo1' is a Rancher UI extension, which belongs in the namespace cattle-ui-plugin-system; "+
				"this CLI does not install extensions yet")
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, clierr.Rancher+" extension list", ce.Hint)
			assert.Zero(t, f.postCount())
		})
	}

	t.Run("an ordinary chart of the same index installs", func(t *testing.T) {
		t.Parallel()
		f := newAppFake(t)
		f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{
			testChart:  {{Version: "2.0.0"}},
			"ext-only": {{Version: "1.0.0", Annotations: ext}},
		})}
		f.root.answers[infoPath(testChart, "2.0.0")] = rootAnswer{status: 200, body: `{"values":{},"chart":{}}`}
		eff, _, err := newTestInstall(f, nil).Plan(t.Context(), installInput())
		require.NoError(t, err)
		assert.Equal(t, "2.0.0", diffPaths(eff)["version"])
	})
}
