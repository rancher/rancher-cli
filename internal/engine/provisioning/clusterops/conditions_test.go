package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func conditionsOf(conds ...map[string]any) map[string]any {
	items := make([]any, 0, len(conds))
	for _, c := range conds {
		items = append(items, c)
	}
	return map[string]any{"status": map[string]any{"conditions": items}}
}

func cond(typ, status, message string) map[string]any {
	return map[string]any{"type": typ, "status": status, "message": message}
}

// TestReady asserts the Rancher UI's rule: when a Connected condition exists,
// its status must be True; only without one is the Ready condition used, and
// it must be True.
func TestReady(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name                 string
		cluster              map[string]any
		ready                bool
		typ, status, message string
	}{
		{"connected true", conditionsOf(cond("Connected", "True", "")), true, "Connected", "True", ""},
		{"connected false", conditionsOf(cond("Connected", "False", "agent offline")), false, "Connected", "False", "agent offline"},
		{"connected unknown", conditionsOf(cond("Connected", "Unknown", "")), false, "Connected", "Unknown", ""},
		{"connected false, ready true", conditionsOf(cond("Ready", "True", ""), cond("Connected", "False", "down")), false, "Connected", "False", "down"},
		{"connected true, ready false", conditionsOf(cond("Ready", "False", "waiting"), cond("Connected", "True", "")), true, "Connected", "True", ""},
		{"ready true without connected", conditionsOf(cond("Ready", "True", "")), true, "Ready", "True", ""},
		{"ready false without connected", conditionsOf(cond("Ready", "False", "waiting for agent")), false, "Ready", "False", "waiting for agent"},
		{"other conditions only", conditionsOf(cond("Updated", "True", "")), false, "", "", ""},
		{"no conditions", map[string]any{"status": map[string]any{}}, false, "", "", ""},
		{"no status", map[string]any{}, false, "", "", ""},
		{"conditions not a list", map[string]any{"status": map[string]any{"conditions": "x"}}, false, "", "", ""},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ready, typ, status, message := Ready(tc.cluster)
			assert.Equal(t, tc.ready, ready)
			assert.Equal(t, tc.typ, typ)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.message, message)
		})
	}
}
