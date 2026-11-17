package resources

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestParseDeleteWait asserts how --wait and --timeout are read: the wait
// is on unless --wait=false is given, --timeout is a positive duration with
// a 5m default, and --wait=false with --timeout is refused.
func TestParseDeleteWait(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      input
		mode    waitMode
		timeout time.Duration
		text    string
		err     string
	}{
		{name: "default", in: input{}, mode: waitOn, timeout: 5 * time.Minute, text: "5m"},
		{name: "--wait", in: input{b: map[string]bool{"wait": true}, isSet: map[string]bool{"wait": true}}, mode: waitOn, timeout: 5 * time.Minute, text: "5m"},
		{name: "--wait=false", in: input{b: map[string]bool{"wait": false}, isSet: map[string]bool{"wait": true}}, mode: waitOff},
		{name: "--timeout 10m", in: input{s: map[string]string{"timeout": "10m"}, isSet: map[string]bool{"timeout": true}}, mode: waitOn, timeout: 10 * time.Minute, text: "10m"},
		{name: "--timeout 0s", in: input{s: map[string]string{"timeout": "0s"}, isSet: map[string]bool{"timeout": true}}, err: `--timeout "0s" must be a positive duration`},
		{name: "--timeout -1s", in: input{s: map[string]string{"timeout": "-1s"}, isSet: map[string]bool{"timeout": true}}, err: `--timeout "-1s" must be a positive duration`},
		{name: "--timeout x", in: input{s: map[string]string{"timeout": "x"}, isSet: map[string]bool{"timeout": true}}, err: `--timeout "x" is not a valid duration`},
		{name: "--wait=false --timeout 1m", in: input{b: map[string]bool{"wait": false}, s: map[string]string{"timeout": "1m"}, isSet: map[string]bool{"wait": true, "timeout": true}},
			err: "--wait=false conflicts with --timeout; remove one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, err := ParseDeleteWait(tc.in)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.mode, w.mode)
			assert.Equal(t, tc.timeout, w.timeout)
			assert.Equal(t, tc.text, w.timeoutText)
			assert.False(t, w.ownToken)
		})
	}
}

// TestPlanFromFetchedRefusesADeleteWithNoWaitSetting asserts a DeleteWait
// no parse made is refused before any request.
func TestPlanFromFetchedRefusesADeleteWithNoWaitSetting(t *testing.T) {
	t.Parallel()
	client := &delClient{obj: objWithRV("web", "1")}
	_, exec, err := PlanFromFetched(t.Context(), client, mustKind(t, "pod"), "team-a", "web", client.obj, "c-abc", DeleteWait{})
	require.Error(t, err)
	assert.Nil(t, exec)
	assert.Equal(t, clierr.CodeGeneric, clierr.From(err).Code)
	require.ErrorContains(t, err, "internal error: the delete has no wait setting")
	assert.Empty(t, client.deletes, "no request")
}

// stayClient answers every Get with the next of its responses, the last one
// repeating: an object that is not deleted, or an error.
type stayClient struct {
	engine.ClusterClient
	responses []stayResponse
	gets      int
	onGet     func()
}

type stayResponse struct {
	obj *unstructured.Unstructured
	err error
}

func (c *stayClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	a := c.responses[min(c.gets, len(c.responses)-1)]
	c.gets++
	if c.onGet != nil {
		c.onGet()
	}
	if a.err != nil {
		return nil, a.err
	}
	return a.obj.DeepCopy(), nil
}

// terminatingObject is an object with a deletionTimestamp and finalizers.
func terminatingObject(name string, finalizers ...string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "uid": "u1"}}}
	ts := metav1.NewTime(time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC))
	obj.SetDeletionTimestamp(&ts)
	obj.SetFinalizers(finalizers)
	return obj
}

func podTarget(t *testing.T) deleteTarget {
	t.Helper()
	k := mustKind(t, "pod")
	return deleteTarget{gvr: k.GVR, ns: "team-a", name: "web", uid: "u1", kind: k.Name,
		recovery: deleteRecovery(k, "team-a", "web", "c-abc"), readBefore: true}
}

func requireCommitted(t *testing.T, err error, kind, name, recovery string) *clierr.Error {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "a clierr error: %v", err)
	assert.False(t, ce.Retryable)
	assert.Equal(t, clierr.CommittedContextDetails{Committed: true, ResourceKind: kind, ResourceName: name, Recovery: recovery}, ce.Details)
	return ce
}

// TestDeleteWaitTimeoutNamesTheLastState asserts the timeout of a delete's
// wait is a GENERIC error that prints --timeout as given, says the deletion
// was requested, and names the object's deletionTimestamp and finalizers,
// the last error, and the get command as its hint and recovery.
func TestDeleteWaitTimeoutNamesTheLastState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &stayClient{responses: []stayResponse{
			{obj: terminatingObject("web", "example.com/hold", "example.com/other")},
			{err: clierr.New(clierr.CodeGeneric, "the server is busy").WithRetryable(true).WithCause(clierr.ResponseError(http.MethodGet, http.StatusServiceUnavailable, nil))},
		}}
		target := podTarget(t)
		start := time.Now()
		_, err := waitDeleted(t.Context(), client, []deleteTarget{target}, start.Add(12*time.Second), "12s")
		assert.Equal(t, 12*time.Second, time.Since(start))
		ce := requireCommitted(t, err, "pod", "web", clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'")
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, "timed out after 12s waiting for pod web to be deleted; the deletion was requested; "+
			"last state: deletionTimestamp 2026-11-16T10:00:00Z, finalizers: example.com/hold, example.com/other; "+
			"last error: GENERIC: the server is busy", ce.Message)
		assert.Equal(t, clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'", ce.Hint)
	})
}

// TestDeleteWaitTimeoutOfANamespaceNamesItsTrueConditions asserts the
// timeout of a namespace's wait also names each condition whose status is
// True, which says what keeps the namespace Terminating.
func TestDeleteWaitTimeoutOfANamespaceNamesItsTrueConditions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ns := terminatingObject("demo")
		_ = unstructured.SetNestedSlice(ns.Object, []any{
			map[string]any{"type": "NamespaceDeletionDiscoveryFailure", "status": "False", "message": "all good"},
			map[string]any{"type": "NamespaceContentRemaining", "status": "True", "message": "Some resources are remaining: pods. has 1 resource instances"},
			map[string]any{"type": "NamespaceFinalizersRemaining", "status": "True"},
		}, "status", "conditions")
		k := mustKind(t, "namespace")
		target := deleteTarget{gvr: k.GVR, name: "demo", uid: "u1", kind: k.Name, recovery: deleteRecovery(k, "", "demo", "c-abc"), readBefore: true}
		_, err := waitDeleted(t.Context(), &stayClient{responses: []stayResponse{{obj: ns}}}, []deleteTarget{target}, time.Now().Add(time.Second), "1s")
		ce := requireCommitted(t, err, "namespace", "demo", clierr.Rancher+" get namespace 'demo' --cluster 'c-abc'")
		assert.Equal(t, "timed out after 1s waiting for namespace demo to be deleted; the deletion was requested; "+
			"last state: deletionTimestamp 2026-11-16T10:00:00Z, finalizers: <none>, "+
			"condition NamespaceContentRemaining=True (Some resources are remaining: pods. has 1 resource instances), "+
			"condition NamespaceFinalizersRemaining=True", ce.Message)
	})
}

// TestDeleteWaitTimeoutWithNoResponseSaysNoStateObserved asserts a timeout
// before any read got a response names no state.
func TestDeleteWaitTimeoutWithNoResponseSaysNoStateObserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &stayClient{responses: []stayResponse{{err: clierr.Retryable(clierr.CodeGeneric, "connection refused")}}}
		_, err := waitDeleted(t.Context(), client, []deleteTarget{podTarget(t)}, time.Now().Add(time.Second), "1s")
		ce := requireCommitted(t, err, "pod", "web", clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'")
		assert.Equal(t, "timed out after 1s waiting for pod web to be deleted; the deletion was requested; last state: <no state observed>; last error: GENERIC: connection refused", ce.Message)
	})
}

// TestDeleteWaitInterruptSaysTheDeletionWasRequested asserts a wait the
// caller cancels ends with a GENERIC error that says the deletion was
// requested and carries the committed details, never the context's error
// alone.
func TestDeleteWaitInterruptSaysTheDeletionWasRequested(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		client := &stayClient{responses: []stayResponse{{obj: terminatingObject("web")}}, onGet: cancel}
		_, err := waitDeleted(ctx, client, []deleteTarget{podTarget(t)}, time.Now().Add(time.Minute), "1m")
		ce := requireCommitted(t, err, "pod", "web", clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'")
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, "stopped waiting for pod web to be deleted; the deletion was requested", ce.Message)
		assert.NotErrorIs(t, err, context.Canceled)
	})
}

// TestDeleteWaitRefusedReadOfAnObjectNotReadBeforeIsAnError asserts a 403
// during the wait for an object the command did not read before the delete
// keeps the response's code: the caller may never have been able to read it.
func TestDeleteWaitRefusedReadOfAnObjectNotReadBeforeIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		refused := clierr.FromResponse("", http.MethodGet, http.StatusForbidden, nil, nil)
		client := &stayClient{responses: []stayResponse{{err: refused}}}
		target := podTarget(t)
		target.readBefore = false
		_, err := waitDeleted(t.Context(), client, []deleteTarget{target}, time.Now().Add(time.Minute), "1m")
		ce := requireCommitted(t, err, "pod", "web", clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'")
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		assert.Equal(t, 6, ce.ExitCode())
		assert.Equal(t, "could not read pod web after the delete request: "+refused.Message+"; the deletion was requested and not confirmed", ce.Message)
		assert.Equal(t, clierr.Rancher+" get pod 'web' --cluster 'c-abc' --namespace 'team-a'", ce.Hint)
		assert.Equal(t, 1, client.gets, "no retry after the refusal")
	})
}

// TestDeleteWaitRefusedReadOfAnObjectReadBeforeEndsWithAWarning asserts a
// 403 during the wait for an object the command read before the delete
// ends that object's wait with a warning, and the next object is still
// waited for.
func TestDeleteWaitRefusedReadOfAnObjectReadBeforeEndsWithAWarning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		refused := clierr.FromResponse("", http.MethodGet, http.StatusForbidden, nil, nil)
		client := &stayClient{responses: []stayResponse{{err: refused}, {err: clierr.New(clierr.CodeNotFound, "gone")}}}
		second := podTarget(t)
		second.name = "web-2"
		end, err := waitDeleted(t.Context(), client, []deleteTarget{podTarget(t), second}, time.Now().Add(time.Minute), "1m")
		require.NoError(t, err)
		assert.False(t, end.waited)
		assert.Equal(t, []string{"your access to pod web ended after the delete request; the deletion was requested and not confirmed"}, end.warnings)
		assert.Equal(t, 2, client.gets, "one read of each object")
	})
}

// TestDeleteProjectWithNamespacesRefusesWaitTrueAndTimeout asserts
// --delete-namespaces refuses a --wait of true and a --timeout before any
// request.
func TestDeleteProjectWithNamespacesRefusesWaitTrueAndTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   input
		want string
	}{
		{"--wait", input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-abc"},
			b: map[string]bool{FlagDeleteNamespaces: true, "wait": true}, isSet: map[string]bool{FlagDeleteNamespaces: true, "wait": true}},
			"--wait conflicts with --delete-namespaces, which does not wait; remove --wait or use --wait=false"},
		{"--timeout", input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-abc", "timeout": "1m"},
			b: map[string]bool{FlagDeleteNamespaces: true}, isSet: map[string]bool{FlagDeleteNamespaces: true, "timeout": true}},
			"--timeout conflicts with --delete-namespaces, which does not wait; remove --timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProjectFixture(t, member("apps", "3"))
			_, _, err := f.cmd.Plan(t.Context(), tc.in)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)
			assert.Empty(t, f.log, "no request")
		})
	}
}

// TestDeleteProjectWithNamespacesDoesNotWait asserts a run with
// --delete-namespaces says deletion started and waited false, warns that
// it does not wait, except with --wait=false, and reads nothing after the
// project's delete.
func TestDeleteProjectWithNamespacesDoesNotWait(t *testing.T) {
	t.Parallel()
	warning := "with --delete-namespaces the command does not wait for the namespaces and the project to be deleted; check with " +
		clierr.Rancher + " get project 'p-1' --cluster 'c-abc'"
	for _, tc := range []struct {
		name     string
		in       input
		warnings []string
	}{
		{"default", input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-abc"}, b: map[string]bool{FlagDeleteNamespaces: true},
			isSet: map[string]bool{FlagDeleteNamespaces: true}}, []string{warning}},
		{"--wait=false", input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-abc"}, b: map[string]bool{FlagDeleteNamespaces: true, "wait": false},
			isSet: map[string]bool{FlagDeleteNamespaces: true, "wait": true}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newProjectFixture(t, member("apps", "3"))
			_, exec, err := f.cmd.Plan(t.Context(), tc.in)
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "project/p-1 deletion started\n  namespaces:\n    deletion started: apps\n", res.Text)
			data, ok := res.Data.(map[string]any)
			require.True(t, ok)
			assert.Equal(t, false, data["waited"])
			assert.Equal(t, tc.warnings, res.Warnings)
			assert.Equal(t, "delete c-abc/p-1 rv=5", f.log[len(f.log)-1], "nothing is read after the project's delete")
		})
	}
}

// TestDeleteProjectPartialFailureCarriesTheCommittedFields asserts the
// committed fields sit beside objects in the JSON when the details carry
// them and are absent otherwise, and that the text lists the objects first.
func TestDeleteProjectPartialFailureCarriesTheCommittedFields(t *testing.T) {
	t.Parallel()
	objs := []safety.Object{{Kind: "namespace", Name: "apps", Outcome: safety.OutcomeRancherKeeps}}
	committed := &clierr.CommittedContextDetails{Committed: true, ResourceKind: "project", ResourceName: "p-1", Recovery: "rancher get project 'p-1' --cluster 'c-abc'"}

	decode := func(v DeleteProjectPartialFailure) map[string]any {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw, &out))
		return out
	}
	without := decode(DeleteProjectPartialFailure{Objects: objs})
	assert.ElementsMatch(t, []string{"objects"}, keysOf(without))
	with := decode(DeleteProjectPartialFailure{Objects: objs, CommittedContextDetails: committed})
	assert.ElementsMatch(t, []string{"objects", "committed", "resourceKind", "resourceName", "recovery"}, keysOf(with))
	assert.Equal(t, true, with["committed"])
	assert.Equal(t, "p-1", with["resourceName"])
	assert.Equal(t, "rancher get project 'p-1' --cluster 'c-abc'", with["recovery"])

	var lines []string
	for _, l := range (DeleteProjectPartialFailure{Objects: objs, CommittedContextDetails: committed}).TextDetails() {
		lines = append(lines, l.String())
	}
	assert.Equal(t, []string{
		"namespaces:",
		"  rancher keeps: apps",
		"committed: project p-1 - the change may already be on the server",
		"recover: rancher get project 'p-1' --cluster 'c-abc'",
	}, lines)
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
