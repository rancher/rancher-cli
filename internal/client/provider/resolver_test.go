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
	t.Run("generated ops are not owned by the bespoke resolver", func(t *testing.T) {
		_, ok := r.Resolve("get cluster")
		assert.False(t, ok, "get cluster's binding is owned by the kind table now")
	})
	t.Run("delete provisioning-cluster is not hand-copied here", func(t *testing.T) {
		_, ok := r.Resolve("delete provisioning-cluster")
		assert.False(t, ok, "this op's binding must be derived from the provisioningcluster kind row at BuildRoot composition time (surface/cli/root.go), never hand-copied into this bespoke map — a hand-copied literal has no machine-checked link back to the row and can silently drift")
	})
	t.Run("unknown op", func(t *testing.T) {
		_, ok := r.Resolve("does not exist")
		assert.False(t, ok)
	})
}
