package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
)

func TestRegistryBindingLookup(t *testing.T) {
	reg := runtime.NewRegistry(provider.NewResolver())
	b := reg.Binding("get cluster")
	assert.Equal(t, safety.TierCRD, b.Tier)
	assert.Equal(t, safety.Beta, b.Stability)

	b2 := reg.Binding("create project")
	assert.Equal(t, safety.TierPublic, b2.Tier)
	assert.Equal(t, safety.Stable, b2.Stability)

	bU := reg.Binding("nope")
	assert.Empty(t, bU.Tier)
	assert.Empty(t, bU.Stability)
}
