package clusterops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseLooseVersionAcceptsWhatRancherAccepts asserts the parser accepts the
// strings the semantic version package Rancher uses accepts in its default
// coercing mode, including the ones ParseVersionKey refuses.
func TestParseLooseVersionAcceptsWhatRancherAccepts(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"v1.30.5+rke2r1", "1.30", "v1", "v01.2.3", "v1.25.0-rc1+k3s1", "v1.2.3-0", "v1.2.3-0a", "v1.2.3+001", "1-2",
		"v" + strings.Repeat("0", 255), strings.Repeat("0", 256), "v18446744073709551615.0.0", "v1.2.3-a.b-c.0+x.y-z",
	} {
		_, ok := parseLooseVersion(v)
		assert.True(t, ok, "%q", v)
	}
}

// TestParseLooseVersionRefusesWhatRancherRefuses asserts the parser refuses
// the strings Rancher's parser refuses.
func TestParseLooseVersionRefusesWhatRancherRefuses(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"", "v", "1.", "1.2.3.4", "x1.2", "V1.2.3", " v1.2.3", "v1.2.3 ", "v1.2.3\n", "v1.2.3-01", "v1.2.3-", "v1.2.3+",
		"v1.2.3-a..b", "v1.2.3+a.", "v1.2.3+a_b", "v1.2.3-\u00e9", "v1.2.3+\u00e9", "\u00e91.2.3", strings.Repeat("0", 257), "v18446744073709551616.0.0",
		"v1.18446744073709551616.0", "v1.2.18446744073709551616", "v1.2.3-a.01", "v1.2.3+a..b", "v1..3",
	} {
		_, ok := parseLooseVersion(v)
		assert.False(t, ok, "%q", v)
	}
}

// TestParseLooseVersionReadsTheNumbers asserts the three numbers and the
// pre-release flag, with a missing part read as 0.
func TestParseLooseVersionReadsTheNumbers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want looseVersion
	}{
		{"v1.30.5+rke2r1", looseVersion{1, 30, 5, false}},
		{"1.30", looseVersion{1, 30, 0, false}},
		{"v01.02.003", looseVersion{1, 2, 3, false}},
		{"v1.25.0-rc1+k3s1", looseVersion{1, 25, 0, true}},
		{"v1+x", looseVersion{1, 0, 0, false}},
	} {
		got, ok := parseLooseVersion(tc.in)
		assert.True(t, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}

// TestBelowKubernetes125 asserts the comparison with v1.25.0: lower numbers are
// below, a pre-release of 1.25.0 is below 1.25.0, build metadata is ignored, and
// for a version Rancher does not parse, ok is false.
func TestBelowKubernetes125(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in    string
		below bool
	}{
		{"v1.24.99", true}, {"v1.25.0-rc1", true}, {"v0.99.0", true}, {"v1.25.0", false}, {"v1.25.0+k3s1", false},
		{"1.25", false}, {"v1.25.1-rc1", false}, {"v1.26.0", false}, {"v2.0.0", false}, {"v1", true}, {"1.24", true},
	} {
		below, ok := BelowKubernetes125(tc.in)
		assert.True(t, ok, tc.in)
		assert.Equal(t, tc.below, below, tc.in)
	}
	_, ok := BelowKubernetes125("1.2.3.4")
	assert.False(t, ok)
	assert.False(t, KubernetesVersionParses(""))
	assert.True(t, KubernetesVersionParses("v1.30.5+rke2r1"))
}
