package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHasRKEConfig asserts HasRKEConfig holds exactly when spec.rkeConfig is
// present and not null: a custom cluster's empty object counts.
func TestHasRKEConfig(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		cluster map[string]any
		want    bool
	}{
		"populated": {map[string]any{"spec": map[string]any{"rkeConfig": map[string]any{"machinePools": []any{}}}}, true},
		"empty":     {map[string]any{"spec": map[string]any{"rkeConfig": map[string]any{}}}, true},
		"null":      {map[string]any{"spec": map[string]any{"rkeConfig": nil}}, false},
		"absent":    {map[string]any{"spec": map[string]any{"kubernetesVersion": "v1.31.4+rke2r1"}}, false},
		"no spec":   {map[string]any{}, false},
		"nil":       {nil, false},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, HasRKEConfig(tc.cluster))
		})
	}
}
