package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oidcBespokeCommandNames is the reviewed set of bespoke commands whose
// Capability requirement must EQUAL the oidcclient kind row's - copied
// through the one shared helper, never a second hand-authored value. The
// coverage rule's uncoveredGated proves PRESENCE only (a typo'd Feature
// string would still pass it); this test proves SAMENESS.
var oidcBespokeCommandNames = []string{
	"create oidc-client",
	"oidc-client add-secret",
	"oidc-client rotate-secret",
	"oidc-client remove-secret",
}

// TestOIDCBespokeCommandsCapabilityEqualsRow asserts that every bespoke OIDC
// command's Capability equals the oidcclient row's exactly, and that the
// schema manifest projects identical prime/learnMore across all of them
// and the row's generated verbs.
func TestOIDCBespokeCommandsCapabilityEqualsRow(t *testing.T) {
	t.Parallel()
	row, ok := kinds.Lookup("oidcclient")
	require.True(t, ok)
	require.True(t, row.Capability.IsSet())

	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	byName := map[string]runtime.Descriptor{}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		byName[d.Name] = d
	}

	for _, name := range oidcBespokeCommandNames {
		d, ok := byName[name]
		require.True(t, ok, "command %q must be registered", name)
		assert.Equal(t, row.Capability, d.Capability, "command %q must copy the oidcclient row's Capability exactly", name)
	}

	m := wr.reg.Schema("v-test", "2.14")
	primeNames := append([]string{"get oidcclient", "patch oidcclient", "delete oidcclient"}, oidcBespokeCommandNames...)
	for _, name := range primeNames {
		found := false
		for _, c := range m.Commands {
			if c.Name != name {
				continue
			}
			found = true
			assert.True(t, c.Prime, "command %q must project prime:true", name)
			assert.NotEmpty(t, c.LearnMore, "command %q must project a learnMore URL", name)
		}
		assert.True(t, found, "command %q must appear in the schema manifest", name)
	}
}
