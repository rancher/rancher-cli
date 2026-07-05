package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

func TestRegistryBindingLookup(t *testing.T) {
	reg := runtime.NewRegistry(provider.NewResolver())
	tier, stab := reg.Binding("get cluster")
	assert.Equal(t, "crd", tier)
	assert.Equal(t, "beta", stab)

	tier2, stab2 := reg.Binding("create project")
	assert.Equal(t, "public", tier2)
	assert.Equal(t, "stable", stab2)

	tU, sU := reg.Binding("nope")
	assert.Empty(t, tU)
	assert.Empty(t, sU)
}
