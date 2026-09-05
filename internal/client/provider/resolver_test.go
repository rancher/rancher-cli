package provider

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

func TestResolveKnownOps(t *testing.T) {
	r := NewResolver()
	resolve := func(op string) (safety.Binding, bool) { b, ok := r[op]; return b, ok }
	// Ops with a 1:1 backing kind row are absent from this map by design:
	// they derive from their rows at composition time in BuildRoot
	// (derivedOpKinds; surface/cli's TestCreateOpBindingsTrackKindRows).
	// A literal reappearing here collides in ComposeResolvers and fails
	// BuildRoot, so absence needs no per-op assertion.
	t.Run("grant is public/stable", func(t *testing.T) {
		b, ok := resolve("grant")
		assert.True(t, ok)
		assert.Equal(t, safety.ContractPublic, b.Contract)
		assert.Equal(t, safety.Stable, b.Stability)
	})
	t.Run("revoke is public/stable", func(t *testing.T) {
		b, ok := resolve("revoke")
		assert.True(t, ok)
		assert.Equal(t, safety.ContractPublic, b.Contract)
		assert.Equal(t, safety.Stable, b.Stability)
	})
	t.Run("inspect project is internal/beta (deliberate downgrade from prior public)", func(t *testing.T) {
		b, ok := resolve("inspect project")
		assert.True(t, ok)
		assert.Equal(t, safety.ContractInternal, b.Contract)
		assert.Equal(t, safety.Beta, b.Stability)
	})
	t.Run("project add-member is not owned by the bespoke resolver", func(t *testing.T) {
		_, ok := resolve("project add-member")
		assert.False(t, ok, "project add-member was replaced by 'grant'; no binding must survive here")
	})
	t.Run("project remove-member is not owned by the bespoke resolver", func(t *testing.T) {
		_, ok := resolve("project remove-member")
		assert.False(t, ok, "project remove-member was replaced by 'revoke'; no binding must survive here")
	})
	t.Run("generated ops are not owned by the bespoke resolver", func(t *testing.T) {
		_, ok := resolve("get cluster")
		assert.False(t, ok, "get cluster's binding is owned by the kind table now")
	})
	t.Run("delete provisioning-cluster is not hand-copied here", func(t *testing.T) {
		_, ok := resolve("delete provisioning-cluster")
		assert.False(t, ok, "this op's binding must be derived from the provisioningcluster kind row at BuildRoot composition time (surface/cli/root.go), never hand-copied into this bespoke map - a hand-copied literal has no machine-checked link back to the row and can silently drift")
	})
	t.Run("unknown op", func(t *testing.T) {
		_, ok := resolve("does not exist")
		assert.False(t, ok)
	})
}
