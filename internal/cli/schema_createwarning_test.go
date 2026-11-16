package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaCreateStateFollowsTheRow asserts the schema's create verb state
// for every row: a row with a CreateWarning has an available create with the
// row's contract and stability and the warning as its note, a row with
// NoCreateViaManifest keeps a refused one, and every other row has none. It
// also asserts the managedchart row has a CreateWarning.
func TestSchemaCreateStateFollowsTheRow(t *testing.T) {
	t.Parallel()
	m := schemaManifest(t)
	byKey := map[string]runtime.SchemaKind{}
	for _, k := range m.Kinds {
		byKey[k.Key] = k
	}
	warned := 0
	for _, row := range kinds.All() {
		sk, ok := byKey[row.Key]
		require.True(t, ok, row.Key)
		create, has := sk.Verbs["create"]
		switch {
		case row.CreateWarning != "":
			warned++
			require.True(t, has, "%s has a CreateWarning but no create state", row.Key)
			assert.Equal(t, runtime.VerbAvailable, create.State, row.Key)
			assert.Equal(t, string(row.Binding.Contract), create.Contract, row.Key)
			assert.Equal(t, string(row.Binding.Stability), create.Stability, row.Key)
			assert.Equal(t, row.CreateWarning, create.Note, row.Key)
		case row.NoCreateViaManifest != "":
			require.True(t, has, "%s refuses create but has no create state", row.Key)
			assert.Equal(t, runtime.VerbRefused, create.State, row.Key)
			assert.Equal(t, row.NoCreateViaManifest, create.Note, row.Key)
		default:
			assert.False(t, has, "%s has a create state but sets neither CreateWarning nor NoCreateViaManifest", row.Key)
		}
	}
	assert.Positive(t, warned)
	mc, ok := kinds.ByKey("managedchart")
	require.True(t, ok)
	assert.NotEmpty(t, mc.CreateWarning)
}
