package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testTarget = target{node: "worker-1", cluster: "prod", id: "c-1", mid: "m-1"}

// waiting is a wait whose Drained condition read before the write was False
// at time t0.
func waiting(action kinds.NodeDrainAction) *nodeWait {
	return &nodeWait{action: action, t: testTarget, timeoutText: "3m",
		planned: drainedCondition{present: true, status: "False", lastUpdate: "t0"}}
}

// observed runs one read of the management node through the wait's decision.
func observed(t *testing.T, w *nodeWait, request string, cond map[string]any) (engine.WaitOutcome, error) {
	t.Helper()
	spec := map[string]any{}
	if request != "" {
		spec["desiredNodeUnschedulable"] = request
	}
	var conds []map[string]any
	if cond != nil {
		conds = append(conds, cond)
	}
	obj := managementNode("m-1", "worker-1", "9", spec, conds...)
	out, err := w.observe(t.Context(), unstr(obj), engine.SourceGet)
	if errors.Is(err, errWaitStopped) {
		err = w.stopError()
	}
	return out, err
}

type waitCell int

const (
	cellContinue waitCell = iota
	cellDone
	cellError
)

func cellOf(out engine.WaitOutcome, err error) waitCell {
	switch {
	case err != nil:
		return cellError
	case out == engine.WaitDone:
		return cellDone
	}
	return cellContinue
}

// drainedStates are the conditions every table covers: absent, Unknown, True,
// False as read before the write and False updated after it.
var drainedStates = []struct {
	name string
	cond map[string]any
}{
	{"absent", nil},
	{"Unknown", drainedCond("Unknown", "", "t1")},
	{"True", drainedCond("True", "", "t1")},
	{"False read before the write", drainedCond("False", "old failure", "t0")},
	{"False updated after the write", drainedCond("False", "new failure", "t1")},
}

// TestDrainWaitEnds asserts the end of a drain wait for every pair of the
// request on the management node and the Drained condition, cell by cell.
func TestDrainWaitEnds(t *testing.T) {
	t.Parallel()
	want := map[string][]waitCell{
		//                     absent        Unknown       True          False before  False after
		"drain":     {cellContinue, cellContinue, cellContinue, cellContinue, cellError},
		"":          {cellContinue, cellContinue, cellDone, cellContinue, cellError},
		"stopDrain": {cellError, cellError, cellError, cellError, cellError},
		"true":      {cellError, cellError, cellError, cellError, cellError},
		"false":     {cellError, cellError, cellError, cellError, cellError},
	}
	for request, row := range want {
		for i, state := range drainedStates {
			t.Run(fmt.Sprintf("request %q Drained %s", request, state.name), func(t *testing.T) {
				t.Parallel()
				out, err := observed(t, waiting(kinds.NodeDrain), request, state.cond)
				assert.Equal(t, row[i], cellOf(out, err), "outcome %v, error %v", out, err)
				if err != nil {
					ce := clierr.From(err)
					assert.Equal(t, clierr.CodeGeneric, ce.Code)
					assert.False(t, ce.Retryable)
					details, ok := ce.Details.(clierr.CommittedContextDetails)
					require.True(t, ok)
					assert.True(t, details.Committed)
					assert.Equal(t, "node", details.ResourceKind)
					assert.Equal(t, "worker-1", details.ResourceName)
					assert.Equal(t, clierr.NewCommand("get management-node").Arg("m-1").FlagValue("--cluster", "prod").Field(), details.Recovery)
				}
			})
		}
	}
}

// TestDrainWaitEndTexts asserts the text and the hint of each error that ends
// a drain wait.
func TestDrainWaitEndTexts(t *testing.T) {
	t.Parallel()
	recovery := clierr.NewCommand("get management-node").Arg("m-1").FlagValue("--cluster", "prod").Field()
	_, err := observed(t, waiting(kinds.NodeDrain), "drain", drainedCond("False", "boom", "t1"))
	ce := clierr.From(err)
	assert.Equal(t, "Rancher could not drain node worker-1: boom; it retries until you stop it", ce.Message)
	assert.Equal(t, clierr.NewCommand("node stop-drain").Arg("worker-1").FlagValue("--cluster", "prod").Flag("--dry-run").Field(), ce.Hint)

	_, err = observed(t, waiting(kinds.NodeDrain), "", drainedCond("False", "boom", "t1"))
	ce = clierr.From(err)
	assert.Equal(t, "drain of node worker-1 failed: boom; the node stays cordoned", ce.Message)

	for _, request := range []string{"stopDrain", "true", "false"} {
		_, err = observed(t, waiting(kinds.NodeDrain), request, nil)
		ce = clierr.From(err)
		assert.Equal(t, "another request changed node worker-1 while it was draining: desiredNodeUnschedulable is "+request, ce.Message)
		assert.Equal(t, recovery, ce.Hint)
	}
}

// TestDrainWaitWithNoConditionReadBeforeTheWrite asserts any False condition
// counts as updated after the write when none was read before it.
func TestDrainWaitWithNoConditionReadBeforeTheWrite(t *testing.T) {
	t.Parallel()
	w := waiting(kinds.NodeDrain)
	w.planned = drainedCondition{}
	_, err := observed(t, w, "", drainedCond("False", "boom", "t0"))
	require.Error(t, err)
	_, err = observed(t, w, "drain", drainedCond("False", "boom", "t0"))
	require.Error(t, err)
}

// TestStopDrainWaitEnds asserts the end of a stop-drain wait for every pair of
// the request and the Drained condition, cell by cell.
func TestStopDrainWaitEnds(t *testing.T) {
	t.Parallel()
	want := map[string][]waitCell{
		"stopDrain": {cellContinue, cellContinue, cellContinue, cellContinue, cellContinue},
		"":          {cellDone, cellError, cellError, cellError, cellError},
		"drain":     {cellError, cellError, cellError, cellError, cellError},
		"true":      {cellError, cellError, cellError, cellError, cellError},
		"false":     {cellError, cellError, cellError, cellError, cellError},
	}
	texts := map[string]string{
		"":      "a new drain of node worker-1 started after the stop",
		"drain": "a drain of node worker-1 started again",
		"true":  "another request changed node worker-1: desiredNodeUnschedulable is true",
		"false": "another request changed node worker-1: desiredNodeUnschedulable is false",
	}
	for request, row := range want {
		for i, state := range drainedStates {
			t.Run(fmt.Sprintf("request %q Drained %s", request, state.name), func(t *testing.T) {
				t.Parallel()
				out, err := observed(t, waiting(kinds.NodeStopDrain), request, state.cond)
				assert.Equal(t, row[i], cellOf(out, err), "outcome %v, error %v", out, err)
				if err != nil {
					assert.Equal(t, texts[request], clierr.From(err).Message)
					assert.False(t, clierr.From(err).Retryable)
				}
			})
		}
	}
}

// TestFailedDrainHint asserts the preview command a failed drain hints: the
// node and --cluster as typed, every typed option, the flag the failure
// message names when it was not typed, each flag once, and for a drain that
// did not complete in time a --drain-timeout of twice the time used, up to
// 3h, left as typed when doubling gives no increase.
func TestFailedDrainHint(t *testing.T) {
	t.Parallel()
	cmd := func(flags ...string) string {
		c := clierr.NewCommand("node drain").Arg("worker-1").FlagValue("--cluster", "prod")
		for _, f := range flags {
			name, value, valued := strings.Cut(f, "=")
			if valued {
				c = c.FlagValue(clierr.CommandWord(name), value)
				continue
			}
			c = c.Flag(clierr.CommandWord(name))
		}
		return c.Flag("--dry-run").Field()
	}
	hint := func(t *testing.T, f drainFlags, message string) string {
		t.Helper()
		w := waiting(kinds.NodeDrain)
		w.flags = f
		_, err := observed(t, w, "", drainedCond("False", message, "t1"))
		return clierr.From(err).Hint
	}
	noController := "cannot delete Pods that declare no controller (use --force to override): apps/bare"
	local := "cannot delete Pods with local storage (use --delete-emptydir-data to override): apps/cache"
	timeout := func(text string, d time.Duration) drainFlags {
		return drainFlags{drainTimeoutText: text, drainTimeout: d}
	}
	tests := []struct {
		name    string
		flags   drainFlags
		message string
		want    string
	}{
		{"force named", drainFlags{drainTimeout: 2 * time.Minute}, noController, cmd("--force")},
		{"emptyDir named", drainFlags{drainTimeout: 2 * time.Minute}, local, cmd("--delete-emptydir-data")},
		{"both named", drainFlags{drainTimeout: 2 * time.Minute}, noController + "; " + local, cmd("--force", "--delete-emptydir-data")},
		{"both named and typed", drainFlags{force: true, deleteEmptyDir: true, drainTimeout: 2 * time.Minute}, noController + "; " + local, cmd("--force", "--delete-emptydir-data")},
		{"typed options are kept", drainFlags{force: true, deleteEmptyDir: true, gracePeriodText: "30", drainTimeoutText: "5m", drainTimeout: 5 * time.Minute}, "boom",
			cmd("--force", "--delete-emptydir-data", "--grace-period=30", "--drain-timeout=5m")},
		{"another message adds nothing", drainFlags{drainTimeout: 2 * time.Minute}, "boom", cmd()},
		{"timeout doubles the default", drainFlags{drainTimeout: 2 * time.Minute}, "Drain failed: drain did not complete within 120s", cmd("--drain-timeout=4m")},
		{"timeout doubles 1h", timeout("1h", time.Hour), "Drain failed: drain did not complete within 3600s", cmd("--drain-timeout=2h")},
		{"timeout doubles 2h and is capped", timeout("2h", 2*time.Hour), "Drain failed: drain did not complete within 7200s", cmd("--drain-timeout=3h")},
		{"timeout 90m doubles to the cap", timeout("90m", 90*time.Minute), "Drain failed: drain did not complete within 5400s", cmd("--drain-timeout=3h")},
		{"timeout at the cap is left as typed", timeout("3h", 3*time.Hour), "Drain failed: drain did not complete within 10800s", cmd("--drain-timeout=3h")},
		{"timeout at the cap typed in seconds is left as typed", timeout("10800s", 3*time.Hour), "Drain failed: drain did not complete within 10800s", cmd("--drain-timeout=10800s")},
		{"typed timeout is kept when the message is no timeout", timeout("10m", 10*time.Minute), "boom", cmd("--drain-timeout=10m")},
		{"timeout and a flag in one message", drainFlags{force: true, drainTimeout: 2 * time.Minute}, "did not complete within 120s; use --force", cmd("--force", "--drain-timeout=4m")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, hint(t, tc.flags, tc.message))
		})
	}
}

// TestFailedDrainReadsThePodDisruptionBudgets asserts a drain that did not
// complete in time, after a preview that listed pods to evict, names the
// commands that read the PodDisruptionBudgets of their namespaces: at most
// five, then how many are left, and none when no pod was to be evicted.
func TestFailedDrainReadsThePodDisruptionBudgets(t *testing.T) {
	t.Parallel()
	namespaces := func(n int) []string {
		var out []string
		for i := 1; i <= n; i++ {
			out = append(out, fmt.Sprintf("ns%d", i))
		}
		return out
	}
	pdb := func(ns string) string {
		return clierr.NewCommand("get poddisruptionbudgets").FlagValue("--cluster", "prod").FlagValue("--namespace", ns).Field()
	}
	prose := "drain of node worker-1 failed: Drain failed: drain did not complete within 120s; the node stays cordoned"
	lead := "; read the PodDisruptionBudgets that can block it with "
	tests := []struct {
		name string
		n    int
		want string
	}{
		{"none", 0, prose},
		{"one", 1, prose + lead + pdb("ns1")},
		{"five", 5, prose + lead + strings.Join([]string{pdb("ns1"), pdb("ns2"), pdb("ns3"), pdb("ns4"), pdb("ns5")}, "; ")},
		{"six", 6, prose + lead + strings.Join([]string{pdb("ns1"), pdb("ns2"), pdb("ns3"), pdb("ns4"), pdb("ns5")}, "; ") + "; and 1 more namespace"},
		{"seven", 7, prose + lead + strings.Join([]string{pdb("ns1"), pdb("ns2"), pdb("ns3"), pdb("ns4"), pdb("ns5")}, "; ") + "; and 2 more namespaces"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := waiting(kinds.NodeDrain)
			w.flags = drainFlags{drainTimeout: 2 * time.Minute}
			w.evictedNamespaces = namespaces(tc.n)
			_, err := observed(t, w, "", drainedCond("False", "Drain failed: drain did not complete within 120s", "t1"))
			assert.Equal(t, tc.want, clierr.From(err).Message)
		})
	}
	w := waiting(kinds.NodeDrain)
	w.evictedNamespaces = namespaces(2)
	_, err := observed(t, w, "", drainedCond("False", "boom", "t1"))
	assert.Equal(t, "drain of node worker-1 failed: boom; the node stays cordoned", clierr.From(err).Message, "only a timeout names the PodDisruptionBudget commands")
}

// drainAndWait plans node drain and runs it against the server with the
// wait on.
func drainAndWait(t *testing.T, f *fakeRancher, flags ...string) (string, map[string]any, []string, error) {
	t.Helper()
	p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", flags...))
	require.NoError(t, err)
	res, err := p.exec(t.Context())
	if err != nil {
		return "", nil, nil, err
	}
	data, _ := res.Data.(map[string]any)
	return res.Text, data, res.Warnings, nil
}

// TestDrainExecWaitsForTheDrainedCondition asserts a drain that Rancher
// finishes ends with the drained text, with node, managementNode, waited,
// drained and the four fields of the Drained condition in the data.
func TestDrainExecWaitsForTheDrainedCondition(t *testing.T) {
	fastWait(t)
	f := idleNode(t)
	running := managementNode("m-1", "worker-1", "8", workerSpec("desiredNodeUnschedulable", "drain"), drainedCond("Unknown", "", "t1"))
	done := managementNode("m-1", "worker-1", "9", workerSpec(), map[string]any{"type": "Drained", "status": "True", "reason": "Drained", "message": "node drained", "lastUpdateTime": "t2", "extra": "x"})
	f.reads = []map[string]any{running, done}
	text, data, warnings, err := drainAndWait(t, f)
	require.NoError(t, err)
	assert.Equal(t, "node/worker-1 drained", strings.Split(text, "\n")[0])
	assert.Empty(t, warnings)
	assert.Equal(t, map[string]any{
		"node": "worker-1", "managementNode": "c-1/m-1", "waited": true, "drained": true,
		"condition": map[string]any{"type": "Drained", "status": "True", "reason": "Drained", "message": "node drained"},
	}, data)
}

// TestDrainExecWaitErrorsAreCommittedAndNotRetryable asserts every way the
// wait ends without success is GENERIC, not retryable, with the committed
// details and the command that reads the management node: a failed drain, a
// removed node, a timeout with its last state, and a drain request that
// disappeared.
func TestDrainExecWaitErrorsAreCommittedAndNotRetryable(t *testing.T) {
	fastWait(t)
	recovery := clierr.NewCommand("get management-node").Arg("m-1").FlagValue("--cluster", "prod").Field()
	check := func(t *testing.T, err error, message string) {
		t.Helper()
		ce := clierr.From(err)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.True(t, strings.HasPrefix(ce.Message, message), ce.Message)
		details, ok := ce.Details.(clierr.CommittedContextDetails)
		require.True(t, ok)
		assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: "node", ResourceName: "worker-1", Recovery: recovery}, details)
	}
	t.Run("failed drain", func(t *testing.T) {
		f := idleNode(t)
		f.reads = []map[string]any{managementNode("m-1", "worker-1", "9", workerSpec(), drainedCond("False", "cannot delete Pods that declare no controller (use --force to override): apps/bare", "t9"))}
		_, _, _, err := drainAndWait(t, f)
		check(t, err, "drain of node worker-1 failed: cannot delete Pods that declare no controller")
		assert.Equal(t, clierr.NewCommand("node drain").Arg("worker-1").FlagValue("--cluster", "prod").Flag("--force").Flag("--dry-run").Field(), clierr.From(err).Hint)
	})
	t.Run("the node is removed", func(t *testing.T) {
		f := idleNode(t)
		f.reads = nil
		_, _, _, err := drainAndWait(t, f)
		check(t, err, "node worker-1 was removed while it was draining")
		assert.Equal(t, recovery, clierr.From(err).Hint)
	})
	t.Run("timeout", func(t *testing.T) {
		f := idleNode(t)
		f.reads = []map[string]any{managementNode("m-1", "worker-1", "9", workerSpec("desiredNodeUnschedulable", "drain"), drainedCond("Unknown", "starting", "t9"))}
		_, _, _, err := drainAndWait(t, f, "--timeout=60ms")
		check(t, err, "timed out after 60ms waiting for node worker-1 to drain; the drain was requested and goes on in Rancher; last state: desiredNodeUnschedulable drain, Drained Unknown (starting)")
		assert.Equal(t, recovery, clierr.From(err).Hint)
	})
	t.Run("the request was removed", func(t *testing.T) {
		f := idleNode(t)
		f.reads = []map[string]any{managementNode("m-1", "worker-1", "9", workerSpec())}
		_, _, _, err := drainAndWait(t, f, "--timeout=60ms")
		check(t, err, "the drain request of node worker-1 was removed and Rancher reported neither done nor failed: a stop-drain, an uncordon, or Rancher's node sync removed it")
	})
}

// TestDrainExecWaitErrorsKeepTheLastError asserts a timeout after reads that
// failed names the last error.
func TestDrainExecWaitErrorsKeepTheLastError(t *testing.T) {
	fastWait(t)
	f := idleNode(t)
	f.readStatus = 503
	_, _, _, err := drainAndWait(t, f, "--timeout=80ms")
	require.Error(t, err)
	ce := clierr.From(err)
	assert.Contains(t, ce.Message, "timed out after 80ms waiting for node worker-1 to drain; the drain was requested and goes on in Rancher; last state: <no state observed>; last error: ")
	assert.False(t, ce.Retryable)
}

// TestDrainExecAReadRefusedAfterTheWriteEndsWithAWarning asserts a 403 on the
// reads after the write ends the command with exit 0, the no-wait text, the
// data with waited false and the warning.
func TestDrainExecAReadRefusedAfterTheWriteEndsWithAWarning(t *testing.T) {
	fastWait(t)
	f := idleNode(t)
	f.readStatus = 403
	text, data, warnings, err := drainAndWait(t, f)
	require.NoError(t, err)
	assert.Equal(t, "node/worker-1 drain requested", strings.Split(text, "\n")[0])
	assert.Equal(t, false, data["waited"])
	assert.Equal(t, []string{"your access to node worker-1 ended after the drain request; the drain was requested and not confirmed"}, warnings)

	g := idleNode(t)
	obj := managementNode("m-1", "worker-1", "7", workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", map[string]any{"unschedulable": true}), drainedCond("Unknown", "", "t0"))
	g.labeled = []map[string]any{obj}
	g.steve = steveView(obj, "draining", "")
	g.readStatus = 403
	p, err := plan(t, newStopCmd(g), newInput("worker-1"))
	require.NoError(t, err)
	res, err := p.exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "node/worker-1 stop requested", strings.Split(res.Text, "\n")[0])
	assert.Equal(t, []string{"your access to node worker-1 ended after the stop request; the stop was requested and not confirmed"}, res.Warnings)
}

// TestStopDrainExecWaitsUntilTheDrainIsGone asserts a stop that Rancher
// finishes ends with the stopped text and data without a drained field, and
// that the other ends use the stop wording.
func TestStopDrainExecWaitsUntilTheDrainIsGone(t *testing.T) {
	fastWait(t)
	draining := managementNode("m-1", "worker-1", "7", workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", map[string]any{"unschedulable": true}), drainedCond("Unknown", "", "t0"))
	run := func(t *testing.T, reads []map[string]any, status int, flags ...string) (string, map[string]any, error) {
		t.Helper()
		f := newFakeRancher(t)
		f.labeled = []map[string]any{draining}
		f.steve = steveView(draining, "draining", "")
		f.reads, f.readStatus = reads, status
		p, err := plan(t, newStopCmd(f), newInput("worker-1", flags...))
		require.NoError(t, err)
		res, err := p.exec(t.Context())
		data, _ := res.Data.(map[string]any)
		return res.Text, data, err
	}
	t.Run("stopped", func(t *testing.T) {
		stopping := managementNode("m-1", "worker-1", "8", workerSpec("desiredNodeUnschedulable", "stopDrain", "internalNodeSpec", map[string]any{"unschedulable": true}), drainedCond("Unknown", "", "t0"))
		gone := managementNode("m-1", "worker-1", "9", workerSpec("internalNodeSpec", map[string]any{"unschedulable": true}))
		text, data, err := run(t, []map[string]any{stopping, gone}, 0)
		require.NoError(t, err)
		assert.Equal(t, "node/worker-1 drain stopped; the node stays cordoned", strings.Split(text, "\n")[0])
		assert.Equal(t, map[string]any{"node": "worker-1", "managementNode": "c-1/m-1", "waited": true}, data)
	})
	t.Run("a new drain started", func(t *testing.T) {
		_, _, err := run(t, []map[string]any{managementNode("m-1", "worker-1", "9", workerSpec(), drainedCond("Unknown", "", "t5"))}, 0)
		require.ErrorContains(t, err, "a new drain of node worker-1 started after the stop")
	})
	t.Run("removed", func(t *testing.T) {
		_, _, err := run(t, nil, 0)
		require.ErrorContains(t, err, "node worker-1 was removed while its drain was stopping")
	})
	t.Run("timeout", func(t *testing.T) {
		stopping := managementNode("m-1", "worker-1", "8", workerSpec("desiredNodeUnschedulable", "stopDrain"))
		_, _, err := run(t, []map[string]any{stopping}, 0, "--timeout=60ms")
		require.ErrorContains(t, err, "timed out after 60ms waiting for the drain of node worker-1 to stop; the stop was requested; last state: desiredNodeUnschedulable stopDrain, Drained absent")
	})
}

// TestDrainExecCancelledWaitIsCommitted asserts an interrupt during the wait,
// through the wait's own cancel handling, ends it with the message "stopped
// waiting for node ..." and the committed details.
func TestDrainExecCancelledWaitIsCommitted(t *testing.T) {
	fastWait(t)
	row, ok := kinds.ByKey("managementnode")
	require.True(t, ok)
	for _, tc := range []struct {
		action  kinds.NodeDrainAction
		request string
		want    string
	}{
		{kinds.NodeDrain, "drain", "stopped waiting for node worker-1 to drain; the drain was requested"},
		{kinds.NodeStopDrain, "stopDrain", "stopped waiting for the drain of node worker-1 to stop; the stop was requested"},
	} {
		f := idleNode(t)
		f.reads = []map[string]any{managementNode("m-1", "worker-1", "9", workerSpec("desiredNodeUnschedulable", tc.request), drainedCond("Unknown", "", "t9"))}
		client, err := testFactory{f: f}.Cluster(t.Context(), "local")
		require.NoError(t, err)
		w := waiting(tc.action)
		w.client, w.gvr, w.timeout = client, row.GVR, time.Minute
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(40*time.Millisecond, cancel)
		_, err = w.run(ctx)
		ce := clierr.From(err)
		require.NotNil(t, ce)
		assert.Equal(t, tc.want, ce.Message)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		details, ok := ce.Details.(clierr.CommittedContextDetails)
		require.True(t, ok)
		assert.True(t, details.Committed)
		assert.Equal(t, "worker-1", details.ResourceName)
		cancel()
	}
}

// TestDrainDefaultTimeoutPrintsAsHelpShowsIt asserts a default --timeout is
// written in the flags' own format, and a typed one as typed.
func TestDrainDefaultTimeoutPrintsAsHelpShowsIt(t *testing.T) {
	t.Parallel()
	f, err := parseDrainFlags(newInput("worker-1"))
	require.NoError(t, err)
	assert.Equal(t, "3m", f.timeoutText)
	assert.Equal(t, 3*time.Minute, f.timeout)
	f, err = parseDrainFlags(newInput("worker-1", "--drain-timeout=1h30m"))
	require.NoError(t, err)
	assert.Equal(t, "1h31m", f.timeoutText)
	f, err = parseDrainFlags(newInput("worker-1", "--timeout=90s"))
	require.NoError(t, err)
	assert.Equal(t, "90s", f.timeoutText)
	f, err = parseDrainFlags(newInput("worker-1", "--wait=false"))
	require.NoError(t, err)
	assert.False(t, f.wait)
	s, err := parseWait(newInput("worker-1"), defaultStopTimeout)
	require.NoError(t, err)
	assert.Equal(t, "2m", s.timeoutText)
}

// TestAbortedErrorKeepsItsHintAndSaysWhenTheRecoveryCommandIsLeftOut asserts
// an authentication or certificate failure during the wait keeps its code and
// hint, and its message says the recovery command is left out when the typed
// --cluster value cannot be printed in a command.
func TestAbortedErrorKeepsItsHintAndSaysWhenTheRecoveryCommandIsLeftOut(t *testing.T) {
	t.Parallel()
	cause := clierr.New(clierr.CodeAuth, "the server rejected the credential").WithHint("rancher auth status")
	w := waiting(kinds.NodeDrain)
	ce := w.abortedError(cause)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, "rancher auth status", ce.Hint)
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
	assert.False(t, ce.Retryable)

	w = waiting(kinds.NodeDrain)
	w.t.cluster = `a"b`
	ce = w.abortedError(cause)
	assert.Equal(t, "rancher auth status", ce.Hint)
	assert.Contains(t, ce.Message, clierr.LeftOutNote)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.Equal(t, "worker-1", details.ResourceName)
}
