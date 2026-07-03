package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveKnownOps(t *testing.T) {
	r := NewResolver()
	t.Run("projects create is public/stable", func(t *testing.T) {
		b, ok := r.Resolve("projects create")
		assert.True(t, ok)
		assert.Equal(t, TierPublic, b.Tier)
		assert.Equal(t, Stable, b.Stability)
	})
	t.Run("clusters ls is crd/beta", func(t *testing.T) {
		b, ok := r.Resolve("clusters ls")
		assert.True(t, ok)
		assert.Equal(t, TierCRD, b.Tier)
	})
	t.Run("unknown op", func(t *testing.T) {
		_, ok := r.Resolve("does not exist")
		assert.False(t, ok)
	})
}
