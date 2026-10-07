package oidcclient

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func planRemoveSecret(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	return mc.Plan(t.Context(), in)
}

// TestRemoveSecretDescriptorHasNoOnceOnlySeam asserts the one bespoke command
// that delivers no once-only value: remove-secret carries Capability but
// NEITHER OutputPolicy NOR Delivery - it is not in the once-only inventory.
func TestRemoveSecretDescriptorHasNoOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewRemoveSecret(stubOIDCFactory{}).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Empty(t, string(d.Delivery))
	assert.Empty(t, d.OutputPolicy.AllowedFormats)
	require.Len(t, d.Args, 2)
	assert.True(t, d.Args[1].Required, "secret-name is always required for remove")
}

func TestRemoveSecretPlanNameRequiredAndValidated(t *testing.T) {
	t.Parallel()
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{obj}}
	cmd := NewRemoveSecret(stubOIDCFactory{c: client})
	_, exec, err := planRemoveSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-typo"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "client-secret-1", ce.Candidates[0].ID)
	assert.Zero(t, client.createCalls, "no dry-run/update before name validation passes")
}

func TestRemoveSecretExecSuccess(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-2", "metadata", "annotations", annotationSecretRemove)
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})

	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated, consumed}}
	cmd := NewRemoveSecret(stubOIDCFactory{c: client})
	_, exec, err := planRemoveSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-2"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "OIDCClientSecretRemoved", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "client-abc", data["clientID"])
	assert.Equal(t, "client-secret-2", data["secretName"])
	assert.Nil(t, res.CommittedContext, "remove delivers no once-only value")
	assert.Equal(t, "oidcclient/my-app patched\n"+
		"  secret client-secret-2 removed - anything using it stops working\n", res.Text)
}

// TestRemoveSecretExecWaitsBetweenTheControllersWrites asserts that a
// consumed removal request whose status still lists the key keeps the wait
// going: Rancher's controller removes the annotation before it updates
// status, so polls can see that state before the key leaves status.
func TestRemoveSecretExecWaitsBetweenTheControllersWrites(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-2", "metadata", "annotations", annotationSecretRemove)
	consumedWithKey := oidcClientObj("my-app", "client-abc", map[string]any{
		"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
	})
	consumed := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})

	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{
		planObj, stillAnnotated, consumedWithKey, consumedWithKey, consumed,
	}}
	cmd := NewRemoveSecret(stubOIDCFactory{c: client})
	_, exec, err := planRemoveSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-2"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "OIDCClientSecretRemoved", res.Kind)
	assert.Equal(t, 5, client.pollCalls, "the wait ran through both in-between polls")
}

// TestRemoveSecretExecTimeoutIsNonRetryable asserts remove's contract: the
// timeout recovery is a read-first, never "re-run to confirm" (strict
// Plan-time validation would fail a re-run whose first attempt landed
// after the last poll).
func TestRemoveSecretExecTimeoutIsNonRetryable(t *testing.T) {
	withFastPoll(t)
	planObj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	stillAnnotated := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	_ = unstructured.SetNestedField(stillAnnotated.Object, "client-secret-1", "metadata", "annotations", annotationSecretRemove)
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{planObj, stillAnnotated}}
	cmd := NewRemoveSecret(stubOIDCFactory{c: client})
	_, exec, err := planRemoveSecret(t, cmd, oidcInput{args: []string{"my-app", "client-secret-1"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Contains(t, details.Recovery, "re-run only while the key remains")
	// Non-retryable: the projection's recover line must never invite an
	// unconditional retry - it names the read-first condition verbatim.
	assert.Equal(t, []string{
		"committed: OIDCClient my-app - the change may already be on the server",
		"recover: " + details.Recovery,
	}, details.TextDetails())
}
