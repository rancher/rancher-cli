package skillinstall

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStatusCommandCarriesTheSelectionOfTheRun asserts the skill status
// command the report names: --agent when every target has one harness and
// --scope when every target has one scope.
func TestStatusCommandCarriesTheSelectionOfTheRun(t *testing.T) {
	t.Parallel()
	one := Report{Outcomes: []Outcome{{Target: Target{Harness: "claude", Scope: ScopeUser}}}}
	assert.Equal(t, "rancher skill status --agent claude --scope user", one.statusCommand())
	several := Report{Outcomes: []Outcome{
		{Target: Target{Harness: "claude", Scope: ScopeProject}},
		{Target: Target{Harness: "codex", Scope: ScopeProject}},
	}}
	assert.Equal(t, "rancher skill status --scope project", several.statusCommand())
	mixed := Report{Outcomes: []Outcome{
		{Target: Target{Harness: "claude", Scope: ScopeUser}},
		{Target: Target{Harness: "claude", Scope: ScopeProject}},
	}}
	assert.Equal(t, "rancher skill status --agent claude", mixed.statusCommand())
	assert.Equal(t, "rancher skill status", Report{}.statusCommand())
}
