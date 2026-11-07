package oidcclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pollStep is one scripted answer of a poll Get.
type pollStep struct {
	obj *unstructured.Unstructured
	err error
}

// cutRequestError is the retryable GENERIC error the provider client builds
// for a request cut by its context.
func cutRequestError() error {
	return clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow")
}

// scriptedPollClient serves the oidcclients Gets from a script, one step per
// call after the first skip calls (the Plan's own read). Past the end of the
// script the last step repeats, or, when blockAtEnd is set, a Get blocks until
// its context ends and returns blockErr, or the retryable GENERIC error the
// provider client builds for a cut request. The secrets Gets go to the
// embedded fake. takes is how long a request takes, and base, when set, makes
// the client log each request of the wait as "kind@seconds" from base.
type scriptedPollClient struct {
	*fakeOIDCClient
	planObj    *unstructured.Unstructured
	skip       int
	script     []pollStep
	blockAtEnd bool
	blockErr   error
	takes      time.Duration
	base       time.Time
	log        []string
	calls      int
}

func (c *scriptedPollClient) record(kind string) {
	if !c.base.IsZero() {
		c.log = append(c.log, fmt.Sprintf("%s@%g", kind, time.Since(c.base).Seconds()))
	}
}

// take waits for takes, or until ctx ends, which is an error.
func (c *scriptedPollClient) take(ctx context.Context) error {
	if c.takes <= 0 {
		return nil
	}
	select {
	case <-time.After(c.takes):
		return nil
	case <-ctx.Done():
		return cutRequestError()
	}
}

// block waits for ctx to end and returns the error the cut request gives.
func (c *scriptedPollClient) block(ctx context.Context) error {
	<-ctx.Done()
	if c.blockErr != nil {
		return c.blockErr
	}
	return cutRequestError()
}

func (c *scriptedPollClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "secrets" {
		return c.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	n := c.calls
	c.calls++
	if n < c.skip {
		return c.planObj, nil
	}
	c.record("get")
	if err := c.take(ctx); err != nil {
		return nil, err
	}
	i := n - c.skip
	if i >= len(c.script) {
		if c.blockAtEnd {
			return nil, c.block(ctx)
		}
		i = len(c.script) - 1
	}
	return c.script[i].obj, c.script[i].err
}

// waitVerb describes one verb that waits on the OIDCClient: how to plan it
// against a client, the object that keeps its wait going, the object that
// ends it with success and the kind of the result (none for rotate-secret,
// whose success needs a changed Secret too), the number of reads its Plan
// makes before the wait starts, whether its timeout is retryable, and the
// secret name and recovery text of the committed details of an aborted wait.
// plan gets the client the verb reads through and the script client behind
// it.
type waitVerb struct {
	name             string
	skip             int
	planObj          *unstructured.Unstructured
	unfinished       *unstructured.Unstructured
	finished         *unstructured.Unstructured
	finishedKind     string
	retryableTimeout bool
	abortSecretName  string
	abortRecovery    string
	plan             func(t *testing.T, client engine.ClusterClient, c *scriptedPollClient) runtime.ExecuteFunc
}

func waitVerbs() []waitVerb { return waitVerbsWithTimeout("30ms") }

func waitVerbsWithTimeout(timeout string) []waitVerb {
	base := func() *unstructured.Unstructured {
		return oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	}
	annotated := func(annotation string) *unstructured.Unstructured {
		o := base()
		_ = unstructured.SetNestedField(o.Object, "client-secret-1", "metadata", "annotations", annotation)
		return o
	}
	flags := map[string]string{"timeout": timeout}
	newKeys := func() *unstructured.Unstructured {
		return oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{}})
	}
	twoSecrets := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{
		"client-secret-1": b64("old-value"), "client-secret-2": b64("new-secret"),
	}}}
	oldSecret := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}}
	return []waitVerb{
		{
			name: "create", skip: 0, unfinished: oidcClientObj("my-app", "", nil), finished: base(),
			finishedKind: "OIDCClientCreated", abortRecovery: recoveryHint("my-app"),
			plan: func(t *testing.T, client engine.ClusterClient, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				c.secretResp = oldSecret
				cmd := NewCreate(stubClusterClientFactory{c: client}, stubRootFactory{c: fakeRootClient{body: []byte(discoveryOK)}},
					stubServerBase{base: "https://rancher.example"})
				_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "add-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretCreate), finished: newKeys(),
			finishedKind:  "OIDCClientSecretAdded",
			abortRecovery: "check " + recoveryHint("my-app") + "; if a new key landed its value was never shown - rotate that key by name (<client> is the oidc-client and <secret> the new key's name) and add --apply --yes after reading the preview: " + clierr.Rancher + " oidc-client rotate-secret <client> <secret> --dry-run",
			plan: func(t *testing.T, client engine.ClusterClient, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				c.secretResp = twoSecrets
				_, exec, err := planAddSecret(t, NewAddSecret(stubClusterClientFactory{c: client}),
					oidcInput{args: []string{"my-app"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "remove-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretRemove),
			finished:     oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-2": map[string]any{}}),
			finishedKind: "OIDCClientSecretRemoved", abortSecretName: "client-secret-1",
			abortRecovery: "read the client first: " + recoveryHint("my-app") + "; the removal landed iff the key is gone; re-run only while the key remains",
			plan: func(t *testing.T, client engine.ClusterClient, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				_, exec, err := planRemoveSecret(t, NewRemoveSecret(stubClusterClientFactory{c: client}),
					oidcInput{args: []string{"my-app", "client-secret-1"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "rotate-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretRegenerate),
			retryableTimeout: true, abortSecretName: "client-secret-1", abortRecovery: recoveryHint("my-app"),
			plan: func(t *testing.T, client engine.ClusterClient, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				c.secretResp = oldSecret
				_, exec, err := planRotateSecret(t, NewRotateSecret(stubClusterClientFactory{c: client}),
					oidcInput{args: []string{"my-app", "client-secret-1"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
	}
}

// waitMode is how a verb's wait reaches the OIDCClient: polling with Get, or
// watching.
type waitMode string

const (
	pollMode  waitMode = "poll"
	watchMode waitMode = "watch"
)

var waitModes = []waitMode{pollMode, watchMode}

// waitRun is what one run of a verb's execute gave, with the clients it ran
// against; watch is nil in poll mode.
type waitRun struct {
	client *scriptedPollClient
	watch  *scriptedWatchClient
	res    runtime.Result
	err    error
}

// waitSetup is what a run's client does: the script of the OIDCClient
// answers, what a request that ends at the deadline returns, how long a
// request takes, a change to the watching client, and a client that wraps it
// for the verb to read through.
type waitSetup struct {
	script     []pollStep
	blockAtEnd bool
	blockErr   error
	takes      time.Duration
	watch      func(w *scriptedWatchClient)
	wrap       func(w *scriptedWatchClient) engine.ClusterClient
}

// exec plans the verb against a client that serves the setup in the mode and
// runs its execute with ctx.
func (v waitVerb) exec(ctx context.Context, t *testing.T, m waitMode, setup waitSetup) waitRun {
	t.Helper()
	c := &scriptedPollClient{
		fakeOIDCClient: &fakeOIDCClient{},
		planObj:        v.planObj, skip: v.skip, script: setup.script, blockAtEnd: setup.blockAtEnd,
		blockErr: setup.blockErr, takes: setup.takes, base: time.Now(),
	}
	r := waitRun{client: c}
	var client engine.ClusterClient = c
	if m == watchMode {
		r.watch = &scriptedWatchClient{scriptedPollClient: c}
		if setup.watch != nil {
			setup.watch(r.watch)
		}
		client = r.watch
		if setup.wrap != nil {
			client = setup.wrap(r.watch)
		}
	}
	exec := v.plan(t, client, c)
	r.res, r.err = exec(ctx)
	return r
}

// run is exec with the poll interval of a fast test, that must fail with a
// *clierr.Error.
func (v waitVerb) run(t *testing.T, m waitMode, script []pollStep, blockAtEnd bool) (*scriptedPollClient, *clierr.Error) {
	t.Helper()
	withFastPoll(t)
	r := v.exec(t.Context(), t, m, waitSetup{script: script, blockAtEnd: blockAtEnd})
	require.Error(t, r.err)
	ce, ok := errors.AsType[*clierr.Error](r.err)
	require.True(t, ok)
	return r.client, ce
}

// TestOIDCVerbWaitEndsAtOnceOnAuthAndTLSFailures asserts that for every verb
// that waits on the OIDCClient, polling or watching, an authentication or
// certificate failure of the first request ends the wait at that request
// with its code, never retryable, and carries the verb's committed details
// (the verb already wrote to the server), in place of any other details the
// error had.
func TestOIDCVerbWaitEndsAtOnceOnAuthAndTLSFailures(t *testing.T) {
	codes := []struct {
		name string
		err  *clierr.Error
	}{
		{"auth", clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithHint("rancher auth status")},
		{"tls trust", clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted").
			WithDetails(clierr.CommittedContextDetails{ResourceKind: "certificate", ResourceName: "leaf"})},
	}
	for _, v := range waitVerbs() {
		for _, m := range waitModes {
			for _, tt := range codes {
				t.Run(v.name+" "+string(m)+" "+tt.name, func(t *testing.T) {
					c, ce := v.run(t, m, []pollStep{{err: tt.err}}, false)
					assert.Equal(t, tt.err.Code, ce.Code)
					assert.Equal(t, tt.err.Hint, ce.Hint)
					assert.False(t, ce.Retryable)
					// Both modes count one read of the script: a poll Get, or the List.
					assert.Equal(t, v.skip+1, c.calls, "the wait ended at its first request")
					details, ok := ce.Details.(clierr.CommittedContextDetails)
					require.True(t, ok)
					assert.True(t, details.Committed)
					assert.Equal(t, "OIDCClient", details.ResourceKind)
					assert.Equal(t, "my-app", details.ResourceName)
					assert.Equal(t, v.abortSecretName, details.SecretName)
					assert.Equal(t, v.abortRecovery, details.Recovery)
				})
			}
		}
	}
}

// TestOIDCVerbWaitAbortKeepsTheTextOfAWrappedError asserts that for every
// verb, polling or watching, an authentication failure that a caller added
// text to ends the wait with that text in its message and the same code.
func TestOIDCVerbWaitAbortKeepsTheTextOfAWrappedError(t *testing.T) {
	for _, v := range waitVerbs() {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m), func(t *testing.T) {
				wrapped := fmt.Errorf("get oidc-client my-app: %w", clierr.New(clierr.CodeAuth, "the server rejected the credential (401)"))
				_, ce := v.run(t, m, []pollStep{{err: wrapped}}, false)
				assert.Equal(t, clierr.CodeAuth, ce.Code)
				assert.Equal(t, "get oidc-client my-app: the server rejected the credential (401)", ce.Message)
				details, ok := ce.Details.(clierr.CommittedContextDetails)
				require.True(t, ok)
				assert.True(t, details.Committed)
			})
		}
	}
}

// TestOIDCVerbWaitRetriesForbiddenAndNamesTheLastError asserts that for
// every verb, polling or watching, a 403 is retried until the timeout, the
// timeout message ends with the last error and the timeout keeps its
// retryable flag. A watched wait gets the 403 from its List, then polls.
func TestOIDCVerbWaitRetriesForbiddenAndNamesTheLastError(t *testing.T) {
	for _, v := range waitVerbs() {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m), func(t *testing.T) {
				c, ce := v.run(t, m, []pollStep{{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")}}, false)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.Equal(t, v.retryableTimeout, ce.Retryable)
				assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
				assert.True(t, strings.HasSuffix(ce.Message, "; last error: RBAC: forbidden (403)"), ce.Message)
				assert.Greater(t, c.calls, v.skip+1, "the 403 was retried")
				details, ok := ce.Details.(clierr.CommittedContextDetails)
				require.True(t, ok)
				assert.True(t, details.Committed)
			})
		}
	}
}

// TestOIDCVerbWaitTimeoutAfterAnErrorThenAnObjectNamesNoError asserts that
// for every verb, polling or watching, a later request that returns an
// object clears the last error: the timeout message has no last error part.
func TestOIDCVerbWaitTimeoutAfterAnErrorThenAnObjectNamesNoError(t *testing.T) {
	for _, v := range waitVerbs() {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m), func(t *testing.T) {
				_, ce := v.run(t, m, []pollStep{
					{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")},
					{obj: v.unfinished},
				}, false)
				assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
				assert.NotContains(t, ce.Message, "last error")
			})
		}
	}
}

// TestOIDCVerbWaitTimeoutDuringAGetNamesNoCutError asserts that for every
// verb, polling or watching, a request cut by the wait's own deadline is not
// recorded as the last error, while an error from an earlier request that
// ended with the context live stays named.
func TestOIDCVerbWaitTimeoutDuringAGetNamesNoCutError(t *testing.T) {
	for _, v := range waitVerbs() {
		for _, m := range waitModes {
			t.Run(v.name+" "+string(m)+" no earlier error", func(t *testing.T) {
				_, ce := v.run(t, m, nil, true)
				assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
				assert.NotContains(t, ce.Message, "last error")
				assert.NotContains(t, ce.Message, "deadline exceeded")
			})
			t.Run(v.name+" "+string(m)+" earlier forbidden", func(t *testing.T) {
				_, ce := v.run(t, m, []pollStep{{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")}}, true)
				assert.Contains(t, ce.Message, "; last error: RBAC: forbidden (403)")
				assert.NotContains(t, ce.Message, "deadline exceeded")
			})
		}
	}
}
