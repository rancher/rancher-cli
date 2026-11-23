package extension

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func ann(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// TestIsExtensionChart asserts a chart is an extension when any version, first
// or later, carries ui-component plugins, and not for another value or none.
func TestIsExtensionChart(t *testing.T) {
	t.Parallel()
	plugins := ann(AnnotationUIComponent, "plugins")
	other := ann(AnnotationUIComponent, "other")
	tests := []struct {
		name     string
		versions []map[string]string
		want     bool
	}{
		{"first version", []map[string]string{plugins, nil}, true},
		{"a later version", []map[string]string{nil, other, plugins}, true},
		{"only version", []map[string]string{plugins}, true},
		{"annotation absent", []map[string]string{nil, ann("a", "b")}, false},
		{"another value", []map[string]string{other}, false},
		{"empty value", []map[string]string{ann(AnnotationUIComponent, "")}, false},
		{"no versions", nil, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsExtensionChart(tt.versions), tt.name)
	}
	assert.True(t, IsExtensionVersion(plugins))
	assert.False(t, IsExtensionVersion(other))
}

// TestIsHiddenExtension asserts the Rancher UI's Extensions page rule: any
// version with catalog.cattle.io/hidden equal to "true", and no other value.
func TestIsHiddenExtension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		versions []map[string]string
		want     bool
	}{
		{"true on first", []map[string]string{ann(AnnotationHidden, "true"), nil}, true},
		{"true on a later version", []map[string]string{nil, ann(AnnotationHidden, "true")}, true},
		{"false", []map[string]string{ann(AnnotationHidden, "false")}, false},
		{"any other non-empty value", []map[string]string{ann(AnnotationHidden, "yes")}, false},
		{"absent", []map[string]string{nil}, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsHiddenExtension(tt.versions), tt.name)
	}
}

// TestKubernetesVersion asserts which field the kube-version check reads and
// that the distribution suffix is removed: status.info first, then
// status.version.gitVersion, each with everything from "+" or "-" cut.
func TestKubernetesVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		info, git string
		want      string
		ok        bool
	}{
		{"info only", "v1.36.5", "", "v1.36.5", true},
		{"version only", "", "v1.30.2", "v1.30.2", true},
		{"both equal", "v1.36.5", "v1.36.5", "v1.36.5", true},
		{"both different, info takes precedence", "v1.36.5", "v1.30.2", "v1.36.5", true},
		{"neither", "", "", "", false},
		{"info with k3s suffix", "v1.36.5+k3s1", "", "v1.36.5", true},
		{"version with rke2 suffix", "", "v1.30.2-rancher1", "v1.30.2", true},
		{"info that differs from version with suffixes", "v1.36.5+k3s1", "v1.30.2+rke2r1", "v1.36.5", true},
	}
	for _, tt := range tests {
		got, ok := KubernetesVersion(tt.info, tt.git)
		assert.Equal(t, tt.ok, ok, tt.name)
		assert.Equal(t, tt.want, got, tt.name)
	}
}

// TestVersionLabel asserts the label of a chart version: the app version alone
// when it equals the chart version or is empty, else the app version with the
// chart version in parentheses.
func TestVersionLabel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "1.2.3", VersionLabel("1.2.3", "1.2.3"))
	assert.Equal(t, "1.2.3", VersionLabel("1.2.3", ""))
	assert.Equal(t, "2.0.0 (1.2.3)", VersionLabel("1.2.3", "2.0.0"))
	assert.Equal(t, "2.0.0", PluginVersion("1.2.3", "2.0.0"))
	assert.Equal(t, "1.2.3", PluginVersion("1.2.3", ""))
}

// TestIsBundleChartReadsEveryVersion asserts the bundle rule at every position
// of the annotation: the first version, a later one, an empty value and none.
func TestIsBundleChartReadsEveryVersion(t *testing.T) {
	t.Parallel()
	bundle := map[string]string{AnnotationBundleMembers: "a,b"}
	empty := map[string]string{AnnotationBundleMembers: ""}
	plain := map[string]string{}
	cases := []struct {
		name     string
		versions []map[string]string
		want     bool
	}{
		{name: "first version", versions: []map[string]string{bundle, plain}, want: true},
		{name: "a later version", versions: []map[string]string{plain, plain, bundle}, want: true},
		{name: "empty value", versions: []map[string]string{empty, plain}},
		{name: "absent", versions: []map[string]string{plain, plain}},
		{name: "no versions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsBundleChart(tc.versions))
		})
	}
}
