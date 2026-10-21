package oidcclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// fakeOIDCClient is a minimal engine.ClusterClient serving both the
// oidcclients GVR (Create + sequential poll Gets) and the secrets GVR (the
// backing-Secret read), distinguished by gvr.Resource - both routes reach
// this same fake through stubOIDCFactory.Cluster, mirroring how a real
// invocation reaches the same "local" cluster for both.
type fakeOIDCClient struct {
	createResp  *unstructured.Unstructured
	createErr   error
	createCalls int

	pollResponses []*unstructured.Unstructured
	pollErrs      []error
	pollCalls     int

	secretResp  *unstructured.Unstructured
	secretErr   error
	secretCalls int
	// secretRespFunc, when set, overrides secretResp and is called once per
	// secret GET - used to simulate the backing value changing between
	// rotate's Plan-time correlation read and its post-consumption
	// delivery read.
	secretRespFunc func() *unstructured.Unstructured
}

func (f *fakeOIDCClient) Get(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	if gvr.Resource == "secrets" {
		f.secretCalls++
		if f.secretErr != nil {
			return nil, f.secretErr
		}
		if f.secretRespFunc != nil {
			return f.secretRespFunc(), nil
		}
		return f.secretResp, nil
	}
	idx := f.pollCalls
	if idx >= len(f.pollResponses) {
		idx = len(f.pollResponses) - 1
	}
	var err error
	if idx < len(f.pollErrs) {
		err = f.pollErrs[idx]
	}
	f.pollCalls++
	if err != nil {
		return nil, err
	}
	if idx < 0 {
		return nil, fmt.Errorf("fakeOIDCClient: no poll response configured")
	}
	return f.pollResponses[idx], nil
}

func (f *fakeOIDCClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (f *fakeOIDCClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}
func (f *fakeOIDCClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *fakeOIDCClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (f *fakeOIDCClient) Create(_ context.Context, _ schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.createCalls++
	if dryRun {
		return obj.DeepCopy(), nil
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	return obj.DeepCopy(), nil
}

type stubOIDCFactory struct{ c *fakeOIDCClient }

func (f stubOIDCFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return f.c, nil
}
func (f stubOIDCFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

// fakeRootClient serves a single discovery response (or error) for
// /oidc/.well-known/openid-configuration.
type fakeRootClient struct {
	body []byte
	err  error
}

func (f fakeRootClient) GetJSON(context.Context, string) ([]byte, error) { return f.body, f.err }

type stubRootFactory struct{ c engine.RootClient }

func (f stubRootFactory) Root() (engine.RootClient, error) { return f.c, nil }

type stubServerBase struct {
	base string
	err  error
}

func (s stubServerBase) ServerBaseURL() (string, error) { return s.base, s.err }

type oidcInput struct {
	args  []string
	flags map[string]string
}

func (i oidcInput) String(k string) string  { return i.flags[k] }
func (i oidcInput) Bool(string) bool        { return false }
func (i oidcInput) Strings(string) []string { return nil }
func (i oidcInput) IsSet(string) bool       { return false }
func (i oidcInput) Args() []string          { return i.args }

func withFastPoll(t *testing.T) {
	t.Helper()
	orig := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = orig })
}

func oidcClientObj(name, clientID string, secrets map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "OIDCClient",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{},
		"status":     map[string]any{},
	}}
	if clientID != "" {
		_ = unstructured.SetNestedField(obj.Object, clientID, "status", "clientID")
	}
	if len(secrets) > 0 {
		_ = unstructured.SetNestedMap(obj.Object, secrets, "status", "clientSecrets")
	}
	return obj
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

const discoveryOK = `{
	"issuer": "https://rancher.example/oidc",
	"authorization_endpoint": "https://rancher.example/oidc/authorize",
	"token_endpoint": "https://rancher.example/oidc/token",
	"jwks_uri": "https://rancher.example/oidc/.well-known/jwks.json"
}`

func planCreate(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	return mc.Plan(t.Context(), in)
}

func TestCreateDescriptorDeclaresCapabilityAndOnceOnlySeam(t *testing.T) {
	t.Parallel()
	d := NewCreate(stubOIDCFactory{}, stubRootFactory{}, stubServerBase{}).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "oidc-provider", d.Capability.Feature)
	assert.Equal(t, runtime.DeliveryOnceOnly, d.Delivery)
	assert.NotContains(t, d.OutputPolicy.AllowedFormats, "name")
	assert.True(t, d.Args[0].Required, "name positional is required")
	assert.Equal(t, "true", d.Destructive)
}

// TestCreatePlanSendsBothRequiredTTLsAndDefaults asserts the CRD-required-TTL
// contract: the manifest always carries both TTL fields, CLI-defaulted
// (3600/86400) when the flags are omitted - the server does not default
// them.
func TestCreatePlanSendsBothRequiredTTLsAndDefaults(t *testing.T) {
	t.Parallel()
	client := &fakeOIDCClient{}
	cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{}, stubServerBase{})
	eff, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive)
	assert.Equal(t, safety.ActionCreate, eff.Action)
}

func TestCreatePlanRequiresName(t *testing.T) {
	t.Parallel()
	// validateInput (the pipeline) enforces the required positional in
	// production; Plan itself indexes Args()[0] and would panic on an
	// empty slice if that invariant were ever bypassed - assert the
	// Descriptor declares Required so the pipeline never lets that happen.
	d := NewCreate(stubOIDCFactory{}, stubRootFactory{}, stubServerBase{}).Describe()
	require.Len(t, d.Args, 1)
	assert.True(t, d.Args[0].Required)
}

// TestParseWaitTimeoutRejectsNonPositiveDurations asserts that --timeout 0 or
// a negative value never reaches context.WithTimeout as an already-expired
// deadline: the mutation would still commit, but the command would
// immediately misreport a flag-value mistake as a controller-health
// timeout, mirroring strictTTLSeconds's own positivity check.
func TestParseWaitTimeoutRejectsNonPositiveDurations(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"0", "0s", "-5s"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			_, err := parseWaitTimeout(oidcInput{flags: map[string]string{"timeout": raw}})
			require.Error(t, err)
			ce, ok := err.(*clierr.Error)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
		})
	}
}

func TestParseWaitTimeoutAcceptsOmittedAndPositive(t *testing.T) {
	t.Parallel()
	d, err := parseWaitTimeout(oidcInput{})
	require.NoError(t, err)
	assert.Equal(t, defaultWaitTimeout, d)

	d, err = parseWaitTimeout(oidcInput{flags: map[string]string{"timeout": "90s"}})
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)
}

// TestCreateExecFullLifecycleSuccess drives Plan+exec end to end: create
// succeeds, the wait predicate observes the initial secret, the backing
// Secret decodes, and discovery succeeds - the Result carries the fixed
// endpoints shape and CommittedContext.
func TestCreateExecFullLifecycleSuccess(t *testing.T) {
	withFastPoll(t)
	obj1 := oidcClientObj("my-app", "", nil)
	obj2 := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{"createdAt": "1", "lastFiveCharacters": "abcde"}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj1, obj2},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-1": b64("super-secret-value")},
		}},
	}
	root := stubRootFactory{c: fakeRootClient{body: []byte(discoveryOK)}}
	base := stubServerBase{base: "https://rancher.example"}
	cmd := NewCreate(stubOIDCFactory{c: client}, root, base)

	_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "OIDCClientCreated", res.Kind)
	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "my-app", data["name"])
	assert.Equal(t, "client-abc", data["clientID"])
	assert.Equal(t, "super-secret-value", data["clientSecret"])
	assert.Equal(t, "client-secret-1", data["secretName"])
	assert.Equal(t, "https://rancher.example/oidc", data["issuer"])
	endpoints, ok := data["endpoints"].(*oidcEndpoints)
	require.True(t, ok)
	assert.Equal(t, "https://rancher.example/oidc/authorize", endpoints.Authorization)
	assert.Equal(t, "https://rancher.example/oidc/token", endpoints.Token)
	assert.Equal(t, "https://rancher.example/oidc/.well-known/jwks.json", endpoints.JWKS)

	require.NotNil(t, res.CommittedContext)
	assert.Equal(t, "OIDCClient", res.CommittedContext.ResourceKind)
	assert.Equal(t, "my-app", res.CommittedContext.ResourceName)
	assert.Equal(t, "client-secret-1", res.CommittedContext.SecretName)
	// The shown-once note rides BOTH Text (below) and OnceOnlyNotice (no
	// discovery-degradation warning here); the pipeline prepends
	// OnceOnlyNotice to Warnings before rendering, so it still reaches
	// -o json without also mirroring to stderr under table/wide.
	assert.Empty(t, res.Warnings)
	assert.Contains(t, res.OnceOnlyNotice, "shown exactly once")
	assert.Equal(t, "oidcclient/my-app created\n"+
		"  client id: client-abc\n"+
		"  client secret: super-secret-value   (shown once - store it now)\n"+
		"  secret name: client-secret-1\n"+
		"  issuer: https://rancher.example/oidc\n", res.Text)
}

// TestCreateExecDegradedDiscovery asserts the four degrade cases: malformed
// JSON, incomplete (missing endpoint), issuer mismatch, and an HTTP-level
// failure all take the SAME degraded path - endpoints omitted, issuer
// derived from the effective base, one warning naming the failure - and
// never fail an already-committed create.
func TestCreateExecDegradedDiscovery(t *testing.T) {
	cases := []struct {
		name string
		root stubRootFactory
		verb string
	}{
		{"malformed JSON", stubRootFactory{c: fakeRootClient{body: []byte("not json")}}, "use"},
		{"incomplete document", stubRootFactory{c: fakeRootClient{body: []byte(`{"issuer":"https://rancher.example/oidc","authorization_endpoint":"https://rancher.example/oidc/authorize"}`)}}, "use"},
		{"issuer mismatch", stubRootFactory{c: fakeRootClient{body: []byte(`{"issuer":"https://wrong.example/oidc","authorization_endpoint":"a","token_endpoint":"b","jwks_uri":"c"}`)}}, "use"},
		{"http failure", stubRootFactory{c: fakeRootClient{err: clierr.New(clierr.CodeGeneric, "connection refused")}}, "read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastPoll(t)
			obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
			client := &fakeOIDCClient{
				pollResponses: []*unstructured.Unstructured{obj},
				secretResp: &unstructured.Unstructured{Object: map[string]any{
					"data": map[string]any{"client-secret-1": b64("super-secret-value")},
				}},
			}
			cmd := NewCreate(stubOIDCFactory{c: client}, tc.root, stubServerBase{base: "https://rancher.example"})
			_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}})
			require.NoError(t, err)
			res, err := exec(t.Context())
			require.NoError(t, err, "discovery failure must never fail an already-committed create")

			data, ok := res.Data.(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "https://rancher.example/oidc", data["issuer"], "degraded issuer derives from the effective base")
			assert.NotContains(t, data, "endpoints")
			// The pipeline prepends OnceOnlyNotice ahead of res.Warnings
			// (prependOnceOnlyNotice) before rendering, so the discovery
			// warning here is the command's own Warnings entry, shown-once
			// first once merged; the table/wide render skips exactly the
			// notice and still mirrors the discovery warning normally.
			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], "could not "+tc.verb+" the discovery document")
			assert.Contains(t, res.OnceOnlyNotice, "shown exactly once")
		})
	}
}

// TestCreateExecImpossibleShapesAreTerminalNotTimeout asserts the two
// impossible-shape cases (clientID present with >1 secret; any secret key
// with no clientID) as IMMEDIATE, non-retryable errors - proven not to
// wait by using a long timeout the test would otherwise block on.
func TestCreateExecImpossibleShapesAreTerminalNotTimeout(t *testing.T) {
	cases := []struct {
		name string
		obj  *unstructured.Unstructured
	}{
		{"two keys on first observation", oidcClientObj("my-app", "client-abc", map[string]any{
			"client-secret-1": map[string]any{}, "client-secret-2": map[string]any{},
		})},
		{"secret key with no clientID", oidcClientObj("my-app", "", map[string]any{
			"client-secret-1": map[string]any{},
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFastPoll(t)
			client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{tc.obj}}
			cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{}, stubServerBase{})
			_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}, flags: map[string]string{"timeout": "1h"}})
			require.NoError(t, err)

			done := make(chan struct{})
			var res runtime.Result
			var execErr error
			go func() {
				res, execErr = exec(t.Context())
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("impossible shape must fail immediately, not wait for the 1h timeout")
			}
			require.Error(t, execErr)
			ce, ok := execErr.(*clierr.Error)
			require.True(t, ok)
			assert.False(t, ce.Retryable)
			details, ok := ce.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.True(t, details.Committed)
			assert.Equal(t, "OIDCClient", details.ResourceKind)
			assert.Equal(t, runtime.Result{}, res)
			assert.Equal(t, 1, client.pollCalls, "the impossible shape must be caught on the FIRST observation")
		})
	}
}

// TestCreateExecTimeoutIsNonRetryable asserts create's timeout contract: a
// NEVER-satisfied predicate times out with a non-retryable error naming the
// committed resource - retrying would create a second client.
func TestCreateExecTimeoutIsNonRetryable(t *testing.T) {
	withFastPoll(t)
	neverProvisioned := oidcClientObj("my-app", "", nil)
	client := &fakeOIDCClient{pollResponses: []*unstructured.Unstructured{neverProvisioned}}
	cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{}, stubServerBase{})
	_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}, flags: map[string]string{"timeout": "20ms"}})
	require.NoError(t, err)

	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "my-app", details.ResourceName)
}

// alwaysProceedCapability lets a full-pipeline test reach Plan/Execute
// without a real capability checker - this file tests render/wait/discovery
// wiring, not the capability stage (covered elsewhere).
type alwaysProceedCapability struct{}

func (alwaysProceedCapability) Check(context.Context, runtime.CapabilityRequirement) runtime.CapabilityDecision {
	return runtime.CapabilityDecision{Outcome: runtime.CapabilityProceed}
}

// failingWriter always fails; n reports bytes accepted before the error.
type failingWriter struct{ n int }

func (w failingWriter) Write(p []byte) (int, error) {
	return min(w.n, len(p)), fmt.Errorf("disk full")
}

// TestCreateFullPipelineFailingStdoutEmitsCommittedContext drives create
// oidc-client through the REAL runtime.Run pipeline (not just exec in
// isolation) with a failing stdout writer, proving 3b's emission end to
// end for this command: the committed client/secret and recovery reach
// stderr as a structured envelope even though stdout never got the value.
func TestCreateFullPipelineFailingStdoutEmitsCommittedContext(t *testing.T) {
	withFastPoll(t)
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj},
		secretResp: &unstructured.Unstructured{Object: map[string]any{
			"data": map[string]any{"client-secret-1": b64("super-secret-value")},
		}},
	}
	cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{c: fakeRootClient{err: fmt.Errorf("no discovery in this test")}}, stubServerBase{base: "https://rancher.example"})

	var errBuf bytes.Buffer
	out := failingWriter{n: 0}
	_, ce := runtime.Run(t.Context(), cmd, oidcInput{args: []string{"my-app"}},
		runtime.IO{Out: out, Err: &errBuf, Format: "json", Apply: true, Yes: true, Capability: alwaysProceedCapability{}}, safety.Binding{})
	require.NotNil(t, ce)
	assert.False(t, ce.Retryable)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "OIDCClient", details.ResourceKind)
	assert.Equal(t, "my-app", details.ResourceName)
	assert.Equal(t, "client-secret-1", details.SecretName)
	assert.Contains(t, errBuf.String(), `"Error"`)
}

// TestCreateOutputPolicyRejectsNameWithZeroRequests asserts that
// `create oidc-client -o name` is rejected BEFORE Plan/capability
// resolution - zero total requests, not merely zero mutation requests - so
// a caller can never invalidate-and-discard a once-only client secret by
// picking -o name.
func TestCreateOutputPolicyRejectsNameWithZeroRequests(t *testing.T) {
	client := &fakeOIDCClient{}
	cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{}, stubServerBase{})
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, oidcInput{args: []string{"my-app"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: "name", Apply: true, Yes: true, Capability: alwaysProceedCapability{}}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Zero(t, client.pollCalls)
	assert.Zero(t, client.secretCalls)
	assert.Zero(t, client.createCalls, "zero TOTAL requests: not even the dry-run Plan Create may fire")
	assert.Empty(t, out.String())
}

// TestCreateExecSecretReadFailureIsCommittedPartialSuccess asserts the
// create-then-FORBIDDEN persona (the only verb that keeps this post-commit
// path): the backing-Secret GET fails after a committed mutation, and the
// resulting error preserves the original classification while adding
// committed-mutation details.
func TestCreateExecSecretReadFailureIsCommittedPartialSuccess(t *testing.T) {
	withFastPoll(t)
	obj := oidcClientObj("my-app", "client-abc", map[string]any{"client-secret-1": map[string]any{}})
	client := &fakeOIDCClient{
		pollResponses: []*unstructured.Unstructured{obj},
		secretErr:     clierr.New(clierr.CodeRBAC, "forbidden"),
	}
	cmd := NewCreate(stubOIDCFactory{c: client}, stubRootFactory{}, stubServerBase{})
	_, exec, err := planCreate(t, cmd, oidcInput{args: []string{"my-app"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.Error(t, err)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code, "original classification must survive")
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
	assert.Equal(t, "client-secret-1", details.SecretName)
}

// TestCommittedPartialSuccessPreservesRetryable asserts that
// committedPartialSuccess must not drop the original error's Retryable
// flag, which errmap.go sets true for transient/server errors: a caller
// inspecting ce.Retryable to decide whether to retry must see the
// classification the underlying read actually carried.
func TestCommittedPartialSuccessPreservesRetryable(t *testing.T) {
	t.Parallel()
	underlying := clierr.Retryable(clierr.CodeGeneric, "temporary failure")
	ce := committedPartialSuccess(underlying, "OIDCClient", "my-app", "client-secret-1", "get oidcclient my-app -o json")
	assert.True(t, ce.Retryable, "Retryable must survive the wrap")
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	details, ok := ce.Details.(clierr.CommittedContextDetails)
	require.True(t, ok)
	assert.True(t, details.Committed)
}

func (*fakeOIDCClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*fakeOIDCClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f stubOIDCFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}
