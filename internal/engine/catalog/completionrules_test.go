package catalog

import (
	"encoding/json"
	"maps"
	"testing"

	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletionSourcesApplyTheRulesOfIsDeprecatedAndIsHidden asserts the
// --version and --chart sources, which cannot import this package and so
// write the deprecated and hidden checks again, decide every version as
// IsDeprecated and IsHidden do: over the deprecated field true and false,
// the catalog.cattle.io/deprecated annotation "true", "false" and absent,
// and the catalog.cattle.io/hidden annotation absent, present and empty,
// present with "true" and present with "false". A version is offered by
// --version when it is not deprecated, and its chart by --chart when it is
// neither deprecated nor hidden.
func TestCompletionSourcesApplyTheRulesOfIsDeprecatedAndIsHidden(t *testing.T) {
	t.Parallel()
	deprecated := []struct {
		name       string
		field      bool
		annotation map[string]string
	}{
		{"field true, annotation absent", true, nil},
		{"field true, annotation true", true, map[string]string{deprecatedAnnotation: "true"}},
		{"field true, annotation false", true, map[string]string{deprecatedAnnotation: "false"}},
		{"field false, annotation absent", false, nil},
		{"field false, annotation true", false, map[string]string{deprecatedAnnotation: "true"}},
		{"field false, annotation false", false, map[string]string{deprecatedAnnotation: "false"}},
		{"field false, annotation empty", false, map[string]string{deprecatedAnnotation: ""}},
		{"field false, annotation mixed case", false, map[string]string{deprecatedAnnotation: "True"}},
	}
	hidden := []struct {
		name       string
		annotation map[string]string
	}{
		{"hidden absent", nil},
		{"hidden present and empty", map[string]string{hiddenAnnotation: ""}},
		{"hidden present with true", map[string]string{hiddenAnnotation: "true"}},
		{"hidden present with false", map[string]string{hiddenAnnotation: "false"}},
	}
	for _, d := range deprecated {
		for _, h := range hidden {
			t.Run(d.name+", "+h.name, func(t *testing.T) {
				t.Parallel()
				v := ChartVersion{Version: "1.0.0", Deprecated: d.field, Annotations: map[string]string{}}
				maps.Copy(v.Annotations, d.annotation)
				maps.Copy(v.Annotations, h.annotation)
				body, err := json.Marshal(Index{Entries: map[string][]ChartVersion{"c": {v}}})
				require.NoError(t, err)

				const repoPath = "/k8s/clusters/c-x/apis/catalog.cattle.io/v1/clusterrepos/r"
				const indexPath = "/k8s/clusters/c-x/v1/catalog.cattle.io.clusterrepos/r"
				ls := completiontest.New(t, map[string]string{repoPath: `{"spec":{"enabled":true}}`, indexPath: string(body)})
				req := completion.Request{Command: installCommandName, ClusterID: "c-x", Tail: []string{"--repo", "r", "--chart", "c"}}

				req.Flag = "version"
				versions := kindscomplete.VersionSource(installCommandName).Complete(t.Context(), ls.Conn(2), req)
				assert.Equal(t, !IsDeprecated(v), len(versions.Names) == 1, "--version offers the version when it is not deprecated: %v", versions.Names)

				req.Flag = "chart"
				charts := kindscomplete.ChartSource(installCommandName).Complete(t.Context(), ls.Conn(2), req)
				assert.Equal(t, !IsDeprecated(v) && !IsHidden(v), len(charts.Names) == 1, "--chart offers the chart when its version is neither deprecated nor hidden: %v", charts.Names)
			})
		}
	}
}

// TestIsHidden asserts a version is hidden when catalog.cattle.io/hidden is
// present and not empty, whatever its value.
func TestIsHidden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"no annotations", nil, false},
		{"absent", map[string]string{"other": "true"}, false},
		{"empty", map[string]string{hiddenAnnotation: ""}, false},
		{"true", map[string]string{hiddenAnnotation: "true"}, true},
		{"false", map[string]string{hiddenAnnotation: "false"}, true},
		{"any text", map[string]string{hiddenAnnotation: "yes"}, true},
	} {
		assert.Equal(t, tc.want, IsHidden(ChartVersion{Annotations: tc.annotations}), tc.name)
	}
}

// installCommandName is the registered name of app install.
var installCommandName = (&installCmd{}).Describe().Name
