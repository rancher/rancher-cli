package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveKnownOps(t *testing.T) {
	r := NewResolver()
	t.Run("create project is public/stable", func(t *testing.T) {
		b, ok := r.Resolve("create project")
		assert.True(t, ok)
		assert.Equal(t, TierPublic, b.Tier)
		assert.Equal(t, Stable, b.Stability)
	})
	t.Run("get clusters is crd/beta", func(t *testing.T) {
		b, ok := r.Resolve("get clusters")
		assert.True(t, ok)
		assert.Equal(t, TierCRD, b.Tier)
	})
	t.Run("unknown op", func(t *testing.T) {
		_, ok := r.Resolve("does not exist")
		assert.False(t, ok)
	})
}
