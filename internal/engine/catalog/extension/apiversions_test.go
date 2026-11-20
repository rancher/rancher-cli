package extension

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPIVersionTable asserts the embedded table is well formed: every row
// names a release tag of a supported minor, a dashboard tag and an extensions
// API version that Coerce reads, and no tag is listed twice.
func TestAPIVersionTable(t *testing.T) {
	t.Parallel()
	rows := APIVersions()
	require.NotEmpty(t, rows)
	seen := map[string]bool{}
	for _, r := range rows {
		assert.Regexp(t, `^v[0-9]+\.[0-9]+\.[0-9]+$`, r.Rancher)
		assert.Regexp(t, `^v[0-9]+\.[0-9]+\.[0-9]+$`, r.Dashboard)
		assert.NotEmpty(t, r.ExtensionsAPI, r.Rancher)
		assert.False(t, seen[r.Rancher], "duplicate row %s", r.Rancher)
		seen[r.Rancher] = true
	}
}

// TestExtensionsAPIVersion asserts the lookup is by the exact release tag and
// that a head build, a release candidate, a version with no row and an empty
// version are not found.
func TestExtensionsAPIVersion(t *testing.T) {
	t.Parallel()
	row := APIVersions()[0]
	got, ok := ExtensionsAPIVersion(row.Rancher)
	assert.True(t, ok)
	assert.Equal(t, row.ExtensionsAPI, got)
	for _, v := range []string{"", "v2.16-head", row.Rancher + "-rc.1", "v9.9.9", "2.15.3"} {
		_, ok := ExtensionsAPIVersion(v)
		assert.False(t, ok, v)
	}
}
