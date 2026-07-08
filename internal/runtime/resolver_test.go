package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComposeResolversRejectsOverlap(t *testing.T) {
	t.Parallel()
	a := MapResolver{"get cluster": {Tier: safety.TierCRD, Stability: safety.Beta}}
	b := MapResolver{"get cluster": {Tier: safety.TierCRD, Stability: safety.Beta}}
	_, err := ComposeResolvers(a, b)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get cluster", "the error names the overlapping op")
}

func TestComposeResolversMergesAll(t *testing.T) {
	t.Parallel()
	a := MapResolver{"create project": {Tier: safety.TierPublic, Stability: safety.Stable}}
	b := MapResolver{"get cluster": {Tier: safety.TierCRD, Stability: safety.Beta}}
	r, err := ComposeResolvers(a, b)
	require.NoError(t, err)

	assert.Len(t, r.All(), 2, "All() is the union of both sides")

	cp, ok := r.Resolve("create project")
	require.True(t, ok)
	assert.Equal(t, safety.TierPublic, cp.Tier)

	gc, ok := r.Resolve("get cluster")
	require.True(t, ok)
	assert.Equal(t, safety.TierCRD, gc.Tier)
}
