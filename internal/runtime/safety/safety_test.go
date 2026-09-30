package safety

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		action Action
		want   bool
	}{
		{ActionCreate, true},
		{ActionUpdate, true},
		{ActionDelete, true},
		{Action(""), false},
		{Action("destroy"), false},
		{Action("scale"), false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.action.Valid(), "Action(%q).Valid()", tt.action)
	}
}

func TestEffectJSONShape(t *testing.T) {
	t.Parallel()
	eff := Effect{
		Action: ActionDelete, Target: "projects/web",
		Server: "prod", Cluster: "c-abc",
		ResourceVersion: "42", ConflictDetection: true,
		Destructive: true,
	}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "delete", m["action"])
	assert.Equal(t, "prod", m["server"])
	assert.Equal(t, "c-abc", m["cluster"])
	// contract and stability are NOT in the serialized Effect (manifest is
	// the single source).
	assert.NotContains(t, m, "contract")
	assert.NotContains(t, m, "stability")
	assert.NotContains(t, m, "tier")
}

// TestEffectWarningsNeverSerialize asserts the json:"-" tag deliberately kept on
// Warnings: the Effect marshals inside the envelope's data
// field, and renderEffect (internal/runtime/pipeline.go) copies these into
// the envelope's top-level warnings - serializing here too would emit every
// warning twice (data.warnings + warnings).
func TestEffectWarningsNeverSerialize(t *testing.T) {
	t.Parallel()
	eff := Effect{Action: ActionUpdate, Target: "x", Warnings: []string{"note-a"}}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	assert.NotContains(t, m, "warnings")
	assert.NotContains(t, m, "Warnings")
}
