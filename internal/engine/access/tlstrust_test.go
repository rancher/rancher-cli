package access

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var accessSlotRemedy = tlstrust.Remedy{CA: "; CA-SLOT", URL: "URL-SLOT", Fix: "FIX-SLOT"}

func untrustedAccessServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	return srv
}

// certificateFailure is the error a request to the server returns when its
// certificate is not trusted: the production client builder with the
// classifying wrapper, against a TLS server the client has no CA for.
func certificateFailure(t *testing.T) error {
	t.Helper()
	srv := untrustedAccessServer(t)
	client, err := httpx.New(httpx.Options{Timeout: 5 * time.Second, WrapTransport: tlstrust.WrapTransportFor(srv.URL, accessSlotRemedy)})
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	_, err = client.Get(srv.URL)
	require.Error(t, err)
	require.NotNil(t, tlstrust.TypedError(err))
	return err
}

// failingRootAt answers every path as inner does, except path, which fails
// with err.
type failingRootAt struct {
	inner engine.RootClient
	path  string
	err   error
}

func (f failingRootAt) GetJSON(ctx context.Context, path string) ([]byte, error) {
	if path == f.path {
		return nil, f.err
	}
	return f.inner.GetJSON(ctx, path)
}

// TestProjectsNamespaceDiscoveryIssueCarriesTLSTrust asserts a cluster's
// namespace read that fails the certificate check is that cluster's
// discovery issue with the code TLS_TRUST, while the command carries on.
// Mutation: the wrapper stops classifying (the code is GENERIC).
func TestProjectsNamespaceDiscoveryIssueCarriesTLSTrust(t *testing.T) {
	cmd, _ := projectsFixture(t, projectsPages())
	inner := cmd.roots.(fakeRootFactory).rc
	cmd.roots = fakeRootFactory{failingRootAt{inner, steveNamespacesPath("c-1"), certificateFailure(t)}}

	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	require.NotNil(t, out.Clusters[0].NamespaceDiscovery.Issue)
	assert.Equal(t, clierr.CodeTLSTrust, out.Clusters[0].NamespaceDiscovery.Issue.Code)
	assert.False(t, out.Clusters[0].NamespaceDiscovery.Issue.Retryable)
}

// TestExplainChainIssueCarriesTLSTrust asserts a chain read that fails the
// certificate check is a chain issue with the code TLS_TRUST and the
// classified message. Mutation: the wrapper stops classifying.
func TestExplainChainIssueCarriesTLSTrust(t *testing.T) {
	t.Parallel()
	trust := certificateFailure(t)
	down := &fakeClient{GetErr: trust}
	w := newChainWalker(t.Context(), down, &fakeClient{}, "c-1", chainSubject{Kind: KindUser, Name: "u-1", ID: "u-1"}, explainQuestion())
	w.explainAllowed(`RBAC: allowed by RoleBinding "rb-1/team-a" of ClusterRole "role" to User "u-1"`)

	require.NotEmpty(t, w.issues)
	assert.Equal(t, clierr.CodeTLSTrust, w.issues[0].Code)
	assert.False(t, w.issues[0].Retryable)
	assert.Contains(t, w.issues[0].Message, "; CA-SLOT;")
}

// TestExplainProvenanceIssueCarriesTLSTrust asserts a GRANTED BY read that
// fails the certificate check is that stage's issue with the code
// TLS_TRUST. Mutation: the wrapper stops classifying.
func TestExplainProvenanceIssueCarriesTLSTrust(t *testing.T) {
	t.Parallel()
	fc := provenanceFixture()
	fc.ListErrFor = map[string]error{"clusterroletemplatebindings||": certificateFailure(t)}
	p, err := readProvenance(t.Context(), fc, provenanceInput{Subject: AccessSubject{Kind: KindUser, Name: "u-alice"}})
	require.NoError(t, err)
	require.Len(t, p.Discovery.Issues, 1)
	assert.Equal(t, stageCRTB, p.Discovery.Issues[0].Stage)
	assert.Equal(t, clierr.CodeTLSTrust, p.Discovery.Issues[0].Issue.Code)
}

// TestWhoCanIdentityReadIssueCarriesTLSTrust asserts an identity read of a
// who-can candidate that fails the certificate check degrades that
// candidate with the code TLS_TRUST. Mutation: the wrapper stops
// classifying.
func TestWhoCanIdentityReadIssueCarriesTLSTrust(t *testing.T) {
	t.Parallel()
	res := acquisitionIssue(t.Context(), certificateFailure(t))
	assert.Equal(t, outcomeDegradable, res.kind)
	require.NotNil(t, res.err)
	ce, ok := errors.AsType[*clierr.Error](res.err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
}

// TestCanEndsWithTLSTrustOnTheCallerIdentityReview asserts that when the
// production cluster client's review post fails the certificate check,
// access can ends at the caller's identity review with the TLS_TRUST error,
// exit 10 and not retryable, not the retryable GENERIC error the review post
// returns for a fan-out to degrade. Mutations: drop the transport error as
// the review post's cause; return the review error unchanged.
func TestCanEndsWithTLSTrustOnTheCallerIdentityReview(t *testing.T) {
	srv := untrustedAccessServer(t)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret", TLSRemedy: accessSlotRemedy}, "c-phmj7")
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, "c-phmj7")
	require.NoError(t, err)

	f := &fakeFactory{caller: &fakeClient{}, resolve: map[string]string{"prod": "c-phmj7"},
		clusterClients: map[string]engine.ClusterClient{"c-phmj7": client}}
	_, err = NewCan(f, f.caller).(runtime.ReadCommand).Execute(t.Context(), canInput(map[string]string{"namespace": "team-a"}, "get", "pods"), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.NotContains(t, ce.Message, "no response from the cluster")
	assert.Contains(t, ce.Message, "; CA-SLOT;")
}

// untrustedLocalClient is the production cluster client for the local
// cluster, pointed at a TLS server whose certificate the client does not
// trust.
func untrustedLocalClient(t *testing.T) engine.ClusterClient {
	t.Helper()
	srv := untrustedAccessServer(t)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret", TLSRemedy: accessSlotRemedy}, engine.LocalCluster)
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)
	return client
}

// TestClustersAndProjectsEndWithTLSTrustOnTheCallerIdentityReview asserts
// that access clusters and access projects end at the caller's identity
// review with the TLS_TRUST error, exit 10 and not retryable, when the
// Rancher server's certificate fails. Mutation: return the review error
// unchanged at either command's identity review.
func TestClustersAndProjectsEndWithTLSTrustOnTheCallerIdentityReview(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(engine.ClusterClient) error
	}{
		{"clusters", func(c engine.ClusterClient) error {
			_, err := NewClusters(nil, nil, c).(runtime.ReadCommand).Execute(t.Context(), clustersInput("", ""), nil)
			return err
		}},
		{"projects", func(c engine.ClusterClient) error {
			_, err := NewProjects(nil, nil, c).(runtime.ReadCommand).Execute(t.Context(), projectsInput("", "", ""), nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce, ok := errors.AsType[*clierr.Error](tc.run(untrustedLocalClient(t)))
			require.True(t, ok)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Equal(t, 10, ce.ExitCode())
			assert.False(t, ce.Retryable)
			assert.Contains(t, ce.Message, "; CA-SLOT;")
		})
	}
}
