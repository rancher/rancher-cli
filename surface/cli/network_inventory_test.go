package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// networkedLocalCommandInventory is the reviewed, closed-world set of
// runtime.Local commands that perform live network I/O (a TLS/version
// preflight, a live token creation) and must declare Network: true so the
// schema manifest reflects what they actually do. The generated kind-table
// rows (runtime.ReadOnly/runtime.Mutating commands) always talk to the
// server and declare Network: true at their one shared builder — outside
// this inventory's scope, which covers only the hand-authored Local set.
var networkedLocalCommandInventory = map[string]bool{
	"auth status":          true,
	"auth login":           true,
	"auth get-token":       true,
	"token":                true,
	"config add-server":    true,
	"config update-server": true,
	"kubeconfig":           true,
}

// TestNetworkedLocalCommandsConformance pins the Local command network
// inventory bidirectionally: every reviewed entry must still declare
// Network: true, and no other Local command may declare Network: true
// without first being added to the reviewed inventory — so adding a live
// preflight to a Local command forces a decision here instead of leaving
// the schema manifest silently wrong, and no command can silently opt in
// to the network badge without review.
func TestNetworkedLocalCommandsConformance(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		if d.CommandType != runtime.Local {
			continue
		}
		want := networkedLocalCommandInventory[d.Name]
		assert.Equal(t, want, d.Network,
			"%s: Local command's Network flag does not match the reviewed inventory (want %v)", d.Name, want)
	}
}
