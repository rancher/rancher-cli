package provider

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

func TestResolveKnownOps(t *testing.T) {
	r := NewResolver()
	t.Run("create project is public/stable", func(t *testing.T) {
		b, ok := r.Resolve("create project")
		assert.True(t, ok)
		assert.Equal(t, safety.TierPublic, b.Tier)
		assert.Equal(t, safety.Stable, b.Stability)
	})
	t.Run("get cluster is crd/beta", func(t *testing.T) {
		b, ok := r.Resolve("get cluster")
		assert.True(t, ok)
		assert.Equal(t, safety.TierCRD, b.Tier)
	})
	t.Run("unknown op", func(t *testing.T) {
		_, ok := r.Resolve("does not exist")
		assert.False(t, ok)
	})
}
