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
		{ActionScale, true},
		{Action(""), false},
		{Action("destroy"), false},
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
		Destructive: true, Tier: TierPublic, Stability: Stable,
	}
	b, err := json.Marshal(eff)
	require.NoError(t, err)
	for _, key := range []string{`"action":"delete"`, `"server":"prod"`, `"cluster":"c-abc"`, `"tier":"public"`, `"stability":"stable"`} {
		assert.Contains(t, string(b), key, "Effect JSON")
	}
}
