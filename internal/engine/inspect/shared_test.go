package inspect

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompositesRejectProjectScope pins PC-3's composite-rejection rule: the
// non-project inspect composites bypass Route, so without an explicit check
// the global --project flag would reach them and be silently ignored — the
// exact no-op class DR-004 removed --project for.
func TestCompositesRejectProjectScope(t *testing.T) {
	tests := []struct {
		name string
		cmd  runtime.Command
		in   runtime.Input
	}{
		{"inspect pod", NewPod(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"inspect deployment", NewDeployment(&fakeFactory{}),
			input{args: []string{"web"}, s: map[string]string{"cluster": "c-abc", "namespace": "default", "project": "p-x"}}},
		{"top node", NewTopNode(&fakeFactory{}),
			input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}}},
		{"images", NewImages(&fakeFactory{}),
			input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := execRead(t, tt.cmd, tt.in)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, tt.name)
		})
	}
}
