package fidelity

import (
	"fmt"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepStateValue is a value of a state file field that is not a string, and
// the text the run reads for it.
type keepStateValue struct {
	name  string
	value any
	text  string
}

var keepStateValues = []keepStateValue{{"null", nil, "null"}, {"a number", 5, "5"}}

// TestKeepRunReadsAStateValueThatIsNotAString asserts how a kept run reads a
// null or a number in the containerId, adminToken, boot, chart and installUuid
// fields of the state file: as the JSON text of the value ("null", "5"), never
// as an empty value. A container id read that way differs from the container's
// and stops the run, an admin token goes to the token check and the test run, a
// boot or a chart differs from the run's value and recreates the pair, and an
// install id goes to the test run with no read of install-uuid on the server.
// The containerId, adminToken and installUuid rows tell a value read as text
// from a value read as empty by the text they assert. The boot and chart rows
// assert only the recreation: a value read as text and an empty value both
// differ from the value the run expects, and a test cannot tell the two apart.
// The rows run on the community and helm-check slots; the chart is compared on
// a helm pair only.
// The oracleImage field and the fields of the downstream are read as a string
// or not at all: keepStopRows and keepDownstreamStopRows assert that.
func TestKeepRunReadsAStateValueThatIsNotAString(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	requireKeepSlotsMatchTheScript(t)
	rows := []struct {
		field  string
		slots  []string
		assert func(t *testing.T, k *keepRun, res runResult, text string)
	}{
		{"containerId", keepFullSlots, func(t *testing.T, k *keepRun, res runResult, text string) {
			t.Helper()
			require.Equalf(t, 4, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stderr, fmt.Sprintf("failed its container-id check: container %s has id oracle-id, but the state file records %s; nothing was removed", k.slot.container, text))
		}},
		{"adminToken", keepFullSlots, func(t *testing.T, k *keepRun, res runResult, text string) {
			t.Helper()
			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stdout, "==> reusing healthy kept container")
			if k.slot.name == "community" {
				env, called := k.s.goEnv()
				require.True(t, called, "the test run starts")
				assert.Equal(t, text, env["E2E_FIDELITY_ADMIN_TOKEN"])
			}
		}},
		{"boot", keepFullSlots, func(t *testing.T, k *keepRun, res runResult, _ string) {
			t.Helper()
			assert.Contains(t, res.stdout, "==> kept pair was created with a different boot; recreating")
			requireRecreated(t, k, res)
		}},
		{"chart", []string{"helm-check"}, func(t *testing.T, k *keepRun, res runResult, _ string) {
			t.Helper()
			assert.Contains(t, res.stdout, "==> kept pair was created from different chart, k3s or cert-manager pins; recreating")
			requireRecreated(t, k, res)
		}},
		{"installUuid", keepFullSlots, func(t *testing.T, k *keepRun, res runResult, text string) {
			t.Helper()
			require.Equalf(t, 0, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			for _, line := range k.s.curlLog() {
				assert.NotContains(t, line, "install-uuid", "a state file with an install id makes no read of install-uuid")
			}
			if k.slot.name == "community" {
				env, called := k.s.goEnv()
				require.True(t, called, "the test run starts")
				assert.Equal(t, text, env[installUUIDEnv])
			}
		}},
	}
	for _, slot := range keepSlots {
		for _, row := range rows {
			if !slices.Contains(row.slots, slot.name) {
				continue
			}
			for _, value := range keepStateValues {
				t.Run(slot.name+"/"+row.field+" is "+value.name, func(t *testing.T) {
					t.Parallel()
					k := newKeepRun(t, slot)
					k.state[row.field] = value.value
					res := k.run()
					row.assert(t, k, res, value.text)
				})
			}
		}
	}
}
