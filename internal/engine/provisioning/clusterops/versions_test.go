package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseVersionKey asserts the semver core and the trailing release
// number are read, and a core that is not three integers fails.
func TestParseVersionKey(t *testing.T) {
	t.Parallel()
	rows := []struct {
		raw                      string
		major, minor, patch, rel int
		ok                       bool
	}{
		{"v1.31.4+rke2r10", 1, 31, 4, 10, true},
		{"v1.31.4+rke2r1", 1, 31, 4, 1, true},
		{"v1.30.5+k3s1", 1, 30, 5, 1, true},
		{"1.31.4", 1, 31, 4, 0, true},
		{"v1.31.4+rke2", 1, 31, 4, 2, true},
		{"v1.31.4+", 1, 31, 4, 0, true},
		{"v1.31", 0, 0, 0, 0, false},
		{"v1.31.x+rke2r1", 0, 0, 0, 0, false},
		{"v1.31.0-rc1+rke2r1", 0, 0, 0, 0, false},
		{"", 0, 0, 0, 0, false},
	}
	for _, tc := range rows {
		major, minor, patch, rel, ok := ParseVersionKey(tc.raw)
		assert.Equal(t, tc.ok, ok, tc.raw)
		assert.Equal(t, []int{tc.major, tc.minor, tc.patch, tc.rel}, []int{major, minor, patch, rel}, tc.raw)
	}
}

// TestCompareVersions asserts the order: semver core, then the release number
// as a number; a string that does not parse is below every one that does.
func TestCompareVersions(t *testing.T) {
	t.Parallel()
	rows := []struct {
		a, b string
		want int
	}{
		{"v1.31.4+rke2r10", "v1.31.4+rke2r9", 1},
		{"v1.31.4+rke2r9", "v1.31.4+rke2r10", -1},
		{"v1.31.4+rke2r2", "v1.31.4+rke2r1", 1},
		{"v1.31.4+rke2r1", "v1.31.4+rke2r1", 0},
		{"v1.31.4+k3s1", "v1.31.4+k3s2", -1},
		{"v1.31.4", "v1.31.4+rke2r1", -1},
		{"v1.31.5+rke2r1", "v1.31.4+rke2r9", 1},
		{"v1.32.0+rke2r1", "v1.31.9+rke2r1", 1},
		{"v2.0.0+rke2r1", "v1.99.9+rke2r1", 1},
		{"v1.31.0-rc1+rke2r1", "v1.30.0+rke2r1", -1},
		{"v1.31.0-rc1+rke2r1", "v1.31.0-rc2+rke2r1", 0},
		{"junk", "v1.0.0", -1},
		{"v1.0.0", "junk", 1},
	}
	for _, tc := range rows {
		assert.Equal(t, tc.want, CompareVersions(tc.a, tc.b), tc.a+" against "+tc.b)
	}
}

// TestCompareTriple asserts the patch position is ignored for a wildcard
// operand and compared otherwise.
func TestCompareTriple(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, CompareTriple(1, 31, 4, 1, 31, 4, false))
	assert.Equal(t, -1, CompareTriple(1, 31, 3, 1, 31, 4, false))
	assert.Equal(t, 1, CompareTriple(1, 31, 5, 1, 31, 4, false))
	assert.Equal(t, 0, CompareTriple(1, 31, 3, 1, 31, 99, true))
	assert.Equal(t, -1, CompareTriple(1, 30, 99, 1, 31, 0, true))
	assert.Equal(t, 1, CompareTriple(2, 0, 0, 1, 99, 0, true))
}

// TestHasServerArgs asserts a row has serverArgs when the key is present and
// not null, an empty object included.
func TestHasServerArgs(t *testing.T) {
	t.Parallel()
	assert.True(t, Release{Version: "v1", ServerArgs: map[string]any{"a": "b"}}.HasServerArgs())
	assert.True(t, Release{Version: "v1", ServerArgs: map[string]any{}}.HasServerArgs())
	assert.False(t, Release{Version: "v1", ServerArgs: nil}.HasServerArgs())
	assert.False(t, Release{Version: "v1"}.HasServerArgs())
}

// TestNewestPatches asserts the newest release of each minor is returned over
// the rows with serverArgs, and rows without them and rows that do not parse
// are left out.
func TestNewestPatches(t *testing.T) {
	t.Parallel()
	args := map[string]any{}
	releases := []Release{
		{Version: "v1.31.3+rke2r1", ServerArgs: args},
		{Version: "v1.31.4+rke2r9", ServerArgs: args},
		{Version: "v1.31.4+rke2r10", ServerArgs: args},
		{Version: "v1.31.5+rke2r1"},
		{Version: "v1.30.9+rke2r1", ServerArgs: args},
		{Version: "v1.30.2+rke2r1", ServerArgs: args},
		{Version: "v1.29.0-rc1+rke2r1", ServerArgs: args},
		{Version: "junk", ServerArgs: args},
	}
	assert.Equal(t, map[string]string{"1.31": "v1.31.4+rke2r10", "1.30": "v1.30.9+rke2r1"}, NewestPatches(releases))
	assert.Empty(t, NewestPatches(nil))
}
