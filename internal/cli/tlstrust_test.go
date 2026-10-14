package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// otherCAPEM is a certificate the test server's own does not match: every
// httptest TLS server presents the same built-in certificate.
func otherCAPEM(t *testing.T) string {
	t.Helper()
	_, pem, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	return pem
}

func untrustedRancher(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// trustMessage is the unknown-authority message of the test server's
// certificate with ca as the remedy clause, rendered the way a command that
// expands no remedy marker prints it.
func trustMessage(t *testing.T, ce *clierr.Error, ca string) string {
	t.Helper()
	d, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	return clierr.ExpandRemedies(tlstrust.UnknownAuthorityMessage(d.IsLeafCertificate(), ca), "")
}

// TestRootClientClassifiesACertificateFailure asserts the root client of
// every ordinary command returns TLS_TRUST (exit 10, not retryable) for an
// untrusted server certificate, worded for where the connection came from:
// a saved entry, RANCHER_CLI_URL alone, with and without
// RANCHER_CLI_CACERT. Mutations: leave the root client unwrapped; restore
// the GENERIC flattening in rootClient.GetJSON; fill the wrong
// TLSRemedy row.
func TestRootClientClassifiesACertificateFailure(t *testing.T) {
	wrongCA := otherCAPEM(t)
	for _, tc := range []struct {
		name      string
		entry     bool
		variable  bool
		remedy    string
		wantCause string
	}{
		{name: "a named entry", entry: true,
			remedy: "; save it for this entry with: rancher config update-server 'acme' --cacert <file>"},
		{name: "a named entry and the variable", entry: true, variable: true,
			remedy: "; the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; to save a CA for this entry, run: rancher config update-server 'acme' --cacert <file>"},
		{name: "RANCHER_CLI_URL alone", remedy: "; set RANCHER_CLI_CACERT to it"},
		{name: "RANCHER_CLI_URL alone and the variable", variable: true,
			remedy: "; the CA came from RANCHER_CLI_CACERT - set it to the server's CA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := untrustedRancher(t)
			dir := t.TempDir()
			t.Setenv(config.EnvConfigDir, dir)
			t.Setenv(config.EnvURL, "")
			t.Setenv(config.EnvToken, "")
			t.Setenv(config.EnvCACert, "")
			if tc.variable {
				t.Setenv(config.EnvCACert, wrongCA)
			}
			if tc.entry {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
					"credentialStore: file\ncurrentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n"), 0o600))
				require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme", URL: srv.URL}, "tok:secret"))
			} else {
				t.Setenv(config.EnvURL, srv.URL)
				t.Setenv(config.EnvToken, "tok:secret")
			}
			rc, err := newClientFactory(dir, config.NewFileStore(dir)).Root()
			require.NoError(t, err)
			_, err = rc.GetJSON(t.Context(), "/v1/anything")

			ce := tlstrust.TypedError(err)
			require.NotNil(t, ce, "got %v", err)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Equal(t, 10, ce.ExitCode())
			assert.False(t, ce.Retryable)
			assert.Equal(t, trustMessage(t, ce, tc.remedy), clierr.ExpandRemedies(ce.Message, ""))
		})
	}
}

// TestClusterClientClassifiesACertificateFailure asserts the cluster client
// of a kind command, with a saved CA the server does not match, fails with
// TLS_TRUST through the real client builders, for a read and for API
// discovery. Mutation: leave kube.RESTConfig without its wrapper.
func TestClusterClientClassifiesACertificateFailure(t *testing.T) {
	srv := untrustedRancher(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(otherCAPEM(t)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"credentialStore: file\ncurrentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+caPath+"\n"), 0o600))
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme", URL: srv.URL}, "tok:secret"))
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCACert, "")

	f := newClientFactory(dir, config.NewFileStore(dir))
	cc, err := f.Cluster(t.Context(), "c-1")
	require.NoError(t, err)
	remedy := "; save it for this entry with: rancher config update-server 'acme' --cacert <file>"
	_, err = cc.ServerResources(t.Context())
	requireTrustError(t, err, remedy)

	pods := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	_, err = cc.Get(t.Context(), pods, "ns", "p")
	requireTrustError(t, err, remedy)

	kubeconfigs := schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "kubeconfigs"}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "ext.cattle.io/v1", "kind": "Kubeconfig",
		"spec": map[string]any{"clusters": []any{"c-1"}}}}
	_, err = cc.Create(t.Context(), kubeconfigs, "", obj, false)
	requireTrustError(t, err, remedy)
}

func requireTrustError(t *testing.T, err error, remedy string) {
	t.Helper()
	ce := tlstrust.TypedError(err)
	require.NotNil(t, ce, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Contains(t, clierr.ExpandRemedies(ce.Message, ""), remedy)
}

// TestKindCommandFailsWithTLSTrustThroughTheRealCLI asserts a kind command
// run through the whole CLI prints the TLS_TRUST envelope and exits 10.
// Mutation: leave kube.RESTConfig without its wrapper.
func TestKindCommandFailsWithTLSTrustThroughTheRealCLI(t *testing.T) {
	srv := untrustedRancher(t)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(otherCAPEM(t)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"credentialStore: file\ncurrentServer: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+caPath+"\n"), 0o600))
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme", URL: srv.URL}, "tok:secret"))
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCACert, "")

	exit, stdout, stderr := runRootJSON(t, "get", "namespaces", "--cluster", "c-1")
	require.Equal(t, 10, exit, "stdout: %s\nstderr: %s", stdout, stderr)
	got := cmdtest.DecodeErrorEnvelope(t, []byte(strings.TrimSpace(stderr)))
	assert.Equal(t, clierr.CodeTLSTrust, got.Code)
	assert.False(t, got.Retryable)
	assert.Contains(t, got.Message, "obtain the certificate through a trusted channel")
	assert.Contains(t, got.Message, "; save it for this entry with: rancher config update-server 'acme' --cacert <file>")
}

// TestCertificateRemedyNamesTheConfigDirectoryUnderKubeconfig asserts the
// config update-server command in a certificate failure message names the
// invocation's own --config under a command whose directory comes from a
// flag, and carries no marker to the terminal. Mutation: build the clause
// with a plain "rancher" literal instead of clierr.Rancher.
func TestCertificateRemedyNamesTheConfigDirectoryUnderKubeconfig(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(config.EnvConfigDir, "")
	t.Setenv(config.EnvCredentialStore, "")
	srv := untrustedRancher(t)
	wd := t.TempDir()
	t.Chdir(wd)
	realWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll("acct", 0o700))
	caPath := filepath.Join(realWD, "acct", "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(otherCAPEM(t)), 0o600))
	writeRemedyConfig(t, "acct", &config.Config{CurrentServer: "acme",
		Servers: map[string]config.Server{"acme": {URL: srv.URL, CA: caPath}}})
	require.NoError(t, config.NewFileStore("acct").SetToken(config.ServerRef{Name: "acme", URL: srv.URL}, "tok:secret"))

	got := runRemedyCLI(t, "--config", "acct", "kubeconfig", "--cluster", "c-1")
	assert.Contains(t, got, "error (TLS_TRUST)")
	assert.NotContains(t, got, clierr.RemedyMarker)
	assert.Contains(t, got, "rancher --config '"+filepath.Join(realWD, "acct")+"' config update-server 'acme' --cacert <file>")
}

// TestCompletionIgnoresACertificateFailure asserts a live completion
// against a server whose certificate is not trusted offers no candidate and
// prints nothing, so the completion client stays out of the classified
// path. Mutation: wrap the completion client and let the typed error
// reach the completion output.
func TestCompletionIgnoresACertificateFailure(t *testing.T) {
	srv := untrustedRancher(t)
	completionFixture(t, srv.URL, true)
	assert.Empty(t, runCompletion(t, []string{"grant", "jane", "--global", "--role"}, ""))
}
