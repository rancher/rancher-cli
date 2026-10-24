package inspect

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// untrustedRealClient is the production cluster client against a TLS server
// whose certificate the saved CA does not match, so every request fails the
// certificate check.
func untrustedRealClient(t *testing.T) engine.ClusterClient {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(srv.Close)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: wrongCA, Token: "id:secret"}, engine.LocalCluster)
	require.NoError(t, err)
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)
	return client
}

// TestInspectClusterDegradedEntriesCarryTLSTrust asserts a section that
// degrades because the cluster client failed the certificate check reports
// the code TLS_TRUST in its degraded entry. Mutation: leave the cluster
// client unwrapped (the code is GENERIC).
func TestInspectClusterDegradedEntriesCarryTLSTrust(t *testing.T) {
	root := &fakeRoot{cluster: mgmtClusterFixture("c-abc", "prod", "active", ""), body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}
	cmd := NewInspectCluster(&fakeFactory{client: untrustedRealClient(t)}, fakeRoots{rc: root})
	res, err := execRead(t, cmd, registrationInput("c-abc"))
	require.NoError(t, err, "a degraded section never fails the command")

	doc, _ := marshaledDoc(t, res.Data)
	entries, ok := doc["degraded"].([]any)
	require.True(t, ok, "the document lists its degraded sections")
	require.NotEmpty(t, entries)
	for _, e := range entries {
		assert.Equal(t, clierr.CodeTLSTrust, e.(map[string]any)["code"])
	}
}

// TestImagesFleetScanFailureOfTheCertificateExitsTen asserts a fleet-wide
// scan whose every cluster failed the certificate check is the hard error
// with code TLS_TRUST (exit 10), not GENERIC (exit 1). Mutation: leave the
// cluster client unwrapped.
func TestImagesFleetScanFailureOfTheCertificateExitsTen(t *testing.T) {
	factory, roots := allClustersFactory(t, map[string]engine.ClusterClient{"c-a": untrustedRealClient(t)})

	_, err := execRead(t, NewImages(factory, roots), input{b: map[string]bool{"all-clusters": true}})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.Contains(t, ce.Message, "no cluster could be read: c-a: ")
}
