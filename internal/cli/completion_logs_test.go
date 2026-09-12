package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestLogsTargetCompletionYieldsExactlyTheTargetTable asserts `logs <TAB>`
// against the curated target table: the positional's Enum (sourced from
// observe.Names(), never a hand-copied list) offers exactly those names,
// sorted, and nothing else.
func TestLogsTargetCompletionYieldsExactlyTheTargetTable(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{
		Name: "logs",
		Args: []runtime.ArgSpec{{Name: "target", Type: "string", Required: true, Enum: observe.Names()}},
	}
	got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 0}, d)
	assert.True(t, owned)
	assert.Equal(t, []string{"agent", "api-extension", "fleet-agent", "kube-api-auth", "server", "webhook"}, got)
}
