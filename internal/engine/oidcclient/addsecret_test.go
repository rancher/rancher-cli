package oidcclient

import (
	"bytes"
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

// TestAddSecretPlanNeverProvisionedFailsBeforeAnyReadOrWrite pins the
// never-provisioned VALIDATION branch: a client created while the
// controller was off has no status.clientID, and add-secret must refuse
// BEFORE the backing-Secret GET or any dry-run/update — zero secret reads,
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

// TestAddSecretPlanPreflightRBACIsOrdinaryPreMutationRefusal pins the
// manage-oidc-clients-only persona: the backing-Secret preflight failing
// with RBAC/NOT_FOUND is an ORDINARY pre-mutation error — no committed
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
		"  client secret: new-secret-value   (shown once — store it now)\n", res.Text)
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
	assert.Contains(t, out.String(), "client secret: new-secret-value   (shown once — store it now)")
}

// TestAddSecretExecAmbiguousTransitionsAreTerminal pins the two ambiguous
// key-set transitions: a concurrent REMOVE (baseline
// {1} -> observed {2} has exactly one new key, which a bare "one new key"
// check would wrongly bless) and a concurrent TWO-ADD (two new keys) — both
// must fail immediately, non-retryable, never deliver a possibly-wrong key.
func TestAddSecretExecAmbiguousTransitionsAreTerminal(t *testing.T) {
	cases := []struct {
		name  string
		after *unstructured.Unstructured
	}{
		{"concurrent remove: baseline key missing", oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-2": map[string]any{},
		})},
		{"concurrent two-add: two new keys", oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{}, "client-secret-3": map[string]any{},
		})},
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
			ce, ok := err.(*clierr.Error)
			require.True(t, ok)
			assert.False(t, ce.Retryable)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.True(t, details.Committed)
		})
	}
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
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	// Non-retryable: the projection's recover line must never claim a
	// blind retry is safe — it names a re-check, then a rotate-by-name.
	assert.Equal(t, []string{
		"committed: OIDCClient my-app — the change may already be on the server",
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
