package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNoKindDeclaresCapabilityUntilCreateGatingLands is a deliberate
// tripwire: `create resource -f` resolves its kind inside Plan, AFTER the
// pipeline's capability stage has already run against a static Descriptor,
// so a Capability-bearing kind would be gated on get/patch/delete but NOT
// on create-via-manifest — a silent bypass of the mandatory stage. The
// create-path gate needs its own designed seam (tracked in the backlog)
// before the first kind row may set Capability; this test turns that
// ordering into a build failure instead of a latent hole.
func TestNoKindDeclaresCapabilityUntilCreateGatingLands(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		assert.False(t, k.Capability.IsSet(),
			"kind %q declares a capability requirement, but create resource -f cannot enforce it yet — design and ship the create-path capability gate first (see backlog)", k.Key)
	}
}
