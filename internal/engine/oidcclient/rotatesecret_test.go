package oidcclient

import (
	"bytes"
	"context"
	"errors"
	"testing"
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

// TestRotateSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite pins that
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

// TestRotateSecretPlanOmittedNameSingleSecretAutoSelects pins the one
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

// TestRotateSecretPlanOmittedNameAmbiguousIsValidation pins the reject
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

// TestRotateSecretPlanNamedSecretNotFoundListsCandidates pins the typo
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
// pins the FORBIDDEN-at-read contract: a caller who cannot read the
// backing Secret at Plan time cannot be delivered the new value either, so
// this fails BEFORE the mutation — ordinary RBAC, no committed details.
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
		"  secret client-secret-1 rotated\n"+
		"  client secret: new-value   (shown once — store it now)\n", res.Text)
}

// TestRotateSecretThroughRunMutatingKeepsPlanWarningAndSuppressesShownOnce
// drives rotate-secret through the REAL pipeline (runtime.Run, not a direct
// exec() call) under default format with --apply --yes: runMutating
// prepends the Plan-time Effect.Warnings ("regenerates secret ...; the
// current value stops working") in front of the exec-time Result.Warnings
// (the shown-once note), so the shown-once note does NOT land at
// Warnings[0] once merged. This proves the note is excluded from the
// table-family stderr mirror only by value, not by position, while the
// unrelated Plan-time advisory — the one warning an operator must not
// lose — still reaches stderr.
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
	assert.NotContains(t, errBuf.String(), "shown exactly once",
		"the shown-once note must not double-print to stderr")
	assert.Contains(t, out.String(), "client secret: new-value   (shown once — store it now)")
}

// hangingSecretAfterFirstRead's secrets-GVR Get returns normally the first
// time (Plan's correlation read), then blocks on its context until it is
// canceled — proves rotate's per-tick correlation read inside check() is
// bound by waitForOIDCClient's own deadline, not the command's outer
// (test-lifetime, effectively unbounded) context.
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

// TestRotateSecretExecPerTickReadHonorsTimeout pins that check()'s
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
		t.Fatal("exec did not return within --timeout; the per-tick Secret read is not bound by the deadline")
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
// err on every read after — for pinning the never-recovers-before-deadline
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

// TestRotateSecretExecTransientSecretReadRetriesUntilSuccess pins that
// check()'s per-tick Secret read must tolerate a transient error and keep
// polling, mirroring waitForOIDCClient's own contract for its OIDCClient
// Get — both a clierr.Error explicitly classified Retryable and a raw
// (unclassified) transport error count as transient.
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

// TestRotateSecretExecPerTickRBACReadIsTerminal pins the other half of the
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

// TestRotateSecretExecTimeoutAfterConsumptionCarriesCommittedDetails pins
// that when the annotation is already consumed but the Secret stays
// unreadable on every tick, the eventual timeout must still carry
// committed context — the rotation may have succeeded even though
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

// TestRotateSecretExecTimeoutIsRetryable pins rotate's ONE divergence from
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
	assert.Contains(t, details.Recovery, "rotate the same name again")
	// The header already carries "(GENERIC, retryable)" for this error; the
	// projection's own recover line must echo the SAME recovery verbatim,
	// never a generic "do not retry" that would contradict it.
	assert.Equal(t, []string{
		"committed: OIDCClient my-app — the change may already be on the server",
		"recover: " + details.Recovery,
	}, details.TextDetails())
}

// TestRotateSecretUnchangedValueProjectsCommittedDetails pins the
// should-be-impossible rotate-unchanged terminal error: non-retryable, and
// its projection echoes the same re-check recovery TextDetails renders —
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
		"committed: OIDCClient my-app — the change may already be on the server",
		"recover: " + recovery,
	}, details.TextDetails())
}
