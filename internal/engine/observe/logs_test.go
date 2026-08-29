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

func execLogs(t *testing.T, clients engine.ClusterClientFactory, in input) (runtime.Result, error) {
	t.Helper()
	cmd := NewLogs(clients).(runtime.ReadCommand)
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

// TestLogsRendersByteIdenticalUnderColor pins the exemption: logs hand-builds
// Text directly (user-owned log content) and sets no StyledText, so
// --color=always renders byte-identical to plain.
func TestLogsRendersByteIdenticalUnderColor(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().
		withListNS(podKind.GVR, "cattle-system", podList("agent-1")).
		withLog("main", "hello\n")
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	cmd := NewLogs(factory)
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
