package observe

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func execLogs(t *testing.T, clients *fakeFactory, in input) (runtime.Result, error) {
	t.Helper()
	cmd := NewLogs(clients, clients).(runtime.ReadCommand)
	return cmd.Execute(t.Context(), in, nil)
}

func TestLogsExecuteMergesAndPrefixesMultiPod(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", podList("agent-1", "agent-2")).
		withLog("main", "hello\n")
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	res, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "[agent-1/main] hello\n")
	assert.Contains(t, res.Text, "[agent-2/main] hello\n")
	data, ok := res.Data.(logsResult)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"agent-1", "agent-2"}, data.Pods)
	assert.Len(t, data.Entries, 2)
}

// TestLogsRendersByteIdenticalUnderColor asserts the exemption: logs hand-builds
// Text directly (user-owned log content) and sets no StyledText, so
// --color=always renders byte-identical to plain.
func TestLogsRendersByteIdenticalUnderColor(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", podList("agent-1")).
		withLog("main", "hello\n")
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	cmd := NewLogs(factory, factory)
	in := input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}}

	var plain, colored bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &plain, Err: new(bytes.Buffer), Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	_, ce = runtime.Run(t.Context(), cmd, in,
		runtime.IO{Out: &colored, Err: new(bytes.Buffer), Format: "", Color: "always", OutTTY: true}, safety.Binding{})
	require.Nil(t, ce)

	assert.Equal(t, plain.String(), colored.String())
	assert.NotContains(t, colored.String(), "\x1b[")
}

func TestLogsExecuteContainerFilter(t *testing.T) {
	t.Parallel()
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		unstructuredPodWithContainers("agent-1", "main", "sidecar"),
	}}
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", pods).
		withLog("main", "main-line\n").
		withLog("sidecar", "sidecar-line\n")
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	res, err := execLogs(t, factory, input{
		args: []string{"agent"},
		s:    map[string]string{engine.FlagCluster: "c-1", flagContainer: "sidecar"},
	})
	require.NoError(t, err)
	data := res.Data.(logsResult)
	require.Len(t, data.Entries, 1)
	assert.Equal(t, "sidecar", data.Entries[0].Container)
	assert.NotContains(t, res.Text, "main-line")
}

func TestLogsExecutePodFlag(t *testing.T) {
	t.Parallel()

	newDownstream := func() *fakeClient {
		return newFakeClient().
			withListNS(podKind.GVR, "cattle-system", podList("agent-1", "agent-2")).
			withLog("main", "hello\n")
	}

	t.Run("narrows to the named pod", func(t *testing.T) {
		t.Parallel()
		downstream := newDownstream()
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		res, err := execLogs(t, factory, input{
			args: []string{"agent"},
			s:    map[string]string{engine.FlagCluster: "c-1", flagPod: "agent-2"},
		})
		require.NoError(t, err)
		data := res.Data.(logsResult)
		assert.Equal(t, []string{"agent-2"}, data.Pods)
		assert.NotContains(t, res.Text, "agent-1")
		assert.Contains(t, res.Text, "[agent-2/main] hello\n")
	})

	t.Run("zero resolved pods reports the no-logs error, not an empty candidate list", func(t *testing.T) {
		t.Parallel()
		downstream := newFakeClient().
			withListNS(podKind.GVR, "cattle-system", podList())
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{
			args: []string{"agent"},
			s:    map[string]string{engine.FlagCluster: "c-1", flagPod: "agent-1"},
		})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Contains(t, ce.Message, "no logs available",
			"the target/namespace/cluster error is the honest one; a pod-not-found with zero candidates points at a list that does not exist")
		assert.Empty(t, ce.Candidates)
	})

	t.Run("unknown pod is NOT_FOUND with the real pod names as candidates", func(t *testing.T) {
		t.Parallel()
		downstream := newDownstream()
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{
			args: []string{"agent"},
			s:    map[string]string{engine.FlagCluster: "c-1", flagPod: "agent-9"},
		})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		require.Len(t, ce.Candidates, 2)
		assert.ElementsMatch(t, []string{"agent-1", "agent-2"}, []string{ce.Candidates[0].ID, ce.Candidates[1].ID})
	})
}

func TestLogsExecutePartialFailureWarnsButSucceeds(t *testing.T) {
	t.Parallel()
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		unstructuredPodWithContainers("agent-1", "main"),
		unstructuredPodWithContainers("agent-2", "broken"),
	}}
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", pods).
		withLog("main", "ok\n").
		withLogErr("broken", errors.New("rbac denied"))
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	res, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
	require.NoError(t, err)
	data := res.Data.(logsResult)
	require.Len(t, data.Entries, 1)
	assert.Equal(t, "main", data.Entries[0].Container)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "rbac denied")
}

func TestLogsExecuteZeroYieldedLogsIsAnError(t *testing.T) {
	t.Parallel()

	t.Run("no pods matched", func(t *testing.T) {
		t.Parallel()
		factory := &fakeFactory{local: newFakeClient()} // webhook: local-only, no pods anywhere
		_, err := execLogs(t, factory, input{args: []string{"webhook"}})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Contains(t, ce.Message, "no logs available")
	})

	t.Run("pods matched but every fetch failed", func(t *testing.T) {
		t.Parallel()
		downstream := newFakeClient().
			withListNS(podKind.GVR, "cattle-system", podList("agent-1")).
			withLogErr("main", errors.New("rbac denied"))
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
	})
}

// TestLogsExecuteEveryReadRefusedKeepsTheRefusal asserts that when no log
// was read and every container's read was refused with AUTH, or every one
// with RBAC, the command returns that code with the first read's message
// and a warning per read, while a mix of the two stays NOT_FOUND.
func TestLogsExecuteEveryReadRefusedKeepsTheRefusal(t *testing.T) {
	t.Parallel()
	pods := func() *unstructured.UnstructuredList {
		return &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			unstructuredPodWithContainers("agent-1", "main"),
			unstructuredPodWithContainers("agent-2", "side"),
		}}
	}
	run := func(mainErr, sideErr error) error {
		downstream := newFakeClient().
			withListNS(podKind.GVR, "cattle-system", pods()).
			withLogErr("main", mainErr).
			withLogErr("side", sideErr)
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
		return err
	}
	auth := func(msg string) error { return clierr.New(clierr.CodeAuth, msg) }
	rbac := func(msg string) error { return clierr.New(clierr.CodeRBAC, msg) }

	for _, tc := range []struct {
		name       string
		main, side error
		code       string
		message    string
	}{
		{"every read AUTH", auth("main rejected"), auth("side rejected"), clierr.CodeAuth, "main rejected"},
		{"every read RBAC", rbac("main forbidden"), rbac("side forbidden"), clierr.CodeRBAC, "main forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := run(tc.main, tc.side)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, []string{
				"could not read the logs of agent-1/main: " + tc.main.Error(),
				"could not read the logs of agent-2/side: " + tc.side.Error(),
			}, ce.Warnings())
		})
	}

	t.Run("one AUTH and one RBAC", func(t *testing.T) {
		t.Parallel()
		err := run(auth("main rejected"), rbac("side forbidden"))
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Empty(t, ce.Warnings())
	})

	t.Run("one AUTH and one failure that is no refusal", func(t *testing.T) {
		t.Parallel()
		err := run(auth("main rejected"), errors.New("container is not running"))
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
	})
}

func TestLogsExecuteSinceWrapper(t *testing.T) {
	t.Parallel()

	newDownstream := func() *fakeClient {
		return newFakeClient().
			withListNS(podKind.GVR, "cattle-system", podList("agent-1")).
			withLog("main", "line\n")
	}

	t.Run("omitted --since leaves SinceSeconds nil", func(t *testing.T) {
		t.Parallel()
		downstream := newDownstream()
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		require.Len(t, downstream.logOptsCalls, 1)
		assert.Nil(t, downstream.logOptsCalls[0].SinceSeconds)
	})

	t.Run("a parsed duration sets SinceSeconds", func(t *testing.T) {
		t.Parallel()
		downstream := newDownstream()
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1", flagSince: "30m"}})
		require.NoError(t, err)
		require.Len(t, downstream.logOptsCalls, 1)
		require.NotNil(t, downstream.logOptsCalls[0].SinceSeconds)
		assert.Equal(t, int64(1800), *downstream.logOptsCalls[0].SinceSeconds)
	})

	t.Run("an explicit zero duration is VALIDATION", func(t *testing.T) {
		t.Parallel()
		downstream := newDownstream()
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1", flagSince: "0s"}})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Empty(t, downstream.logOptsCalls, "the since VALIDATION error must reject before any fetch")
	})
}

func TestLogsExecuteUnknownTargetIsValidation(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{local: newFakeClient()}
	_, err := execLogs(t, factory, input{args: []string{"bogus"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogsExecuteTailFlag(t *testing.T) {
	t.Parallel()

	t.Run("empty --tail defaults to 200", func(t *testing.T) {
		t.Parallel()
		downstream := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("agent-1")).withLog("main", "x\n")
		factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1"}})
		require.NoError(t, err)
		require.Len(t, downstream.logOptsCalls, 1)
		require.NotNil(t, downstream.logOptsCalls[0].TailLines)
		assert.Equal(t, int64(200), *downstream.logOptsCalls[0].TailLines)
	})

	t.Run("negative --tail is VALIDATION", func(t *testing.T) {
		t.Parallel()
		factory := &fakeFactory{local: newFakeClient()}
		_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1", flagTail: "-1"}})
		require.Error(t, err)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

// ownerFactory is the factory of a cluster owner: the local cluster client
// refuses every management Cluster read, and steve serves prod as c-abc
// with an authorized cluster endpoint and state active.
func ownerFactory(downstream *fakeClient) (*fakeFactory, *fakeClient) {
	local := newFakeClient().withDeniedKind(clusterKind.GVR, clierr.New(clierr.CodeRBAC, "forbidden"))
	root := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-abc", "state": map[string]any{"name": "active"}},
		"spec": map[string]any{
			"displayName":              "prod",
			"localClusterAuthEndpoint": map[string]any{"enabled": true},
		},
	}})
	return &fakeFactory{
		local:      local,
		root:       root,
		perCluster: map[string]engine.ClusterClient{"c-abc": downstream},
		resolve:    map[string]string{"prod": "c-abc"},
	}, local
}

// TestLogsKubeAPIAuthAsClusterOwner asserts that a caller who cannot read
// management Clusters on the local cluster still resolves kube-api-auth on
// a cluster they own: the Cluster object comes from steve, and no read of
// the management Cluster kind reaches the local client.
func TestLogsKubeAPIAuthAsClusterOwner(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", podList("kaa-1")).
		withLog("main", "ready\n")
	factory, local := ownerFactory(downstream)

	res, err := execLogs(t, factory, input{args: []string{"kube-api-auth"}, s: map[string]string{engine.FlagCluster: "prod"}})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "[kaa-1/main] ready\n")
	assert.NotContains(t, local.kindReads, clusterKind.GVR, "no management Cluster read may reach the local client")
	assert.Equal(t, []string{clusterPath("c-abc")}, factory.root.paths)
}

// TestLogsKubeAPIAuthPropagatesSteveError asserts that a failed steve read
// of the Cluster object fails logs for kube-api-auth, not reported as a
// cluster without an authorized cluster endpoint.
func TestLogsKubeAPIAuthPropagatesSteveError(t *testing.T) {
	t.Parallel()
	factory, _ := ownerFactory(newFakeClient())
	factory.root.withError(clusterPath("c-abc"), rootStatusError(500, clusterPath("c-abc")))

	_, err := execLogs(t, factory, input{args: []string{"kube-api-auth"}, s: map[string]string{engine.FlagCluster: "prod"}})
	require.ErrorContains(t, err, "Internal Server Error")
	assert.NotEqual(t, clierr.CodeNotFound, clierrCode(t, err))
}

// TestLogsAndBundleSinceRefusalsNameTheFlag asserts a --since that does not
// parse, is below one second or overflows is refused naming --since (never
// --ttl), before any request, by both commands that take it.
func TestLogsAndBundleSinceRefusalsNameTheFlag(t *testing.T) {
	t.Parallel()
	const grammar = "use a duration such as 90m or 12h; it can start with a number of days, as in 90d or 1d12h"
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"bogus", `invalid --since "bogus": ` + grammar},
		{"500us", `invalid --since "500us": the duration is shorter than one second; pass at least 1s`},
		{"213504d", `invalid --since "213504d": days must be at most 106751 (about 292 years)`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			downstream := newFakeClient()
			factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

			_, err := execLogs(t, factory, input{args: []string{"agent"}, s: map[string]string{engine.FlagCluster: "c-1", flagSince: tc.raw}})
			require.EqualError(t, err, tc.want)
			assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
			assert.Empty(t, downstream.logOptsCalls)

			_, err = execBundle(t, factory, input{s: map[string]string{flagSince: tc.raw}})
			require.EqualError(t, err, tc.want)
			assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
		})
	}
}

// TestLogsDownstreamOnlyTargetWithoutClusterNamesTheTarget asserts a target
// that runs only on a downstream cluster, given no --cluster, is refused
// naming that target, with the cluster listing command as the hint, before
// any request.
func TestLogsDownstreamOnlyTargetWithoutClusterNamesTheTarget(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"agent", "kube-api-auth"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			factory := &fakeFactory{local: newFakeClient()}
			_, err := execLogs(t, factory, input{args: []string{target}})
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "--cluster is required; "+target+" runs only on a downstream cluster, pass a downstream cluster id or name", ce.Message)
			assert.Equal(t, engine.ClusterRequiredHint.Field(), ce.Hint)
		})
	}
}
