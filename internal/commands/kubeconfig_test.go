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
// only ResolveCluster is exercised (kubeconfig never opens a ClusterClient).
type stubFactory struct {
	resolved   map[string]string
	resolveErr error
}

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return nil, nil }
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
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{resolved: map[string]string{"webprod": "c-abc"}})
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
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, nil)
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
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--cluster is required")
}

// Round-2 repro: RANCHER_CLI_URL overriding a stored context's server used to
// keep pairing the NEW url with the STORED server's CA and entry name.
// Overriding only RANCHER_CLI_URL against a stored context must fall back to
// RANCHER_CLI_CACERT (or none) and a host-derived name — never a stored CA
// against a URL it was never issued for.
func TestKubeconfigEnvURLOverrideDropsStoredCAAndName(t *testing.T) {
	prodCA := "-----BEGIN CERTIFICATE-----\nprod\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, prodCA) // stored "prod" -> https://r.example.com, CA=prodCA
	t.Setenv("RANCHER_CLI_URL", "https://staging.example.com")

	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, nil)
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
// mismatch — the stored CA/name still apply.
func TestKubeconfigEnvURLMatchingStoredServerKeepsStoredCAAndName(t *testing.T) {
	prodCA := "-----BEGIN CERTIFICATE-----\nprod\n-----END CERTIFICATE-----\n"
	dir := writeTestConfig(t, prodCA) // stored "prod" -> https://r.example.com, CA=prodCA
	t.Setenv("RANCHER_CLI_URL", "https://r.example.com")

	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)

	kc, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := kc.Clusters["prod-c-ctx"]
	require.NotNil(t, cluster, "got %v", kc.Clusters)
	assert.Equal(t, []byte(prodCA), cluster.CertificateAuthorityData, "same URL is not a mismatch — the stored CA still applies")
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
	dir := t.TempDir() // no config.yaml at all — no stored context, no stored server

	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-env"}}, nil)
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
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, p)
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
		CurrentServer: "prod",
		Servers:       map[string]config.Server{"prod": {URL: "HTTP://foo%zz.example.com/v3"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, p)
	require.NoError(t, err)
	assert.Contains(t, errBuf.String(), "not https")
}

func TestKubeconfigNoWarningOnHTTPSServer(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, strings.NewReader(""), true, false)
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	_, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, p)
	require.NoError(t, err)
	assert.Empty(t, errBuf.String())
}

func TestKubeconfigOmitsCADataWhenUnconfigured(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t, "")
	cmd := NewKubeconfig(dir, config.NewFileStore(dir), stubFactory{})
	res, err := cmd.(runtime.ReadCommand).Execute(context.Background(), args{s: map[string]string{"cluster": "c-ctx"}}, nil)
	require.NoError(t, err)
	cfg, err := clientcmd.Load(res.RawBytes)
	require.NoError(t, err)
	cluster := cfg.Clusters["prod-c-ctx"]
	require.NotNil(t, cluster)
	assert.Empty(t, cluster.CertificateAuthorityData)
	assert.False(t, cluster.InsecureSkipTLSVerify)
}
