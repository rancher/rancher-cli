package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsPrerelease asserts the shape of a prerelease: three numeric parts,
// leading zeros and a leading "v" allowed, a "-" prerelease after them and
// optional build metadata. A "-" inside the build metadata and any other
// shape are not prereleases.
func TestIsPrerelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.0.0-rc.1", true},
		{"v1.0.0-rc.1", true},
		{"1.0.0-alpha", true},
		{"1.0.0-rc.1+build", true},
		{"1.0.001-rc1", true},
		{"1.0.0", false},
		{"5.4.001", false},
		{"106.0.2+up0.6.0", false},
		{"1.0.0+up1.2-rc", false},
		{"3.1908-rc1", false},
		{"1.2-beta", false},
		{"release-1.0", false},
		{"latest", false},
		{"", false},
		{"1.0.0-", false},
	} {
		assert.Equal(t, tc.want, IsPrerelease(tc.version), tc.version)
	}
}

func versionsOf(texts ...string) []ChartVersion {
	out := make([]ChartVersion, 0, len(texts))
	for _, t := range texts {
		out = append(out, ChartVersion{Version: t})
	}
	return out
}

// TestDefaultVersion asserts the pick: the first version in the given order
// that is not a prerelease, else the first version, and none for no versions.
func TestDefaultVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []string
		want string
		ok   bool
	}{
		{"first release", []string{"2.0.0", "1.0.0"}, "2.0.0", true},
		{"skips a leading prerelease", []string{"3.0.0-rc.1", "2.0.0", "1.0.0"}, "2.0.0", true},
		{"only prereleases", []string{"1.0.0-rc.2", "1.0.0-rc.10"}, "1.0.0-rc.2", true},
		{"order is the caller's, not numeric", []string{"1.9.0", "1.10.0"}, "1.9.0", true},
		{"leading zeros", []string{"5.4.001", "3.0.601"}, "5.4.001", true},
		{"short version", []string{"v3.1908", "v3.1907"}, "v3.1908", true},
		{"no versions", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DefaultVersion(versionsOf(tc.in...))
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got.Version)
		})
	}
}

// TestIsDeprecated asserts a version is deprecated by its deprecated field or
// by the annotation with the value "true", and by no other value.
func TestIsDeprecated(t *testing.T) {
	t.Parallel()
	assert.True(t, IsDeprecated(ChartVersion{Deprecated: true}))
	assert.True(t, IsDeprecated(ChartVersion{Annotations: map[string]string{"catalog.cattle.io/deprecated": "true"}}))
	assert.False(t, IsDeprecated(ChartVersion{Annotations: map[string]string{"catalog.cattle.io/deprecated": "false"}}))
	assert.False(t, IsDeprecated(ChartVersion{Annotations: map[string]string{"catalog.cattle.io/deprecated": "True"}}))
	assert.False(t, IsDeprecated(ChartVersion{}))
}
