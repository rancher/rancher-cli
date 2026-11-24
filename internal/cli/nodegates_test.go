package cli

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runNodeCommand runs node drain or node stop-drain through the pipeline
// against a node server, with the gates as typed.
func runNodeCommand(t *testing.T, s *nodeServer, command string, in fieldCoverageInput, apply, yes, dryRun bool) (int, *clierr.Error) {
	t.Helper()
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	cmd := entries[command].make(s.deps())
	var out, errOut bytes.Buffer
	return runtime.Run(t.Context(), cmd, in, runtime.IO{
		Out: &out, Err: &errOut, Format: "json", Apply: apply, Yes: yes, DryRun: dryRun,
		Capability: scrubAllowAll{}, Projects: scrubProjects{},
	}, wr.reg.Binding(command))
}

// TestNodeGates asserts a command cannot weaken its gates: node drain with
// --apply and no --yes is CONFIRMATION_REQUIRED and sends no write beyond the
// dry run, node drain with --apply --yes writes, node stop-drain with --apply
// alone writes, and a preview writes nothing but the dry run. The same holds
// for the requests through Rancher's node action.
func TestNodeGates(t *testing.T) {
	worker := map[string]any{"worker": true, "internalNodeSpec": map[string]any{}}
	draining := map[string]any{"worker": true, "desiredNodeUnschedulable": "drain", "internalNodeSpec": map[string]any{"unschedulable": true}}
	noWait := func(args ...string) fieldCoverageInput {
		return fieldCoverageInput{args: args, values: map[string]string{"cluster": scrubCluster}, bools: map[string]bool{"wait": false}}
	}
	noPlant := func(map[string]any) {}
	t.Run("drain without yes", func(t *testing.T) {
		s := newNodeServer(t, "active", nodeObject(worker), nodeObject(worker), noPlant)
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), true, false, false)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
		assert.Equal(t, []bool{true}, s.writes(), "only the dry run is sent")
	})
	t.Run("drain with yes", func(t *testing.T) {
		s := newNodeServer(t, "active", nodeObject(worker), nodeObject(worker), noPlant)
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), true, true, false)
		require.Nil(t, ce)
		assert.Equal(t, []bool{true, false}, s.writes())
	})
	t.Run("stop-drain with apply alone", func(t *testing.T) {
		s := newNodeServer(t, "draining", nodeObject(draining, map[string]any{"type": "Drained", "status": "Unknown", "lastUpdateTime": "t0"}), nodeObject(draining), noPlant)
		_, ce := runNodeCommand(t, s, "node stop-drain", noWait("worker-1"), true, false, false)
		require.Nil(t, ce)
		assert.Equal(t, []bool{true, false}, s.writes())
	})
	viaNorman := func(t *testing.T, state, action string, before, after map[string]any) *nodeServer {
		t.Helper()
		s := newNodeServer(t, state, before, after, noPlant)
		s.norman = action
		return s
	}
	t.Run("norman drain without yes", func(t *testing.T) {
		s := viaNorman(t, "active", "drain", nodeObject(worker), nodeObject(worker))
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), true, false, false)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeConfirmationRequired, ce.Code)
		assert.Empty(t, s.actions(), "no action is sent")
		assert.Empty(t, s.writes())
	})
	t.Run("norman drain with yes", func(t *testing.T) {
		s := viaNorman(t, "active", "drain", nodeObject(worker), nodeObject(worker))
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), true, true, false)
		require.Nil(t, ce)
		assert.Equal(t, []string{"/v3/nodes/" + scrubCluster + ":m-1?action=drain"}, s.actions())
		assert.Empty(t, s.writes())
	})
	t.Run("norman stop-drain with apply alone", func(t *testing.T) {
		s := viaNorman(t, "draining", "stopDrain", nodeObject(draining, map[string]any{"type": "Drained", "status": "Unknown", "lastUpdateTime": "t0"}), nodeObject(draining))
		_, ce := runNodeCommand(t, s, "node stop-drain", noWait("worker-1"), true, false, false)
		require.Nil(t, ce)
		assert.Equal(t, []string{"/v3/nodes/" + scrubCluster + ":m-1?action=stopDrain"}, s.actions())
	})
	t.Run("a norman preview sends no action", func(t *testing.T) {
		s := viaNorman(t, "active", "drain", nodeObject(worker), nodeObject(worker))
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), false, false, true)
		require.Nil(t, ce)
		assert.Empty(t, s.actions())
	})
	t.Run("a preview writes only the dry run", func(t *testing.T) {
		s := newNodeServer(t, "active", nodeObject(worker), nodeObject(worker), noPlant)
		_, ce := runNodeCommand(t, s, "node drain", noWait("worker-1"), false, false, true)
		require.Nil(t, ce)
		assert.Equal(t, []bool{true}, s.writes())
	})
}
