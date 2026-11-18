package nodes

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDrainCmd(f *fakeRancher, noLister bool) runtime.Command {
	return NewDrain(testFactory{f: f, noLister: noLister}, testRoots{f: f})
}

func newStopCmd(f *fakeRancher) runtime.Command {
	return NewStopDrain(testFactory{f: f}, testRoots{f: f})
}

func requireCLIError(t *testing.T, err error) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	ce := clierr.From(err)
	require.NotNil(t, ce)
	return ce
}

// TestDrainDescribe asserts the descriptor of both commands: mutating, text
// shaped, destructive only for drain, with --cluster from the shared flag and
// the wait flags.
func TestDrainDescribe(t *testing.T) {
	t.Parallel()
	d := newDrainCmd(nil, false).Describe()
	assert.Equal(t, "node drain", d.Name)
	assert.Equal(t, runtime.Mutating, d.CommandType)
	assert.Equal(t, runtime.ShapeText, d.Shape)
	assert.Equal(t, "true", d.Destructive)
	assert.True(t, d.Auth)
	var names []string
	for _, f := range d.Flags {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"cluster", "force", "delete-emptydir-data", "grace-period", "drain-timeout", "wait", "timeout"}, names)
	assert.True(t, d.Flags[0].Required)

	s := newStopCmd(nil).Describe()
	assert.Equal(t, "node stop-drain", s.Name)
	assert.Equal(t, "false", s.Destructive)
	names = nil
	for _, f := range s.Flags {
		names = append(names, f.Name)
	}
	assert.Equal(t, []string{"cluster", "wait", "timeout"}, names)
}

// TestDrainPlanDisplayIdentity asserts the Effect of node drain names the
// node by its downstream name (DisplayKind node, DisplayName the node name,
// Target the management node) and a committed result names the node.
func TestDrainPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := idleNode(t)
	p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--wait=false"))
	require.NoError(t, err)
	assert.Equal(t, "node", p.eff.DisplayKind)
	assert.Equal(t, "worker-1", p.eff.DisplayName)
	assert.Equal(t, "nodes/m-1", p.eff.Target)
	assert.Equal(t, "c-1", p.eff.Cluster)
	assert.Equal(t, safety.PreviewServerDryRun, p.eff.PreviewSource)
	assert.Equal(t, "7", p.eff.ResourceVersion)
	assert.True(t, p.eff.ConflictDetection)
	assert.True(t, p.eff.Destructive)
	assert.False(t, p.eff.Reversible)

	res, err := p.exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "node/worker-1 drain requested", strings.Split(res.Text, "\n")[0])
	assert.Equal(t, map[string]any{"node": "worker-1", "managementNode": "c-1/m-1", "waited": false}, res.Data)
}

// TestDrainBodyCarriesTheFiveFields asserts the dry-run write has the drain
// request and all five options with Rancher's defaults when no flag is typed
// and with each typed value, the listed resourceVersion as the precondition
// and the field manager.
func TestDrainBodyCarriesTheFiveFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flags []string
		want  map[string]any
	}{
		{"no flag", nil, map[string]any{"force": false, "ignoreDaemonSets": true, "deleteLocalData": false, "gracePeriod": float64(-1), "timeout": float64(120)}},
		{"every flag", []string{"--force", "--delete-emptydir-data", "--grace-period=30", "--drain-timeout=5m"},
			map[string]any{"force": true, "ignoreDaemonSets": true, "deleteLocalData": true, "gracePeriod": float64(30), "timeout": float64(300)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := idleNode(t)
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1", tc.flags...))
			require.NoError(t, err)
			puts := f.writes()
			require.Len(t, puts, 1)
			assert.True(t, puts[0].dryRun)
			q, err := url.ParseQuery(puts[0].query)
			require.NoError(t, err)
			assert.Equal(t, "cli.rancher.io", q.Get("fieldManager"))
			spec := puts[0].body["spec"].(map[string]any)
			assert.Equal(t, "drain", spec["desiredNodeUnschedulable"])
			assert.Equal(t, tc.want, spec["nodeDrainInput"])
			assert.Equal(t, "7", puts[0].body["metadata"].(map[string]any)["resourceVersion"])
		})
	}
}

func diffPaths(eff safety.Effect) []string {
	var paths []string
	for _, d := range eff.Diff {
		paths = append(paths, d.Path)
	}
	return paths
}

// TestDrainPlanDiffNamesTheTwoFields asserts the Effect's diff lists the
// request and the options.
func TestDrainPlanDiffNamesTheTwoFields(t *testing.T) {
	t.Parallel()
	f := idleNode(t)
	p, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
	require.NoError(t, err)
	assert.Contains(t, diffPaths(p.eff), "/spec/desiredNodeUnschedulable")
	for _, path := range diffPaths(p.eff) {
		assert.True(t, strings.HasPrefix(path, "/spec/desiredNodeUnschedulable") || strings.HasPrefix(path, "/spec/nodeDrainInput"), path)
	}
}

// TestDrainFlagRefusals asserts each bad flag value is a VALIDATION error
// with the decided text before any request.
func TestDrainFlagRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flags []string
		want  string
	}{
		{[]string{"--grace-period=0"}, `--grace-period "0" must be a whole number of seconds, 1 or more`},
		{[]string{"--grace-period=abc"}, `--grace-period "abc" must be a whole number of seconds, 1 or more`},
		{[]string{"--grace-period=-5"}, `--grace-period "-5" must be a whole number of seconds, 1 or more`},
		{[]string{"--drain-timeout=4h"}, `--drain-timeout "4h" must be whole seconds from 1s to 3h, e.g. 5m`},
		{[]string{"--drain-timeout=500ms"}, `--drain-timeout "500ms" must be whole seconds from 1s to 3h, e.g. 5m`},
		{[]string{"--drain-timeout=1.5s"}, `--drain-timeout "1.5s" must be whole seconds from 1s to 3h, e.g. 5m`},
		{[]string{"--drain-timeout=soon"}, `--drain-timeout "soon" must be whole seconds from 1s to 3h, e.g. 5m`},
		{[]string{"--wait=false", "--timeout=1m"}, `--wait=false conflicts with --timeout; remove one`},
		{[]string{"--timeout=-1m"}, `--timeout "-1m" must be a positive duration, e.g. 10m`},
		{[]string{"--timeout=later"}, `--timeout "later" is not a valid duration, e.g. 10m`},
	} {
		f := idleNode(t)
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1", tc.flags...))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code, tc.flags)
		assert.Equal(t, tc.want, ce.Message)
		assert.Empty(t, f.seen(), "no request before the flags are valid")
	}
	f := idleNode(t)
	_, err := plan(t, newStopCmd(f), newInput("worker-1", "--wait=false", "--timeout=1m"))
	require.ErrorContains(t, err, "--wait=false conflicts with --timeout; remove one")
	assert.Empty(t, f.seen())
}

// TestDrainPlanLookup asserts how the management node is found: by the node
// name label first, among the whole namespace when the label finds none, with
// no label for a name that is not a valid label value, and with the decided
// errors for none, two, and a refused list.
func TestDrainPlanLookup(t *testing.T) {
	t.Parallel()
	t.Run("label hit sends the label and reads once", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err)
		var lists []string
		for _, r := range f.seen() {
			if strings.Contains(r, "/namespaces/c-1/nodes?") {
				lists = append(lists, r)
			}
		}
		require.Len(t, lists, 1)
		assert.Contains(t, lists[0], "labelSelector=management.cattle.io%2Fnodename%3Dworker-1")
	})
	t.Run("label miss falls back to the namespace", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.plain, f.labeled = f.labeled, nil
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err)
		lists := 0
		for _, r := range f.seen() {
			if strings.Contains(r, "/namespaces/c-1/nodes?") {
				lists++
			}
		}
		assert.Equal(t, 2, lists)
	})
	t.Run("a name that is no label value is listed without a label", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("a", 64)
		f := newFakeRancher(t)
		obj := managementNode("m-1", long, "7", workerSpec())
		f.plain = []map[string]any{obj}
		f.steve = steveView(obj, "active", "")
		_, err := plan(t, newDrainCmd(f, false), newInput(long))
		require.NoError(t, err)
		for _, r := range f.seen() {
			assert.NotContains(t, r, "labelSelector")
		}
	})
	t.Run("an item of another node is not a match", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.labeled = []map[string]any{managementNode("m-9", "worker-9", "3", workerSpec())}
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
	})
	t.Run("none found", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.labeled, f.plain = nil, nil
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, "cluster c-1 has no node worker-1 that Rancher manages", ce.Message)
		assert.Equal(t, clierr.NewCommand("get node").FlagValue("--cluster", "prod").Field(), ce.Hint)
	})
	t.Run("two found", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.labeled = append(f.labeled, managementNode("m-2", "worker-1", "3", workerSpec()))
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, "cluster c-1 has 2 management nodes for node worker-1", ce.Message)
		assert.Equal(t, []clierr.Candidate{{ID: "m-1", Name: "worker-1"}, {ID: "m-2", Name: "worker-1"}}, ce.Candidates)
	})
	t.Run("the list is refused", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.listStatus = 403
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, "you cannot list the management nodes of cluster c-1 (refused with HTTP 403); this command needs access to the local cluster", ce.Message)
		status, ok := clierr.HTTPStatus(err)
		assert.True(t, ok)
		assert.Equal(t, 403, status)
	})
	t.Run("another list error keeps its text", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.listStatus = 503
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.NotEqual(t, clierr.CodeRBAC, ce.Code)
	})
	t.Run("the node argument is required", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		_, err := plan(t, newDrainCmd(f, false), newInput(""))
		require.ErrorContains(t, err, "argument <node> is required")
	})
}

// TestDrainPlanSteveReads asserts the errors of the read of the management
// node through the Steve API: a refused read names the path, a missing object or a
// resourceVersion that differs from the listed one is a retryable CONFLICT,
// and a body that is not an object is a GENERIC error that is not retryable.
func TestDrainPlanSteveReads(t *testing.T) {
	t.Parallel()
	t.Run("403", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.steveStatus = 403
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, "you cannot read management node m-1 of cluster c-1 (GET /v1/management.cattle.io.nodes/c-1/m-1 was refused with HTTP 403)", ce.Message)
	})
	t.Run("404", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.steveStatus = 404
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "node worker-1 changed while it was read; run the command again", ce.Message)
	})
	t.Run("another resourceVersion", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.steve = steveView(managementNode("m-1", "worker-1", "8", workerSpec()), "active", "")
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		assert.True(t, ce.Retryable)
		assert.Equal(t, "node worker-1 changed while it was read; run the command again", ce.Message)
		assert.Empty(t, f.writes(), "nothing is written")
	})
	t.Run("a body that is not an object", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.steve = nil
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		assert.True(t, strings.HasPrefix(ce.Message, "could not read management node m-1 of cluster c-1: "), ce.Message)
	})
	t.Run("another error keeps its text", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.steveStatus = 503
		_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		ce := requireCLIError(t, err)
		assert.NotEqual(t, clierr.CodeRBAC, ce.Code)
		assert.NotEqual(t, clierr.CodeConflict, ce.Code)
	})
}

// TestDrainPlanStateRefusals asserts each state Rancher offers no drain or
// stop-drain action for is a VALIDATION error with the decided text, and
// that the node already draining names the stop-drain preview as the hint.
func TestDrainPlanStateRefusals(t *testing.T) {
	t.Parallel()
	cordoned := map[string]any{"unschedulable": true}
	tests := []struct {
		name       string
		spec       map[string]any
		conds      []map[string]any
		state, msg string
		stop       bool
		code, want string
		hint       string
		retryable  bool
	}{
		{name: "not a worker", spec: map[string]any{"controlPlane": true, "etcd": true}, state: "active",
			want: "Rancher offers drain only for worker nodes; node worker-1 has the roles etcd and controlPlane"},
		{name: "not a worker, stop", spec: map[string]any{"controlPlane": true}, state: "active", stop: true,
			want: "Rancher offers stop drain only for worker nodes; node worker-1 has the roles controlPlane"},
		{name: "unavailable", spec: workerSpec(), state: "unavailable", msg: "Kubelet stopped posting node status",
			want: "Rancher offers no action on node worker-1 while it is unavailable: Kubelet stopped posting node status; to drain it anyway, cordon it first by setting spec.unschedulable to true on the node: Rancher then shows it cordoned and offers drain"},
		{name: "unavailable, stop", spec: workerSpec(), state: "unavailable", stop: true,
			want: "Rancher offers no action on node worker-1 while it is unavailable"},
		{name: "unknown state", spec: workerSpec(), state: "",
			want: "Rancher offers no action on node worker-1 while it is unknown"},
		{name: "already draining", spec: workerSpec("desiredNodeUnschedulable", "drain", "internalNodeSpec", cordoned), conds: []map[string]any{drainedCond("Unknown", "", "t0")}, state: "draining",
			want: "node worker-1 is already draining",
			hint: clierr.NewCommand("node stop-drain").Arg("worker-1").FlagValue("--cluster", "prod").Flag("--dry-run").Field()},
		{name: "already drained", spec: workerSpec("internalNodeSpec", cordoned), conds: []map[string]any{drainedCond("True", "", "t0")}, state: "active",
			want: "node worker-1 is already drained"},
		{name: "condition left", spec: workerSpec(), conds: []map[string]any{drainedCond("True", "", "t0")}, state: "active",
			code: clierr.CodeConflict, retryable: true, want: "Rancher has not yet removed the Drained condition of node worker-1; run the command again"},
		{name: "stop, not draining", spec: workerSpec(), state: "active", stop: true, want: "node worker-1 is not draining"},
		{name: "stop, cordoned", spec: workerSpec("internalNodeSpec", cordoned), state: "active", stop: true, want: "node worker-1 is not draining"},
		{name: "stop, drained", spec: workerSpec("internalNodeSpec", cordoned), conds: []map[string]any{drainedCond("True", "", "t0")}, state: "active", stop: true,
			want: "node worker-1 is not draining; it is drained"},
		{name: "stop, drain requested and not started", spec: workerSpec("desiredNodeUnschedulable", "drain"), state: "active", stop: true,
			want: "a drain of node worker-1 is requested and has not started; Rancher offers no stop until it starts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRancher(t)
			obj := managementNode("m-1", "worker-1", "7", tc.spec, tc.conds...)
			f.labeled = []map[string]any{obj}
			f.steve = steveView(obj, tc.state, tc.msg)
			cmd := newDrainCmd(f, false)
			if tc.stop {
				cmd = newStopCmd(f)
			}
			_, err := plan(t, cmd, newInput("worker-1"))
			ce := requireCLIError(t, err)
			wantCode := tc.code
			if wantCode == "" {
				wantCode = clierr.CodeValidation
			}
			assert.Equal(t, wantCode, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Empty(t, f.writes(), "no write is sent for a refused state")
		})
	}
}

// TestDrainPlanTreatsANodeWithNoRoleAsAWorker asserts a management node whose
// etcd, controlPlane and worker are all false is a worker, as Rancher's node
// formatter reads it, so drain is allowed on it.
func TestDrainPlanTreatsANodeWithNoRoleAsAWorker(t *testing.T) {
	t.Parallel()
	f := newFakeRancher(t)
	obj := managementNode("m-1", "worker-1", "7", map[string]any{"internalNodeSpec": map[string]any{}})
	f.labeled = []map[string]any{obj}
	f.steve = steveView(obj, "active", "")
	_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
	require.NoError(t, err)
}

// TestDrainPlanPodWarnings asserts the Effect's warnings, in order: the three
// always present, the blockers, the pod list that was read or failed, the
// DaemonSet note, the local cluster note and a wait shorter than the drain.
func TestDrainPlanPodWarnings(t *testing.T) {
	t.Parallel()
	always := []string{
		"the server-side dry-run checks the request to Rancher; it does not run the drain. Rancher runs kubectl drain, which cordons the node and then evicts the pods listed here through the Eviction API, respecting PodDisruptionBudgets",
		"Rancher runs the drain as its system user for cluster c-1, not with your permissions",
		"stopping a drain leaves the node cordoned; evicted pods are not put back",
	}
	t.Run("pods that block and a DaemonSet pod", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.pods = []map[string]any{
			pod("bare", nil, nil, nil, "Running").Object,
			pod("cache", controlledBy("ReplicaSet"), emptyDirVolume, nil, "Running").Object,
			pod("proxy", controlledBy("DaemonSet"), nil, nil, "Running").Object,
			pod("web", controlledBy("ReplicaSet"), nil, nil, "Running").Object,
		}
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--timeout=1m"))
		require.NoError(t, err)
		assert.Equal(t, append(append([]string(nil), always...),
			"kubectl drain refuses this node before it evicts anything: Pods that declare no controller (use --force to override); Pods with local storage (use --delete-emptydir-data to override); Rancher then marks the drain failed and the node stays cordoned",
			"this pod list is read now; Rancher lists the pods again when the drain runs",
			"a DaemonSet pod whose DaemonSet is gone is evicted with --force and blocks the drain without it",
			"the wait ends before Rancher's drain timeout; the drain goes on",
		), p.eff.Warnings)
		got := map[string]string{}
		for _, o := range p.eff.Objects {
			got[o.Name] = o.Outcome
		}
		assert.Equal(t, map[string]string{
			"apps/bare": safety.OutcomeBlocksTheDrain, "apps/cache": safety.OutcomeBlocksTheDrain,
			"apps/proxy": safety.OutcomeSkipped, "apps/web": safety.OutcomeWillBeEvicted,
		}, got)
	})
	t.Run("force removes the DaemonSet note and the blocker", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.pods = []map[string]any{pod("bare", nil, nil, nil, "Running").Object, pod("proxy", controlledBy("DaemonSet"), nil, nil, "Running").Object}
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--force"))
		require.NoError(t, err)
		assert.Equal(t, append(append([]string(nil), always...), "this pod list is read now; Rancher lists the pods again when the drain runs"), p.eff.Warnings)
	})
	t.Run("the pod list is refused", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		f.podStatus = 403
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
		require.NoError(t, err, "a refused pod list is a warning")
		require.Len(t, p.eff.Warnings, 4)
		assert.True(t, strings.HasPrefix(p.eff.Warnings[3], "the preview cannot list the pods on node worker-1 (RBAC: "), p.eff.Warnings[3])
		assert.True(t, strings.HasSuffix(p.eff.Warnings[3], "); Rancher lists them as its system user, and kubectl drain's rules for pods still apply"), p.eff.Warnings[3])
		assert.Empty(t, p.eff.Objects)
	})
	t.Run("the cluster is local", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		in := newInput("worker-1")
		in.flags["cluster"] = "c-1"
		p, err := plan(t, newDrainCmd(f, false), in)
		require.NoError(t, err)
		assert.NotContains(t, p.eff.Warnings, "this node is in the cluster Rancher runs in; evicting Rancher's own pods can stop the drain and the API this command uses")
		flags := drainFlags{drainTimeout: 2 * time.Minute}
		flags.wait, flags.timeout = true, 3*time.Minute
		d := drainWarnings(target{node: "worker-1", id: "local", cluster: "local"}, flags, podPlan{}, nil)
		assert.Contains(t, d, "this node is in the cluster Rancher runs in; evicting Rancher's own pods can stop the drain and the API this command uses")
	})
	t.Run("no wait gives no timeout note", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--wait=false"))
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(p.eff.Warnings, "|"), "the wait ends before")
	})
	t.Run("a timeout equal to the drain timeout gives no note", func(t *testing.T) {
		t.Parallel()
		f := idleNode(t)
		p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--drain-timeout=5m", "--timeout=5m"))
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(p.eff.Warnings, "|"), "the wait ends before")
	})
}

// TestDrainPlanPodReadAsksForNodePodsInEveryNamespace asserts the pod read
// is one list across every namespace of the downstream cluster with the field
// selector of the node.
func TestDrainPlanPodReadAsksForNodePodsInEveryNamespace(t *testing.T) {
	t.Parallel()
	f := idleNode(t)
	_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
	require.NoError(t, err)
	var pods []string
	for _, r := range f.seen() {
		if strings.Contains(r, "/api/v1/pods") {
			pods = append(pods, r)
		}
	}
	require.Len(t, pods, 1)
	assert.Contains(t, pods[0], "/k8s/clusters/c-1/api/v1/pods?")
	assert.Contains(t, pods[0], "fieldSelector=spec.nodeName%3Dworker-1")
	assert.NotContains(t, pods[0], "labelSelector")
}

// TestDrainWithAClientThatCannotListByFieldSelectorIsAnInternalError asserts
// a client without the port is a programming error, not a server response.
func TestDrainWithAClientThatCannotListByFieldSelectorIsAnInternalError(t *testing.T) {
	t.Parallel()
	f := idleNode(t)
	_, err := plan(t, newDrainCmd(f, true), newInput("worker-1"))
	ce := requireCLIError(t, err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "internal error: the cluster client cannot list by field selector", ce.Message)
}

// TestDrainServerRefusals asserts the dry-run write's refusals pass through
// with the decided additions: an admission 403 is VALIDATION and unchanged,
// another 403 is RBAC with the roles that grant the update appended and no
// hint, a 409 is a retryable CONFLICT and a webhook-denial 409 a CONFLICT
// that is not retryable.
func TestDrainServerRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		message   string
		code      string
		retryable bool
		suffix    string
	}{
		{"admission", 403, `nodes.management.cattle.io "m-1" is forbidden: this namespace may not be deleted`, clierr.CodeValidation, false, ""},
		{"rbac", 403, `nodes.management.cattle.io "m-1" is forbidden: User "u" cannot update resource "nodes" in API group "management.cattle.io" in the namespace "c-1"`, clierr.CodeRBAC, false,
			"; this command needs access to the local cluster, with update on nodes.management.cattle.io in namespace c-1 there"},
		{"conflict", 409, `Operation cannot be fulfilled on nodes.management.cattle.io "m-1": the object has been modified; please apply your changes to the latest version and try again`, clierr.CodeConflict, true, ""},
		{"webhook denial conflict", 409, `admission webhook "rancher.cattle.io.nodes.management.cattle.io" denied the request: conflict`, clierr.CodeConflict, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := idleNode(t)
			f.putStatus, f.putMessage = tc.status, tc.message
			_, err := plan(t, newDrainCmd(f, false), newInput("worker-1"))
			ce := requireCLIError(t, err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.True(t, strings.HasSuffix(ce.Message, tc.suffix), ce.Message)
			if tc.suffix != "" {
				assert.Empty(t, ce.Hint)
			}
		})
	}
}

// TestDrainExecNoWaitReadsNothingAfterTheWrite asserts --wait=false sends the
// write and no read of the management node after it.
func TestDrainExecNoWaitReadsNothingAfterTheWrite(t *testing.T) {
	t.Parallel()
	f := idleNode(t)
	p, err := plan(t, newDrainCmd(f, false), newInput("worker-1", "--wait=false"))
	require.NoError(t, err)
	before := len(f.seen())
	res, err := p.exec(t.Context())
	require.NoError(t, err)
	after := f.seen()[before:]
	require.Len(t, after, 1)
	assert.True(t, strings.HasPrefix(after[0], "PUT "), after[0])
	assert.Equal(t, false, res.Data.(map[string]any)["waited"])
	puts := f.writes()
	require.Len(t, puts, 2)
	assert.True(t, puts[0].dryRun)
	assert.False(t, puts[1].dryRun)
}
