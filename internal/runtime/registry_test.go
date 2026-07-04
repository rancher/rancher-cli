package runtime

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaMergesResolverTier(t *testing.T) {
	reg := NewRegistry(provider.NewResolver())
	reg.Register(fakeCmd{}) // "widgets ls", readOnly (from command_test.go)
	reg.Register(clustersLsStub{})
	reg.Register(flaggedStub{})

	m := reg.Schema("v-test")
	assert.Equal(t, "cli.rancher.io/v1", m.OutputAPIVersion)
	assert.NotEmpty(t, m.ErrorCodes)
	assert.NotEmpty(t, m.GlobalFlags)

	byName := map[string]SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}
	require.Contains(t, byName, "get clusters")
	assert.Equal(t, "crd", byName["get clusters"].Tier) // merged from resolver

	// Per-command Flags must reach the manifest (agent discoverability).
	require.Contains(t, byName, "widgets set")
	require.Len(t, byName["widgets set"].Flags, 1)
	assert.Equal(t, "token", byName["widgets set"].Flags[0].Name)
	assert.True(t, byName["widgets set"].Flags[0].Required)
}

type clustersLsStub struct{ fakeCmd }

func (clustersLsStub) Describe() Descriptor {
	return Descriptor{Name: "get clusters", CommandType: ReadOnly}
}

type flaggedStub struct{ fakeCmd }

func (flaggedStub) Describe() Descriptor {
	return Descriptor{Name: "widgets set", CommandType: Local,
		Flags: []ArgSpec{{Name: "token", Type: "string", Required: true}}}
}
