package kubeconfig

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// stubFactory is a minimal engine.ClusterClientFactory for kubeconfig tests:
// ResolveCluster answers from resolved, and Cluster hands out a stub local
// client whose management objects are objects (a cluster with the endpoint
// off is synthesized for any id not present, so the proxy-only tests need
// no fixtures).
type stubFactory struct {
	resolved   map[string]string
	resolveErr error
	objects    map[string]*unstructured.Unstructured
	lists      map[string]*unstructured.UnstructuredList
}

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return &stubClusterClient{objects: f.objects, lists: f.lists}, nil
}
func (f stubFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if id, ok := f.resolved[nameOrID]; ok {
		return id, nil
	}
	return nameOrID, nil
}

var _ engine.ClusterClientFactory = stubFactory{}

// stubClusterClient answers GET from objects ("name" or "ns/name") and LIST
// from lists ("resource|ns"); a management cluster absent from objects is
// synthesized with the endpoint off.
type stubClusterClient struct {
	objects map[string]*unstructured.Unstructured
	lists   map[string]*unstructured.UnstructuredList
}

func (c *stubClusterClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	key := name
	if ns != "" {
		key = ns + "/" + name
	}
	if obj, ok := c.objects[key]; ok {
		return obj, nil
	}
	if gvr.Resource == "clusters" {
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{}}}, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, key+" not found")
}

func (c *stubClusterClient) List(_ context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	if l, ok := c.lists[gvr.Resource+"|"+ns]; ok {
		return l, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (c *stubClusterClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("unreachable")
}
func (c *stubClusterClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("unreachable")
}
func (c *stubClusterClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return errors.New("unreachable")
}
func (c *stubClusterClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("unreachable")
}
func (c *stubClusterClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("unreachable")
}
func (c *stubClusterClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("unreachable")
}

// aceObjects builds the management objects of a cluster with the endpoint
// on and the feature on: an fqdn cluster when fqdn is set, else a cluster
// with status.caCert and the given management nodes in its namespace.
func aceObjects(id, fqdn string, caPEM []byte, nodes ...unstructured.Unstructured) (map[string]*unstructured.Unstructured, map[string]*unstructured.UnstructuredList) {
	endpoint := map[string]any{"enabled": true}
	cluster := map[string]any{"metadata": map[string]any{"name": id}, "spec": map[string]any{"localClusterAuthEndpoint": endpoint}}
	if fqdn != "" {
		endpoint["fqdn"] = fqdn
		endpoint["caCerts"] = string(caPEM)
	} else {
		cluster["status"] = map[string]any{"caCert": base64.StdEncoding.EncodeToString(caPEM)}
	}
	objects := map[string]*unstructured.Unstructured{
		id:                         {Object: cluster},
		"multi-cluster-management": {Object: map[string]any{"metadata": map[string]any{"name": "multi-cluster-management"}, "spec": map[string]any{"value": true}}},
	}
	lists := map[string]*unstructured.UnstructuredList{"nodes|" + id: {Items: nodes}}
	return objects, lists
}

// controlPlaneNode builds a management control-plane node with one
// InternalIP and the given Ready condition.
func controlPlaneNode(name, hostname, ip string, ready bool) unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"controlPlane": true, "requestedHostname": hostname},
		"status": map[string]any{"internalNodeStatus": map[string]any{
			"addresses":  []any{map[string]any{"type": "InternalIP", "address": ip}},
			"conditions": []any{map[string]any{"type": "Ready", "status": status}},
		}},
	}}
}

// testCAPEM is a self-signed certificate for the direct entries' CA fields.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ace-ca"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// loadKubeconfig decodes the command's output the way kubectl would.
func loadKubeconfig(t *testing.T, res runtime.Result) *clientcmdapi.Config {
	t.Helper()
	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	return kc
}

// writeTestConfig writes a config.yaml with server "prod" -> https://r.example.com
// (optionally with a CA) and current server set to "prod".
func writeTestConfig(t *testing.T, ca string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers:       map[string]config.Server{"prod": {URL: "https://r.example.com", CA: ca}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

func TestKubeconfigEmitsExecSection(t *testing.T) {
	t.Parallel()
	caPEM := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, caPEM)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{resolved: map[string]string{"webprod": "c-abc"}})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "webprod"}}, nil)
	require.NoError(t, err)
	require.NotNil(t, res.RawBytes)

	cfg, err := clientcmd.Load(res.RawBytes) // NEVER string-match the YAML
	require.NoError(t, err)
	cluster := cfg.Clusters["prod-c-abc"]
	require.NotNil(t, cluster)
	assert.Equal(t, "https://r.example.com/k8s/clusters/c-abc", cluster.Server)
	assert.NotEmpty(t, cluster.CertificateAuthorityData)
	user := cfg.AuthInfos["prod-c-abc"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec)
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", user.Exec.APIVersion)
	assert.Equal(t, "rancher", user.Exec.Command)
	assert.Equal(t, []string{
		"auth", "get-token", "--server", "https://r.example.com", "--cluster", "c-abc", "--name", "prod-c-abc",
		"--cacert", caPEM,
	}, user.Exec.Args)
	assert.Equal(t, "prod-c-abc", cfg.CurrentContext)
}

func TestKubeconfigExecArgsOmitCacertWhenNoCA(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	user := cfg.AuthInfos["prod-c-ctx"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec)
	assert.NotContains(t, user.Exec.Args, "--cacert")
}

func TestKubeconfigRequiresClusterFlag(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--cluster is required")
}

// Round-2 repro: RANCHER_CLI_URL overriding a stored context's server used to
// keep pairing the NEW url with the STORED server's CA and entry name.
// Overriding only RANCHER_CLI_URL against a stored context must fall back to
// RANCHER_CLI_CACERT (or none) and a host-derived name - never a stored CA
// against a URL it was never issued for.
func TestKubeconfigEnvURLOverrideDropsStoredCAAndName(t *testing.T) {
	prodCA := "-----BEGIN CERTIFICATE-----\nprod\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, prodCA) // stored "prod" -> https://r.example.com, CA=prodCA
	t.Setenv("RANCHER_CLI_URL", "https://staging.example.com")

	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := kc.Clusters["staging.example.com-c-ctx"]
	require.NotNil(t, cluster, "entry name must derive from the override host, not the stored server name, got %v", kc.Clusters)
	assert.Equal(t, "https://staging.example.com/k8s/clusters/c-ctx", cluster.Server)
	assert.Empty(t, cluster.CertificateAuthorityData, "must never pair the stored CA with a different server's URL")
	assert.Equal(t, "staging.example.com-c-ctx", kc.CurrentContext)
}

// The env override matching the stored server's own URL exactly is not a
// mismatch - the stored CA/name still apply.
func TestKubeconfigEnvURLMatchingStoredServerKeepsStoredCAAndName(t *testing.T) {
	prodCA := "-----BEGIN CERTIFICATE-----\nprod\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, prodCA) // stored "prod" -> https://r.example.com, CA=prodCA
	t.Setenv("RANCHER_CLI_URL", "https://r.example.com")

	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := kc.Clusters["prod-c-ctx"]
	require.NotNil(t, cluster, "got %v", kc.Clusters)
	assert.Equal(t, []byte(prodCA), cluster.CertificateAuthorityData, "same URL is not a mismatch - the stored CA still applies")
}

// DR-007 repro: kubeconfig used to call cfg.CurrentServer() directly, so an
// env-only session (RANCHER_CLI_URL/RANCHER_CLI_CACERT, no stored context) failed
// with CONTEXT_NOT_SET even though every other command works from env alone.
// It must now resolve via Effective like everything else, and synthesize an
// entry name from the server host since no stored server name exists.
func TestKubeconfigEnvOnlySession(t *testing.T) {
	caPEM := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	t.Setenv("RANCHER_CLI_URL", "https://env.example.com")
	t.Setenv("RANCHER_CLI_CACERT", caPEM)
	dir := t.TempDir() // no config.yaml at all - no stored context, no stored server

	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-env"}}, nil)
	require.NoError(t, err)

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := kc.Clusters["env.example.com-c-env"]
	require.NotNil(t, cluster, "entry name must synthesize from the server host, got %v", kc.Clusters)
	assert.Equal(t, "https://env.example.com/k8s/clusters/c-env", cluster.Server)
	assert.Equal(t, []byte(caPEM), cluster.CertificateAuthorityData, "RANCHER_CLI_CACERT must be baked in for an env-only session")
	assert.Equal(t, "env.example.com-c-env", kc.CurrentContext)
}

func TestKubeconfigWarnsOnHTTPServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers:       map[string]config.Server{"prod": {URL: "http://r.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, p)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not https",
		"the non-https server notice must reach the prompter's stderr writer")

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	assert.NotEmpty(t, kc.CurrentContext, "stdout must still be exactly the bare kubeconfig YAML")
}

// A malformed server URL makes kube.BaseURL's url.Parse fail, falling back to
// raw string trimming that preserves the original scheme case verbatim - the
// warning must still fire (case-insensitive, not-https rather than a literal
// "http://" match) instead of silently skipping it (CW-7 review finding 10).
func TestKubeconfigWarnsOnMalformedUppercaseHTTPServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers:       map[string]config.Server{"prod": {URL: "HTTP://foo%zz.example.com/v3"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, p)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not https")
}

func TestKubeconfigNoWarningOnHTTPSServer(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, p)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestKubeconfigOmitsCADataWhenUnconfigured(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := cfg.Clusters["prod-c-ctx"]
	require.NotNil(t, cluster)
	assert.Empty(t, cluster.CertificateAuthorityData)
	assert.False(t, cluster.InsecureSkipTLSVerify)
}

func (f stubFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// TestKubeconfigDirectFQDNContextIsCurrent asserts the fqdn direct context:
// one extra cluster and context named <entry>-fqdn with the endpoint's CA,
// current, sharing the one exec user entry, and provideClusterInfo set.
func TestKubeconfigDirectFQDNContextIsCurrent(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	ca := testCAPEM(t)
	objects, lists := aceObjects("c-abc", "ace.example.test", ca)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{resolved: map[string]string{"webprod": "c-abc"}, objects: objects, lists: lists})
	var notices bytes.Buffer
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "webprod"}}, ui.New(&notices, nil, true, false))
	require.NoError(t, err)
	kc := loadKubeconfig(t, res)
	require.Len(t, kc.AuthInfos, 1, "one exec user entry shared by every context")
	require.Contains(t, kc.Clusters, "prod-c-abc-fqdn")
	assert.Equal(t, "https://ace.example.test", kc.Clusters["prod-c-abc-fqdn"].Server)
	assert.Equal(t, ca, kc.Clusters["prod-c-abc-fqdn"].CertificateAuthorityData)
	assert.Equal(t, "prod-c-abc", kc.Contexts["prod-c-abc-fqdn"].AuthInfo)
	assert.Equal(t, "prod-c-abc-fqdn", kc.CurrentContext)
	assert.True(t, kc.AuthInfos["prod-c-abc"].Exec.ProvideClusterInfo)
	assert.Equal(t, "https://r.example.com/k8s/clusters/c-abc", kc.Clusters["prod-c-abc"].Server, "the proxy context is still present")
	assert.Empty(t, notices.String())
}

// TestKubeconfigDirectNodeContexts asserts one context per control-plane
// node, the first Ready node current, the decoded status CA on each, and
// the proxy staying current when no node is Ready.
func TestKubeconfigDirectNodeContexts(t *testing.T) {
	t.Parallel()
	ca := testCAPEM(t)
	t.Run("first ready node is current", func(t *testing.T) {
		t.Parallel()
		dir := writeTestConfig(t, "")
		objects, lists := aceObjects("c-abc", "", ca,
			controlPlaneNode("m-1", "c-abc-cp1", "10.0.0.1", false),
			controlPlaneNode("m-2", "c-abc-cp2", "10.0.0.2", true))
		cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{objects: objects, lists: lists})
		res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-abc"}}, ui.New(&bytes.Buffer{}, nil, true, false))
		require.NoError(t, err)
		kc := loadKubeconfig(t, res)
		require.Len(t, kc.Contexts, 3)
		assert.Equal(t, "https://10.0.0.1:6443", kc.Clusters["prod-c-abc-cp1"].Server)
		assert.Equal(t, ca, kc.Clusters["prod-c-abc-cp2"].CertificateAuthorityData)
		assert.Equal(t, "prod-c-abc-cp2", kc.CurrentContext)
		for name, c := range kc.Contexts {
			assert.Equal(t, "prod-c-abc", c.AuthInfo, name)
		}
		assert.True(t, kc.AuthInfos["prod-c-abc"].Exec.ProvideClusterInfo)
	})
	t.Run("no ready node keeps the proxy current", func(t *testing.T) {
		t.Parallel()
		dir := writeTestConfig(t, "")
		objects, lists := aceObjects("c-abc", "", ca, controlPlaneNode("m-1", "c-abc-cp1", "10.0.0.1", false))
		cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{objects: objects, lists: lists})
		res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-abc"}}, ui.New(&bytes.Buffer{}, nil, true, false))
		require.NoError(t, err)
		kc := loadKubeconfig(t, res)
		require.Len(t, kc.Contexts, 2)
		assert.Equal(t, "prod-c-abc", kc.CurrentContext)
		assert.True(t, kc.AuthInfos["prod-c-abc"].Exec.ProvideClusterInfo, "a direct context exists even though the proxy stays current")
	})
}

// TestKubeconfigDirectOmittedIsOneNotice asserts the proxy-only fallback:
// the endpoint enabled but no control-plane node yields today's file, no
// provideClusterInfo, and exactly one stderr notice with the reason, with a
// node name that carries a control character shown in its printable form.
func TestKubeconfigDirectOmittedIsOneNotice(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	ca := testCAPEM(t)
	worker := controlPlaneNode("m-w", "c-abc-w1", "10.0.0.9", true)
	worker.Object["spec"].(map[string]any)["controlPlane"] = false
	noAddress := controlPlaneNode("m-\x1b[31mred", "c-abc-cp2", "", true)
	noAddress.Object["status"].(map[string]any)["internalNodeStatus"].(map[string]any)["addresses"] = []any{}
	objects, lists := aceObjects("c-abc", "", ca, worker, noAddress)
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{objects: objects, lists: lists})
	var notices bytes.Buffer
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-abc"}}, ui.New(&notices, nil, true, false))
	require.NoError(t, err)
	kc := loadKubeconfig(t, res)
	require.Len(t, kc.Contexts, 1)
	assert.Equal(t, "prod-c-abc", kc.CurrentContext)
	assert.False(t, kc.AuthInfos["prod-c-abc"].Exec.ProvideClusterInfo)
	out := notices.String()
	assert.Equal(t, 1, strings.Count(out, "direct endpoint contexts omitted"), out)
	assert.Contains(t, out, "no address")
	assert.NotContains(t, out, "\x1b", "control characters never reach the terminal")
}

// TestKubeconfigDirectEndpointOffIsUnchanged asserts a cluster without the
// endpoint yields today's file: one context, no provideClusterInfo, no
// notice.
func TestKubeconfigDirectEndpointOffIsUnchanged(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewCommand(dir, config.NewFileStore(dir), stubFactory{})
	var notices bytes.Buffer
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cluster": "c-abc"}}, ui.New(&notices, nil, true, false))
	require.NoError(t, err)
	kc := loadKubeconfig(t, res)
	require.Len(t, kc.Contexts, 1)
	assert.False(t, kc.AuthInfos["prod-c-abc"].Exec.ProvideClusterInfo)
	assert.Empty(t, notices.String())
}
