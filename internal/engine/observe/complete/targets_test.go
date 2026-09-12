package complete

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTargetsAreConsistent asserts every target has a name, no target is
// both local-only and downstream-only, a target with a static namespace
// has a selector, and Lookup and Names answer the table.
func TestTargetsAreConsistent(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, target := range Targets {
		assert.NotEmpty(t, target.Name)
		assert.False(t, seen[target.Name], target.Name)
		seen[target.Name] = true
		assert.False(t, target.LocalOnly && target.DownstreamOnly, target.Name)
		if target.Namespace != "" {
			assert.NotEmpty(t, target.Selector, target.Name)
		}
		got, ok := Lookup(target.Name)
		assert.True(t, ok)
		assert.Equal(t, target, got)
	}
	assert.Equal(t, []string{"server", "agent", "fleet-agent", "kube-api-auth", "webhook", "api-extension"}, Names())
	_, ok := Lookup("bogus")
	assert.False(t, ok)
}
