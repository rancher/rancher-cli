package resources

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// blockingClient is a ClusterClient whose Get answers the scripted errors
// first (one per call) and then blocks until the context ends, returning
// the error the provider client builds for a cut request: a retryable
// GENERIC naming the deadline.
type blockingClient struct {
	seqClient
	scripted []error
}

func (b *blockingClient) Get(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	i := b.calls
	b.calls++
	if i < len(b.scripted) {
		return nil, b.scripted[i]
	}
	<-ctx.Done()
	return nil, clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow")
}

// TestWaitForEndsAtOnceOnAuthAndTLSFailures asserts a 401 (AUTH) or a
// certificate failure (TLS_TRUST) ends the wait at the first Get, returned
// as is with its code and without the retryable flag, instead of retrying
// until the timeout.
func TestWaitForEndsAtOnceOnAuthAndTLSFailures(t *testing.T) {
	withFastPoll(t)
	tests := []struct {
		name string
		err  *clierr.Error
	}{
		{"auth", clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithHint("rancher auth status")},
		{"tls trust", clierr.New(clierr.CodeTLSTrust, "the server certificate is not trusted")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{tt.err}}
				cond := waitCond{kind: "state", name: "active"}
				start := time.Now()
				_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, time.Minute, false)
				elapsed := time.Since(start)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Same(t, tt.err, ce)
				assert.Equal(t, tt.err.Code, ce.Code)
				assert.False(t, ce.Retryable)
				assert.Equal(t, 1, client.calls)
				assert.Less(t, elapsed, time.Minute, "the wait must end at once, not use up the timeout")
			})
		})
	}
}

// TestWaitForRetriesOtherGetErrorsAndNamesTheLastOne asserts a 403, a 500,
// a slow server and an unclassified error are retried until the timeout, and
// that the timeout message names the last one after the last state and before
// the advice: "fix that error" for an error that is not retryable, the retry
// advice for one that is.
func TestWaitForRetriesOtherGetErrorsAndNamesTheLastOne(t *testing.T) {
	withFastPoll(t)
	const retryAdvice = "retry with a longer --timeout or inspect the resource to see why the wait condition is not met"
	const fixAdvice = "fix that error, or retry with a longer --timeout"
	tests := []struct {
		name   string
		err    error
		want   string
		advice string
	}{
		{"rbac", clierr.New(clierr.CodeRBAC, "forbidden (403)"), "RBAC: forbidden (403)", fixAdvice},
		{"server error", clierr.Retryable(clierr.CodeGeneric, "server error (500)"), "GENERIC: server error (500)", retryAdvice},
		{"slow server", clierr.Retryable(clierr.CodeGeneric, "context deadline exceeded - the server may be slow"),
			"GENERIC: context deadline exceeded - the server may be slow", retryAdvice},
		{"raw", errors.New("connection refused"), "GENERIC: connection refused", fixAdvice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{tt.err}}
				cond := waitCond{kind: "state", name: "active"}
				_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, 20*time.Millisecond, false)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeGeneric, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Equal(t, "timed out waiting for x; last state: <no state observed>; last error: "+tt.want+
					"; "+tt.advice, ce.Message)
				assert.Greater(t, client.calls, 1)
			})
		})
	}
}

// TestWaitForTimeoutAfterAnErrorThenAnObjectNamesNoError asserts a later Get
// that returns an object clears the last error: the timeout message names
// the state and no error.
func TestWaitForTimeoutAfterAnErrorThenAnObjectNamesNoError(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		client := &seqClient{
			objs: []*unstructured.Unstructured{nil, stateObj("provisioning", "yes")},
			errs: []error{clierr.New(clierr.CodeRBAC, "forbidden (403)"), nil},
		}
		cond := waitCond{kind: "state", name: "active"}
		_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, 20*time.Millisecond, false)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, "timed out waiting for x; last state: provisioning; "+
			"retry with a longer --timeout or inspect the resource to see why the wait condition is not met", ce.Message)
	})
}

// TestWaitForSucceedsAfterAForbiddenGet asserts a 403 does not end the
// wait: a later Get that satisfies the condition succeeds.
func TestWaitForSucceedsAfterAForbiddenGet(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		client := &seqClient{
			objs: []*unstructured.Unstructured{nil, stateObj("active", "no")},
			errs: []error{clierr.New(clierr.CodeRBAC, "forbidden (403)"), nil},
		}
		cond := waitCond{kind: "state", name: "active"}
		got, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, time.Second, false)
		require.NoError(t, err)
		assert.Equal(t, "active", mustState(got))
		assert.Equal(t, 2, client.calls)
	})
}

// TestWaitForTimeoutDuringAGetNamesNoCutError asserts a Get that the wait's
// own deadline cuts is not recorded as the last error, while an error from
// an earlier Get that ended with the context live stays named.
func TestWaitForTimeoutDuringAGetNamesNoCutError(t *testing.T) {
	withFastPoll(t)
	tests := []struct {
		name     string
		scripted []error
		want     string
	}{
		{"no earlier error", nil,
			"timed out waiting for x; last state: <no state observed>; " +
				"retry with a longer --timeout or inspect the resource to see why the wait condition is not met"},
		{"earlier forbidden", []error{clierr.New(clierr.CodeRBAC, "forbidden (403)")},
			"timed out waiting for x; last state: <no state observed>; last error: RBAC: forbidden (403); " +
				"fix that error, or retry with a longer --timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &blockingClient{scripted: tt.scripted}
				cond := waitCond{kind: "state", name: "active"}
				_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, 50*time.Millisecond, false)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, tt.want, ce.Message)
			})
		})
	}
}

// answeredThenCutClient is a ClusterClient whose Get blocks until the
// context ends and then returns what the provider client returns for a
// call a deadline ended after the server answered 429: a retryable GENERIC
// error whose cause holds the status.
type answeredThenCutClient struct{ seqClient }

func (answeredThenCutClient) Get(ctx context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	<-ctx.Done()
	return nil, clierr.Retryable(clierr.CodeGeneric, "https://rancher.example.com answered HTTP 429 Too Many Requests and the request ran out of time (30s) - retry later").
		WithCause(clierr.ResponseError(http.MethodGet, http.StatusTooManyRequests, nil))
}

// TestWaitForTimeoutDuringAGetThatWasAnsweredNamesTheAnswer asserts a Get
// that the wait's own deadline cut is recorded as the last error when it
// names a status the server answered, and not when it names none.
func TestWaitForTimeoutDuringAGetThatWasAnsweredNamesTheAnswer(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		cond := waitCond{kind: "state", name: "active"}
		_, err := waitFor(t.Context(), &answeredThenCutClient{}, schema.GroupVersionResource{}, "", "x", cond, 50*time.Millisecond, false)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, "timed out waiting for x; last state: <no state observed>; last error: GENERIC: "+
			"https://rancher.example.com answered HTTP 429 Too Many Requests and the request ran out of time (30s) - retry later; "+
			"retry with a longer --timeout or inspect the resource to see why the wait condition is not met", ce.Message)
	})
}

// TestWaitForNotFoundStillEndsAtOnce asserts NOT_FOUND keeps its own
// handling: the wait ends at the first Get with that error.
func TestWaitForNotFoundStillEndsAtOnce(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		client := &seqClient{objs: []*unstructured.Unstructured{nil}, errs: []error{clierr.New(clierr.CodeNotFound, "x not found")}}
		cond := waitCond{kind: "state", name: "active"}
		_, err := waitFor(t.Context(), client, schema.GroupVersionResource{}, "", "x", cond, time.Minute, false)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.Equal(t, 1, client.calls)
	})
}

// untrustedRealClient is the production cluster client against a TLS server
// whose certificate the saved CA does not match, so every request fails the
// certificate check. It also returns the server's certificate.
func untrustedRealClient(t *testing.T) (engine.ClusterClient, *x509.Certificate) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret"}, engine.LocalCluster)
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)
	return client, srv.Certificate()
}

// TestWaitForEndsAtOnceOnACertificateFailureFromTheRealClient asserts the
// error the production cluster client returns for a server certificate the
// saved CA does not match is one the wait ends on at once, with the code
// TLS_TRUST.
func TestWaitForEndsAtOnceOnACertificateFailureFromTheRealClient(t *testing.T) {
	client, _ := untrustedRealClient(t)
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}
	cond := waitCond{kind: "condition", name: "Ready", status: "True"}
	start := time.Now()
	_, err := waitFor(t.Context(), client, gvr, "", "c-abc", cond, time.Minute, false)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Less(t, time.Since(start), 30*time.Second, "the wait must end at once, not use up the timeout")
}

// TestCommittedPartialSuccessKeepsTheCertificateFacts asserts that a
// certificate failure from the production cluster client, wrapped by
// CommittedPartialSuccess, keeps its message's pointer to the certificate
// meaningful: the message names the certificate's subject, issuer and
// SHA-256 fingerprint, and the error's details are the committed details.
func TestCommittedPartialSuccessKeepsTheCertificateFacts(t *testing.T) {
	client, cert := untrustedRealClient(t)
	gvr := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}
	_, err := client.Get(t.Context(), gvr, "", "c-abc")
	orig, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	require.Equal(t, clierr.CodeTLSTrust, orig.Code)
	facts, ok := orig.Details.(tlstrust.Details)
	require.True(t, ok)
	require.NotEmpty(t, facts.Subject)
	require.NotEmpty(t, facts.Issuer)
	require.Equal(t, tlstrust.Fingerprint(cert), facts.Fingerprint)

	committed := clierr.CommittedContextDetails{Committed: true, ResourceKind: "OIDCClient", ResourceName: "my-app", Recovery: "check it"}
	ce := engine.CommittedPartialSuccess(err, committed)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, committed, ce.Details)
	assert.Contains(t, ce.Message, orig.Message)
	assert.Contains(t, ce.Message, facts.Subject)
	assert.Contains(t, ce.Message, facts.Issuer)
	assert.Contains(t, ce.Message, tlstrust.Fingerprint(cert))
	assert.NotContains(t, ce.Message, "valid: ")
}
