package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
)

// stubFactory is a minimal engine.ClusterClientFactory for kubeconfig tests:
// only ResolveCluster/ContextCluster are exercised (kubeconfig never opens a
// ClusterClient).
type stubFactory struct {
	resolved   map[string]string
	resolveErr error
	ctxCluster string
	ctxErr     error
}

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return nil, nil }
func (f stubFactory) ContextCluster() (string, error)                               { return f.ctxCluster, f.ctxErr }
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

// writeTestConfig writes a config.yaml with server "prod" -> https://r.example.com
// (optionally with a CA) and current context cluster "c-ctx".
func writeTestConfig(t *testing.T, ca string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "prod",
		Contexts:       map[string]config.Context{"prod": {Server: "prod", Cluster: "c-ctx"}},
		Servers:        map[string]config.Server{"prod": {URL: "https://r.example.com", CA: ca}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

func TestKubeconfigEmitsExecStanza(t *testing.T) {
	t.Parallel()
	caPEM := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, caPEM)
	cmd := NewKubeconfig(dir, stubFactory{resolved: map[string]string{"webprod": "c-abc"}})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "webprod"}}, nil)
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
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	user := cfg.AuthInfos["prod-c-ctx"]
	require.NotNil(t, user)
	require.NotNil(t, user.Exec)
	assert.NotContains(t, user.Exec.Args, "--cacert")
}

func TestKubeconfigFallsBackToContextCluster(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	require.NotNil(t, cfg.Clusters["prod-c-ctx"])

	cmd = NewKubeconfig(dir, stubFactory{})
	_, err = cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestKubeconfigWarnsOnHTTPServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "prod",
		Contexts:       map[string]config.Context{"prod": {Server: "prod", Cluster: "c-ctx"}},
		Servers:        map[string]config.Server{"prod": {URL: "http://r.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, p)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not https",
		"the non-https server notice must reach the prompter's stderr writer")

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	assert.NotEmpty(t, kc.CurrentContext, "stdout must still be exactly the bare kubeconfig YAML")
}

// A malformed server URL makes kube.BaseURL's url.Parse fail, falling back to
// raw string trimming that preserves the original scheme case verbatim — the
// warning must still fire (case-insensitive, not-https rather than a literal
// "http://" match) instead of silently skipping it (CW-7 review finding 10).
func TestKubeconfigWarnsOnMalformedUppercaseHTTPServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "prod",
		Contexts:       map[string]config.Context{"prod": {Server: "prod", Cluster: "c-ctx"}},
		Servers:        map[string]config.Server{"prod": {URL: "HTTP://foo%zz.example.com/v3"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	_, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, p)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not https")
}

func TestKubeconfigNoWarningOnHTTPSServer(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	_, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, p)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestKubeconfigOmitsCADataWhenUnconfigured(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewKubeconfig(dir, stubFactory{ctxCluster: "c-ctx"})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := cfg.Clusters["prod-c-ctx"]
	require.NotNil(t, cluster)
	assert.Empty(t, cluster.CertificateAuthorityData)
	assert.False(t, cluster.InsecureSkipTLSVerify)
}
