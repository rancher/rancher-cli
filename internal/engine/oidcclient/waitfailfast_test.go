package oidcclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
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

// scriptedPollClient serves the oidcclients Gets from a script, one step per
// call after the first skip calls (the Plan's own read). Past the end of the
// script the last step repeats, or, when blockAtEnd is set, a Get blocks until
// its context ends and returns the retryable GENERIC error the provider
// client builds for a cut request. The secrets Gets go to the embedded fake.
type scriptedPollClient struct {
	*fakeOIDCClient
	planObj    *unstructured.Unstructured
	skip       int
	script     []pollStep
	blockAtEnd bool
	calls      int
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
	i := n - c.skip
	if i >= len(c.script) {
		if c.blockAtEnd {
			<-ctx.Done()
			return nil, clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow")
		}
		i = len(c.script) - 1
	}
	return c.script[i].obj, c.script[i].err
}

// waitVerb describes one verb that waits on the OIDCClient: how to plan it
// against a client, the object that keeps its wait going, the number of
// reads its Plan makes before the wait starts, whether its timeout is
// retryable, and the secret name and recovery text of the committed details
// of an aborted wait.
type waitVerb struct {
	name             string
	skip             int
	planObj          *unstructured.Unstructured
	unfinished       *unstructured.Unstructured
	retryableTimeout bool
	abortSecretName  string
	abortRecovery    string
	plan             func(t *testing.T, c *scriptedPollClient) runtime.ExecuteFunc
}

func waitVerbs() []waitVerb {
	base := func() *unstructured.Unstructured {
		return oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	}
	annotated := func(annotation string) *unstructured.Unstructured {
		o := base()
		_ = unstructured.SetNestedField(o.Object, "client-secret-1", "metadata", "annotations", annotation)
		return o
	}
	flags := map[string]string{"timeout": "30ms"}
	oldSecret := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}}
	return []waitVerb{
		{
			name: "create", skip: 0, unfinished: oidcClientObj("my-app", "", nil),
			abortRecovery: recoveryHint("my-app"),
			plan: func(t *testing.T, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				_, exec, err := planCreate(t, NewCreate(stubClusterClientFactory{c: c}, stubRootFactory{}, stubServerBase{}),
					oidcInput{args: []string{"my-app"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "add-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretCreate),
			abortRecovery: "check " + recoveryHint("my-app") + "; if a new key landed its value was never shown - rotate that key by name",
			plan: func(t *testing.T, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				_, exec, err := planAddSecret(t, NewAddSecret(stubClusterClientFactory{c: c}),
					oidcInput{args: []string{"my-app"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "remove-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretRemove),
			abortSecretName: "client-secret-1",
			abortRecovery:   recoveryHint("my-app") + " first; the removal landed iff the key is gone; re-run only while the key remains",
			plan: func(t *testing.T, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				_, exec, err := planRemoveSecret(t, NewRemoveSecret(stubClusterClientFactory{c: c}),
					oidcInput{args: []string{"my-app", "client-secret-1"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
		{
			name: "rotate-secret", skip: 1, planObj: base(), unfinished: annotated(annotationSecretRegenerate),
			retryableTimeout: true, abortSecretName: "client-secret-1", abortRecovery: recoveryHint("my-app"),
			plan: func(t *testing.T, c *scriptedPollClient) runtime.ExecuteFunc {
				t.Helper()
				c.secretResp = oldSecret
				_, exec, err := planRotateSecret(t, NewRotateSecret(stubClusterClientFactory{c: c}),
					oidcInput{args: []string{"my-app", "client-secret-1"}, flags: flags})
				require.NoError(t, err)
				return exec
			},
		},
	}
}

func (v waitVerb) run(t *testing.T, script []pollStep, blockAtEnd bool) (*scriptedPollClient, *clierr.Error) {
	t.Helper()
	withFastPoll(t)
	c := &scriptedPollClient{
		fakeOIDCClient: &fakeOIDCClient{},
		planObj:        v.planObj, skip: v.skip, script: script, blockAtEnd: blockAtEnd,
	}
	exec := v.plan(t, c)
	_, err := exec(t.Context())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	return c, ce
}

// TestOIDCVerbWaitEndsAtOnceOnAuthAndTLSFailures asserts that for every verb
// that waits on the OIDCClient an authentication or certificate failure of
// a poll Get ends the wait at that Get with its code, never retryable, and
// carries the verb's committed details (the verb already wrote to the
// server), in place of any other details the error had.
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
		for _, tt := range codes {
			t.Run(v.name+" "+tt.name, func(t *testing.T) {
				c, ce := v.run(t, []pollStep{{err: tt.err}}, false)
				assert.Equal(t, tt.err.Code, ce.Code)
				assert.Equal(t, tt.err.Hint, ce.Hint)
				assert.False(t, ce.Retryable)
				assert.Equal(t, v.skip+1, c.calls, "the wait ended at its first poll Get")
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

// TestOIDCVerbWaitAbortKeepsTheTextOfAWrappedError asserts that for every
// verb an authentication failure that a wrapper added text to ends the wait
// with the wrapper's text in its message and the same code.
func TestOIDCVerbWaitAbortKeepsTheTextOfAWrappedError(t *testing.T) {
	for _, v := range waitVerbs() {
		t.Run(v.name, func(t *testing.T) {
			wrapped := fmt.Errorf("get oidc-client my-app: %w", clierr.New(clierr.CodeAuth, "the server rejected the credential (401)"))
			_, ce := v.run(t, []pollStep{{err: wrapped}}, false)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Equal(t, "get oidc-client my-app: the server rejected the credential (401)", ce.Message)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.True(t, details.Committed)
		})
	}
}

// TestOIDCVerbWaitRetriesForbiddenAndNamesTheLastError asserts that for
// every verb a 403 is retried until the timeout, the timeout message ends
// with the last error and the timeout keeps its retryable flag.
func TestOIDCVerbWaitRetriesForbiddenAndNamesTheLastError(t *testing.T) {
	for _, v := range waitVerbs() {
		t.Run(v.name, func(t *testing.T) {
			c, ce := v.run(t, []pollStep{{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")}}, false)
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

// TestOIDCVerbWaitTimeoutAfterAnErrorThenAnObjectNamesNoError asserts that
// for every verb a later poll Get that returns an object clears the last
// error: the timeout message has no last error part.
func TestOIDCVerbWaitTimeoutAfterAnErrorThenAnObjectNamesNoError(t *testing.T) {
	for _, v := range waitVerbs() {
		t.Run(v.name, func(t *testing.T) {
			_, ce := v.run(t, []pollStep{
				{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")},
				{obj: v.unfinished},
			}, false)
			assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
			assert.NotContains(t, ce.Message, "last error")
		})
	}
}

// TestOIDCVerbWaitTimeoutDuringAGetNamesNoCutError asserts that for every
// verb a poll Get cut by the wait's own deadline is not recorded as the
// last error, while an error from an earlier Get that ended with the context
// live stays named.
func TestOIDCVerbWaitTimeoutDuringAGetNamesNoCutError(t *testing.T) {
	for _, v := range waitVerbs() {
		t.Run(v.name+" no earlier error", func(t *testing.T) {
			_, ce := v.run(t, nil, true)
			assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app")
			assert.NotContains(t, ce.Message, "last error")
			assert.NotContains(t, ce.Message, "deadline exceeded")
		})
		t.Run(v.name+" earlier forbidden", func(t *testing.T) {
			_, ce := v.run(t, []pollStep{{err: clierr.New(clierr.CodeRBAC, "forbidden (403)")}}, true)
			assert.Contains(t, ce.Message, "; last error: RBAC: forbidden (403)")
			assert.NotContains(t, ce.Message, "deadline exceeded")
		})
	}
}
