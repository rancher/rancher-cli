package oidcclient

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func planAddSecret(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	return mc.Plan(t.Context(), in)
}

func TestAddSecretDescriptorDeclaresCapabilityAndOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewAddSecret(stubOIDCFactory{}).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "oidc-provider", d.Capability.Feature)
	assert.Equal(t, runtime.DeliveryOnceOnly, d.Delivery)
	assert.NotContains(t, d.OutputPolicy.AllowedFormats, "name")
	assert.Equal(t, "true", d.Destructive)
}

// TestAddSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite asserts the
// never-provisioned VALIDATION branch: a client created while the
// controller was off has no status.clientID, and add-secret must refuse
// BEFORE the backing-Secret GET or any dry-run/update - zero secret reads,
// zero mutation requests.
func TestAddSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "", nil)
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{obj}}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Zero(t, client.secretCalls, "the delivery preflight must not run for a never-provisioned client")
	assert.Zero(t, client.createCalls, "no dry-run/update may fire before the never-provisioned check")
}

// TestAddSecretPlanPreflightRBACIsOrdinaryPreMutationRefusal asserts the
// manage-oidc-clients-only persona: the backing-Secret preflight failing
// with RBAC/NOT_FOUND is an ORDINARY pre-mutation error - no committed
// details (nothing mutated), and zero annotation writes.
func TestAddSecretPlanPreflightRBACIsOrdinaryPreMutationRefusal(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj},
		secretErr:     clierr.New(clierr.CodeRBAC, "forbidden"),
	}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Nil(t, ce.Details, "a pre-mutation preflight refusal must carry no committed-mutation details")
	assert.Zero(t, client.createCalls, "zero mutation requests: the dry-run/update must not run")
}

func TestAddSecretExecSuccessDeliversTheOneNewKey(t *testing.T) {
	withFastPoll(t)
	afterAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(afterAnnotated.Object, "true", "metadata", "annotations", annotationSecretCreate)
	afterConsumed := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{afterAnnotated, afterConsumed},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-2": b64("new-secret-value")},
		}},
	}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "OIDCClientSecretAdded", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "client-abc", data["clientID"])
	assert.Equal(t, "client-secret-2", data["secretName"])
	assert.Equal(t, "new-secret-value", data["clientSecret"])
	require.NotNil(t, res.CommittedContext)
	assert.Equal(t, "client-secret-2", res.CommittedContext.SecretName)
	assert.Equal(t, "oidcclient/my-app patched\n"+
		"  secret client-secret-2 added\n"+
		"  client secret: new-secret-value   (shown once - store it now)\n", res.Text)
}

// TestAddSecretThroughRunMutatingKeepsPlanWarningAndSuppressesShownOnce
// drives add-secret through the REAL pipeline (runtime.Run, not a direct
// exec() call) under default format with --apply --yes: runMutating
// prepends the Plan-time Effect.Warnings ("creates a new client secret on
// ...") in front of the exec-time Result.Warnings (the shown-once note),
// so the shown-once note does NOT land at Warnings[0] once merged. This
// proves the note is still excluded from the table-family stderr mirror
// only by value, not by position, while the unrelated Plan-time advisory
// still reaches stderr as any other warning would.
func TestAddSecretThroughRunMutatingKeepsPlanWarningAndSuppressesShownOnce(t *testing.T) {
	withFastPoll(t)
	afterAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(afterAnnotated.Object, "true", "metadata", "annotations", annotationSecretCreate)
	afterConsumed := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{afterAnnotated, afterConsumed},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-2": b64("new-secret-value")},
		}},
	}
	cmd := NewAddSecret(stubOIDCFactory{c: client})

	var out, errBuf bytes.Buffer
	status, ce := runtime.Run(t.Context(), cmd, oidcInput{args: []string{"my-app"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "", Apply: true, Yes: true, Capability: alwaysProceedCapabilityChecker{}},
		safety.Binding{Contract: safety.ContractInternal, Stability: safety.Beta})
	require.Nil(t, ce)
	assert.Zero(t, status)

	assert.Contains(t, errBuf.String(), "# warning: creates a new client secret on my-app\n",
		"the Plan-time safety advisory must still reach stderr after the merge")
	assert.NotContains(t, errBuf.String(), "shown exactly once",
		"the shown-once note must not double-print to stderr")
	assert.Contains(t, out.String(), "client secret: new-secret-value   (shown once - store it now)")
}

// TestAddSecretExecAmbiguousTransitionsAreTerminal asserts the ambiguous
// key-set transitions: a concurrent REMOVE (baseline
// {1} -> observed {2} has exactly one new key, which a bare "one new key"
// check would wrongly bless), a concurrent TWO-ADD (two new keys) and a
// removal with no new key - each must fail at the first poll that sees
// it, non-retryable, never deliver a possibly-wrong key, and name the keys
// that appeared and disappeared.
func TestAddSecretExecAmbiguousTransitionsAreTerminal(t *testing.T) {
	const suffix = "); another change to this client, or Rancher's controller handling the annotation twice, can do this"
	const changedBy = "oidc-client my-app: its client secrets changed by more than the one new secret add-secret asked for ("
	const noneAdded = "oidc-client my-app: add-secret expected exactly one new client secret, but none was added and some were removed ("
	cases := []struct {
		name   string
		after  *unstructured.Unstructured
		prefix string
		change string
	}{
		{"concurrent remove: baseline key missing", oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-2": map[string]any{},
		}), changedBy, "added: client-secret-2; removed: client-secret-1"},
		{"concurrent two-add: two new keys", oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{}, "client-secret-3": map[string]any{},
		}), changedBy, "added: client-secret-2, client-secret-3"},
		{"concurrent remove: no new key", oidcClientObj("my-app", "client-abc", map[string]any{}),
			noneAdded, "removed: client-secret-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastPoll(t)
			before := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
			client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{before, tc.after}}
			cmd := NewAddSecret(stubOIDCFactory{c: client})
			_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}})
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.prefix+tc.change+suffix, ce.Message)
			assert.Equal(t, 2, client.pollCalls, "the first poll that sees the change ends the wait")
			assert.False(t, ce.Retryable)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.True(t, details.Committed)
		})
	}
}

// TestAddSecretExecWaitsBetweenTheControllersWrites asserts that a
// consumed create request with an unchanged key set keeps the wait going:
// Rancher's controller removes the annotation before it updates status, so
// polls can see that state before the new key lands.
func TestAddSecretExecWaitsBetweenTheControllersWrites(t *testing.T) {
	withFastPoll(t)
	before := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	annotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(annotated.Object, "true", "metadata", "annotations", annotationSecretCreate)
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	added := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{before, annotated, consumed, consumed, added},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-2": b64("new-secret-value")},
		}},
	}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "client-secret-2", data["secretName"])
	assert.Equal(t, "new-secret-value", data["clientSecret"])
	assert.Equal(t, 5, client.pollCalls, "the wait ran through both in-between polls")
}

// TestAddSecretExecTimeoutNamesTheRemovedAnnotation asserts the timeout
// message when the create annotation was removed and no new client secret
// ever appeared.
func TestAddSecretExecTimeoutNamesTheRemovedAnnotation(t *testing.T) {
	withFastPoll(t)
	before := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{before, consumed}}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, "timed out waiting for oidc-client my-app to add a secret: the cattle.io/oidc-client-secret-create annotation was removed but no new client secret appeared", ce.Message)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Contains(t, details.Recovery, "rotate that key by name")
}

// TestAddSecretExecTimeoutWithoutAnObservedObject asserts that a wait whose
// every read fails until the timeout ends with the plain timeout message
// and no panic: no object was ever read, so there is no request state to
// name.
func TestAddSecretExecTimeoutWithoutAnObservedObject(t *testing.T) {
	withFastPoll(t)
	before := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{before, before},
		pollErrs:      []error{nil, errors.New("connection refused")},
	}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, "timed out waiting for oidc-client my-app to add a secret", ce.Message)
	assert.Greater(t, client.pollCalls, 1, "the wait polled and every poll failed")
}

func TestAddSecretExecTimeoutIsNonRetryable(t *testing.T) {
	withFastPoll(t)
	before := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "true", "metadata", "annotations", annotationSecretCreate)
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{before, stillAnnotated}}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	_, exec, err := planAddSecret(t, cmd, oidcInput{args: []string{"my-app"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, "timed out waiting for oidc-client my-app to add a secret", ce.Message,
		"a request the controller never consumed gets the plain timeout")
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	// Non-retryable: the projection's recover line must never claim a
	// blind retry is safe - it names a re-check, then a rotate-by-name.
	assert.Equal(t, []string{
		"committed: OIDCClient my-app - the change may already be on the server",
		"recover: " + details.Recovery,
	}, details.TextDetails())
	assert.Contains(t, details.Recovery, "rotate that key by name")
}

func TestAddSecretOutputPolicyRejectsNameWithZeroRequests(t *testing.T) {
	client := &fakeOIDCClient{}
	cmd := NewAddSecret(stubOIDCFactory{c: client})
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, oidcInput{args: []string{"my-app"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "name", Apply: true, Yes: true, Capability: alwaysProceedCapability{}}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Zero(t, client.pollCalls)
	assert.Zero(t, client.secretCalls)
	assert.Zero(t, client.createCalls)
}
