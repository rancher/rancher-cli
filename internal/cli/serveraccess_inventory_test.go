package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localServerAccessInventory is the reviewed, closed-world set of
// runtime.Local commands that contact the server, with the most each may
// do there: read for a TLS or version preflight, write for one that
// creates or deletes server objects (tokens). Every other Local command
// never contacts the server. The generated kind-table rows (runtime.ReadOnly
// and runtime.Mutating commands) take their type's default, outside this
// inventory's scope, which covers only the hand-authored Local set.
var localServerAccessInventory = map[string]runtime.ServerAccess{
	"auth status":          runtime.ServerAccessRead,
	"auth login":           runtime.ServerAccessWrite,
	"auth get-token":       runtime.ServerAccessWrite,
	"auth logout":          runtime.ServerAccessWrite,
	"auth delete-token":    runtime.ServerAccessWrite,
	"config delete-server": runtime.ServerAccessWrite,
	"token":                runtime.ServerAccessWrite,
	"config add-server":    runtime.ServerAccessRead,
	"config update-server": runtime.ServerAccessRead,
	"kubeconfig":           runtime.ServerAccessRead,
}

// TestLocalServerAccessConformance asserts the Local command inventory both
// ways: every reviewed entry still declares its server access, and no other
// Local command declares any without first being added here - so a Local
// command that starts contacting the server forces a decision here instead
// of leaving the schema manifest silently wrong.
func TestLocalServerAccessConformance(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if d.CommandType != runtime.Local {
			continue
		}
		want, ok := localServerAccessInventory[d.Name]
		if !ok {
			want = runtime.ServerAccessNone
		}
		assert.Equal(t, want, d.EffectiveServerAccess(),
			"%s: Local command's server access does not match the reviewed inventory", d.Name)
	}
}
