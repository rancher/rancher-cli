package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledWithoutRemedy lists the rows that stop a wait on Stalled=True and
// have no StalledRemedy, each with the reason none is needed.
var stalledWithoutRemedy = map[string]string{
	"gitrepo":       "Fleet writes the clone or fetch error into the Stalled condition message, which the wait error prints",
	"helmop":        "Fleet writes the chart download or install error into the Stalled condition message, which the wait error prints",
	"bundle":        "Fleet writes the deployment error into the Stalled condition message, which the wait error prints",
	"fleet-cluster": "Fleet writes the failing bundle's error into the Stalled condition message, which the wait error prints",
}

// TestEveryStalledWaitRowSetsARemedyOrGivesAReason asserts each row with
// WaitFailsOnStalled sets StalledRemedy or is listed in stalledWithoutRemedy
// with a reason, and that no listed row sets a remedy or lacks the flag.
func TestEveryStalledWaitRowSetsARemedyOrGivesAReason(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, k := range All() {
		if !k.WaitFailsOnStalled {
			assert.Nil(t, k.StalledRemedy, "%s sets StalledRemedy without WaitFailsOnStalled", k.Name)
			continue
		}
		reason, listed := stalledWithoutRemedy[k.Name]
		switch {
		case k.StalledRemedy != nil:
			assert.False(t, listed, "%s sets StalledRemedy and is also listed in stalledWithoutRemedy", k.Name)
		case !listed:
			assert.Fail(t, "row stops a wait on Stalled with no remedy", "%s has WaitFailsOnStalled; set StalledRemedy or list it in stalledWithoutRemedy with a reason", k.Name)
		default:
			assert.NotEmpty(t, reason, k.Name)
			seen[k.Name] = true
		}
	}
	for name := range stalledWithoutRemedy {
		assert.True(t, seen[name], "stalledWithoutRemedy lists %s, which has no WaitFailsOnStalled or sets a remedy", name)
	}
}

// TestOperationRowSetsItsStalledRemedy asserts the operation row waits on
// Reconciling=False, stops on Stalled and offers the command that shows the
// release.
func TestOperationRowSetsItsStalledRemedy(t *testing.T) {
	t.Parallel()
	k, ok := Lookup("operation")
	require.True(t, ok)
	assert.Equal(t, "condition=Reconciling=False", k.WaitDefault)
	assert.True(t, k.WaitFailsOnStalled)
	require.NotNil(t, k.StalledRemedy)
}
