package oidcclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func planRotateSecret(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	return mc.Plan(t.Context(), in)
}

func TestRotateSecretDescriptorDeclaresCapabilityAndOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewRotateSecret(stubOIDCFactory{}).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, runtime.DeliveryOnceOnly, d.Delivery)
	assert.NotContains(t, d.OutputPolicy.AllowedFormats, "name")
	assert.False(t, d.Args[1].Required, "secret-name is optional at the descriptor level")
}

// TestRotateSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite asserts that
// an unprovisioned client (empty status.clientID) must fail VALIDATION
// before any Secret read or write: a client with status.clientSecrets
// populated but clientID empty/mistyped (a manual status patch) must not
// reach readClientSecret with an empty resource name. The guard must fire
// before resolveSecretName or any Secret read.
func TestRotateSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{obj}}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Zero(t, client.secretCalls, "no correlation read before the provisioned guard")
	assert.Zero(t, client.createCalls, "no dry-run/update may fire before the provisioned guard")
}

// TestRotateSecretPlanOmittedNameSingleSecretAutoSelects asserts the one
// deterministic omission case.
func TestRotateSecretPlanOmittedNameSingleSecretAutoSelects(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-1": b64("old-value")},
		}},
	}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	assert.NotNil(t, exec)
}

// TestRotateSecretPlanOmittedNameAmbiguousIsValidation asserts the reject
// cases: omitting secret-name with zero or multiple secrets is VALIDATION,
// listing the actual names as candidates.
func TestRotateSecretPlanOmittedNameAmbiguousIsValidation(t *testing.T) {
	cases := []struct {
		name    string
		secrets map[string]any
	}{
		{"zero secrets", nil},
		{"multiple secrets", map[string]any{"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := oidcClientObj("my-app", "client-abc", tc.secrets)
			client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{obj}}
			cmd := NewRotateSecret(stubOIDCFactory{c: client})
			_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app"}})
			require.Error(t, err)
			assert.Nil(t, exec)
			ce, ok := err.(*clierr.Error)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Zero(t, client.secretCalls, "no correlation read before the name is resolved")
		})
	}
}

// TestRotateSecretPlanNamedSecretNotFoundListsCandidates asserts the typo
// case: the controller silently skips an unknown name, so this must fail
// at Plan with the real names as candidates.
func TestRotateSecretPlanNamedSecretNotFoundListsCandidates(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{obj}}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-typo"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "client-secret-1", ce.Candidates[0].ID)
}

// TestRotateSecretPlanCorrelationReadFailureIsOrdinaryPreMutationRefusal
// asserts the FORBIDDEN-at-read contract: a caller who cannot read the
// backing Secret at Plan time cannot be delivered the new value either, so
// this fails BEFORE the mutation - ordinary RBAC, no committed details.
func TestRotateSecretPlanCorrelationReadFailureIsOrdinaryPreMutationRefusal(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj},
		secretErr:     clierr.New(clierr.CodeRBAC, "forbidden"),
	}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Nil(t, ce.Details, "a pre-mutation refusal must carry no committed-mutation details")
	assert.Zero(t, client.createCalls, "zero mutation requests: dry-run/update must not run")
}

func TestRotateSecretExecSuccessDeliversNewValue(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-1", "metadata", "annotations", annotationSecretRegenerate)
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})

	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated, consumed}}
	// Return "old-value" for the Plan-time correlation read, "new-value" afterward.
	secretReadCount := 0
	client.secretRespFunc = func() *unstructured.Unstructured {
		secretReadCount++
		if secretReadCount == 1 {
			return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}}
		}
		return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("new-value")}}}
	}

	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "OIDCClientSecretRotated", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "new-value", data["clientSecret"])
	assert.Equal(t, "client-secret-1", data["secretName"])
	require.NotNil(t, res.CommittedContext)
	assert.Equal(t, "oidcclient/my-app patched\n"+
		"  secret client-secret-1 regenerated\n"+
		"  client secret: new-value   (shown once - store it now)\n", res.Text)
}

// TestRotateSecretThroughRunMutatingKeepsPlanWarningAndSuppressesShownOnce
// drives rotate-secret through the REAL pipeline (runtime.Run, not a direct
// exec() call) under default format with --apply --yes: runMutating
// prepends the Plan-time Effect.Warnings ("regenerates secret ...; the
// current value stops working") in front of the exec-time Result.Warnings
// (the shown-once note), so the shown-once note does NOT land at
// Warnings[0] once merged. This proves the note is excluded from the
// table-family stderr mirror only by value, not by position, while the
// unrelated Plan-time advisory - the one warning an operator must not
// lose - still reaches stderr.
func TestRotateSecretThroughRunMutatingKeepsPlanWarningAndSuppressesShownOnce(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-1", "metadata", "annotations", annotationSecretRegenerate)
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})

	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated, consumed}}
	secretReadCount := 0
	client.secretRespFunc = func() *unstructured.Unstructured {
		secretReadCount++
		if secretReadCount == 1 {
			return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}}
		}
		return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("new-value")}}}
	}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})

	var out, errBuf bytes.Buffer
	status, ce := runtime.Run(t.Context(), cmd, oidcInput{args: []string{"my-app", "client-secret-1"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "", Apply: true, Yes: true, Capability: alwaysProceedCapabilityChecker{}},
		safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta})
	require.Nil(t, ce)
	assert.Zero(t, status)

	assert.Contains(t, errBuf.String(),
		"# warning: regenerates secret client-secret-1 on my-app; the current value stops working\n",
		"the Plan-time safety advisory must still reach stderr after the merge")
	assert.NotContains(t, errBuf.String(), "this client secret is shown once - store it now",
		"the shown-once note must not double-print to stderr")
	assert.Contains(t, out.String(), "client secret: new-value   (shown once - store it now)")
}

// hangingSecretAfterFirstRead's secrets-GVR Get returns normally the first
// time (Plan's correlation read), then blocks on its context until it is
// canceled - proves rotate's read of the client Secret inside check() is
// bound by the wait's own deadline, not the command's outer (test-lifetime,
// unbounded) context.
type hangingSecretAfterFirstRead struct {
	*fakeOIDCClient
	secretGetCalls int
}

type stubClusterClientFactory struct{ c engine.ClusterClient }

func (f stubClusterClientFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return f.c, nil
}
func (f stubClusterClientFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	return s, nil
}

func (h *hangingSecretAfterFirstRead) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource != "secrets" {
		return h.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	h.secretGetCalls++
	if h.secretGetCalls == 1 {
		return h.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRotateSecretExecPerTickReadHonorsTimeout asserts that check()'s
// correlation read is handed the deadline-bound context, not execute's
// outer (unbounded) ctx: exec returns promptly once --timeout elapses
// instead of hanging indefinitely on a stuck read.
func TestRotateSecretExecPerTickReadHonorsTimeout(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-1", "metadata", "annotations", annotationSecretRegenerate)
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})

	client := &hangingSecretAfterFirstRead{fakeOIDCClient: &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated, consumed},
		secretResp:    &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}},
	}}
	cmd := NewRotateSecret(stubClusterClientFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}, flags: map[string]string{"timeout": "30ms"}})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, execErr := exec(t.Context())
		done <- execErr
	}()
	select {
	case execErr := <-done:
		require.Error(t, execErr, "exec must fail once the deadline-bound read is canceled")
	case <-time.After(2 * time.Second):
		t.Fatal("exec did not return within --timeout; the read of the client Secret is not bound by the deadline")
	}
}

// secretErrAfterFirstRead succeeds on the first secrets-GVR Get (Plan's
// correlation read), returns errs[i] for the (i+1)th read after that, and
// falls back to okValue once errs is exhausted.
type secretErrAfterFirstRead struct {
	*fakeOIDCClient
	secretGetCalls int
	errs           []error
	okValue        string
}

func (f *secretErrAfterFirstRead) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource != "secrets" {
		return f.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	f.secretGetCalls++
	if f.secretGetCalls == 1 {
		return f.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	if idx := f.secretGetCalls - 2; idx < len(f.errs) {
		return nil, f.errs[idx]
	}
	return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64(f.okValue)}}}, nil
}

// alwaysErrSecretRead succeeds on the first secrets-GVR Get, then returns
// err on every read after - for asserting the never-recovers-before-deadline
// case, where secretErrAfterFirstRead's exhaustion-then-success shape would
// let the wait succeed instead of time out.
type alwaysErrSecretRead struct {
	*fakeOIDCClient
	calls int
	err   error
}

func (f *alwaysErrSecretRead) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource != "secrets" {
		return f.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	f.calls++
	if f.calls == 1 {
		return f.fakeOIDCClient.Get(ctx, gvr, ns, name)
	}
	return nil, f.err
}

// TestRotateSecretExecTransientSecretReadRetriesUntilSuccess asserts that
// check()'s read of the client Secret must tolerate a transient error and
// keep waiting, as the wait does for a failed request for the OIDCClient -
// both a clierr.Error explicitly classified Retryable and a raw
// (unclassified) transport error count as transient. This client cannot
// watch, so the wait polls.
func TestRotateSecretExecTransientSecretReadRetriesUntilSuccess(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"clierr retryable", clierr.Retryable(clierr.CodeGeneric, "temporary blip")},
		{"raw transport error", errors.New("connection reset")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastPoll(t)
			planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
			consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
			client := &secretErrAfterFirstRead{
				fakeOIDCClient: &fakeOIDCClient{
					pollResponses: []*unstructured.Unstructured{planObj, consumed},
					secretResp:    &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}},
				},
				errs:    []error{tc.err},
				okValue: "new-value",
			}
			cmd := NewRotateSecret(stubClusterClientFactory{c: client})
			_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}})
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err, "a transient read must not end the wait")
			data, ok := res.Data.(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "new-value", data["clientSecret"])
		})
	}
}

// TestRotateSecretExecPerTickRBACReadIsTerminal asserts the other half of the
// classification: a definitive, non-retryable error (RBAC) must still end
// the wait immediately on the FIRST post-annotation read, never tolerated
// as if it were transient.
func TestRotateSecretExecPerTickRBACReadIsTerminal(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &secretErrAfterFirstRead{
		fakeOIDCClient: &fakeOIDCClient{
			pollResponses: []*unstructured.Unstructured{planObj, consumed},
			secretResp:    &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}},
		},
		errs: []error{clierr.New(clierr.CodeRBAC, "forbidden")},
	}
	cmd := NewRotateSecret(stubClusterClientFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, 2, client.secretGetCalls, "terminal on the first post-annotation read, no retries")
}

// TestRotateSecretExecTimeoutAfterConsumptionCarriesCommittedDetails asserts
// that when the annotation is already consumed but the Secret stays
// unreadable on every tick, the eventual timeout must still carry
// committed context - the rotation may have succeeded even though
// delivery never confirmed it.
func TestRotateSecretExecTimeoutAfterConsumptionCarriesCommittedDetails(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &alwaysErrSecretRead{
		fakeOIDCClient: &fakeOIDCClient{
			pollResponses: []*unstructured.Unstructured{planObj, consumed},
			secretResp:    &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}},
		},
		err: clierr.Retryable(clierr.CodeGeneric, "still blipping"),
	}
	cmd := NewRotateSecret(stubClusterClientFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed, "the annotation was already consumed; the rotation may have succeeded even though delivery timed out")
}

// TestRotateSecretExecTimeoutIsRetryable asserts rotate's ONE divergence from
// create/add/remove: its timeout IS retryable, because the stated recovery
// is literally rotating the same key again.
func TestRotateSecretExecTimeoutIsRetryable(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-1", "metadata", "annotations", annotationSecretRegenerate)
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated},
		secretResp:    &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64("old-value")}}},
	}
	cmd := NewRotateSecret(stubOIDCFactory{c: client})
	_, exec, err := planRotateSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.True(t, ce.Retryable, "rotate's timeout must be retryable")
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "once the controller is healthy, rotate the same secret again (the command previews it; add --apply --yes after reading the preview): "+clierr.Rancher+" oidc-client rotate-secret 'my-app' 'client-secret-1' --dry-run", details.Recovery)
	// The header already carries "(GENERIC, retryable)" for this error; the
	// projection's own recover line must echo the SAME recovery verbatim,
	// never a generic "do not retry" that would contradict it.
	assert.Equal(t, []string{
		"committed: OIDCClient my-app - the change may already be on the server",
		"recover: " + details.Recovery,
	}, details.TextDetails())
}

// TestRotateSecretUnchangedValueProjectsCommittedDetails asserts the
// should-be-impossible rotate-unchanged terminal error: non-retryable, and
// its projection echoes the same re-check recovery TextDetails renders -
// never an invitation to retry a rotation whose value never actually moved.
func TestRotateSecretUnchangedValueProjectsCommittedDetails(t *testing.T) {
	t.Parallel()
	recovery := recoveryHint("my-app")
	ce := rotateUnchangedValueError("my-app", "client-secret-1", recovery)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, recovery, details.Recovery)
	assert.Equal(t, []string{
		"committed: OIDCClient my-app - the change may already be on the server",
		"recover: " + recovery,
	}, details.TextDetails())
}

func (f stubClusterClientFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// TestTransientSecretReadErrorIgnoresWrappedTerminalError asserts a wrapped
// non-retryable classified error is terminal, not transient.
func TestTransientSecretReadErrorIgnoresWrappedTerminalError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("x: %w", clierr.New(clierr.CodeRBAC, "no"))
	assert.False(t, isTransientSecretReadError(err))
}

// timedSecretReads wraps the watching client and answers the reads of the
// client Secret after the Plan's own: errs[i] for the read after the first
// that has an entry, then okValue. It records when each read after the first
// started, from the start of the run.
type timedSecretReads struct {
	*scriptedWatchClient
	errs    []error
	okValue string
	n       int
	reads   []time.Duration
}

func (c *timedSecretReads) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if gvr.Resource != "secrets" {
		return c.scriptedWatchClient.Get(ctx, gvr, ns, name)
	}
	c.n++
	if c.n == 1 {
		return c.scriptedWatchClient.Get(ctx, gvr, ns, name)
	}
	c.reads = append(c.reads, time.Since(c.base))
	if i := c.n - 2; i < len(c.errs) {
		return nil, c.errs[i]
	}
	return &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"client-secret-1": b64(c.okValue)}}}, nil
}

// TestRotateSecretWatchedChecksAgainEveryIntervalOnATransientSecretRead
// asserts that when the event that clears the regenerate annotation arrives
// 3 seconds into a watched wait, a transient error reading the client Secret
// makes the wait read it again every 2 seconds while the stream is open, with
// no other request for the OIDCClient than its one List and one watch; that a
// terminal read error and an unchanged value end the wait on the first read;
// and that a transient error that lasts to the timeout times out with the
// verb's retryable committed details.
func TestRotateSecretWatchedChecksAgainEveryIntervalOnATransientSecretRead(t *testing.T) {
	transient := func(n int) []error {
		errs := make([]error, n)
		for i := range errs {
			errs[i] = clierr.Retryable(clierr.CodeGeneric, "temporary blip")
		}
		return errs
	}
	seconds := func(secs ...int) []time.Duration {
		var out []time.Duration
		for _, s := range secs {
			out = append(out, time.Duration(s)*time.Second)
		}
		return out
	}
	tests := []struct {
		name      string
		timeout   string
		errs      []error
		okValue   string
		wantReads []time.Duration
		check     func(t *testing.T, v waitVerb, r waitRun)
	}{
		{"two transient errors then the new value", "60s", []error{clierr.Retryable(clierr.CodeGeneric, "temporary blip"), errors.New("connection reset")}, "new-value",
			seconds(3, 5, 7), func(t *testing.T, _ waitVerb, r waitRun) {
				t.Helper()
				require.NoError(t, r.err)
				data, ok := r.res.Data.(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "new-value", data["clientSecret"])
			}},
		{"a terminal read error ends the wait on the first read", "60s", []error{clierr.New(clierr.CodeRBAC, "forbidden")}, "new-value",
			seconds(3), func(t *testing.T, v waitVerb, r waitRun) {
				t.Helper()
				ce := assertCommitted(t, v, r.err)
				assert.Equal(t, clierr.CodeRBAC, ce.Code)
				assert.False(t, ce.Retryable)
			}},
		{"an unchanged value ends the wait on the first read", "60s", nil, "old-value",
			seconds(3), func(t *testing.T, v waitVerb, r waitRun) {
				t.Helper()
				ce := assertCommitted(t, v, r.err)
				assert.False(t, ce.Retryable)
				assert.Contains(t, ce.Message, "did not change")
			}},
		{"a transient error to the timeout", "12s", transient(20), "new-value",
			seconds(3, 5, 7, 9, 11), func(t *testing.T, v waitVerb, r waitRun) {
				t.Helper()
				ce := assertCommitted(t, v, r.err)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Contains(t, ce.Message, "timed out waiting for oidc-client my-app to regenerate secret client-secret-1")
				details, _ := ce.Details.(clierr.CommittedContextDetails)
				assert.Equal(t, "client-secret-1", details.SecretName)
				assert.Equal(t, rotateAgainRecovery("my-app", "client-secret-1"), details.Recovery)
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				setPollInterval(t, 2*time.Second)
				v := waitVerbsWithTimeout(tt.timeout)[3]
				require.Equal(t, "rotate-secret", v.name)
				consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
				var secrets *timedSecretReads
				r := v.exec(t.Context(), t, watchMode, waitSetup{
					script: []pollStep{{obj: v.unfinished}, {obj: consumed}},
					watch:  func(w *scriptedWatchClient) { w.eventDelay = 3 * time.Second },
					wrap: func(w *scriptedWatchClient) engine.ClusterClient {
						secrets = &timedSecretReads{scriptedWatchClient: w, errs: tt.errs, okValue: tt.okValue}
						return secrets
					},
				})
				tt.check(t, v, r)
				assert.Equal(t, tt.wantReads, secrets.reads)
				assert.Equal(t, []string{"list@0", "watch@0"}, r.client.log, "one List and one watch, no other request for the OIDCClient")
			})
		})
	}
}
