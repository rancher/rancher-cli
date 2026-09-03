package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provisioningClusterServer serves the provisioning.cattle.io/v1 Cluster list
// from the fleet-default namespace on the local management cluster, counting
// hits. A second path accepts an explicit namespace override so the
// --namespace flag test can verify a different namespace is used.
func provisioningClusterServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	handler := func(ns string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch ns {
			case "fleet-default":
				_, _ = w.Write([]byte(`{"items":[
					{"metadata":{"name":"my-rke2","namespace":"fleet-default"}},
					{"metadata":{"name":"my-k3s","namespace":"fleet-default"}}
				]}`))
			default:
				_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"ns-cluster","namespace":%q}}]}`, ns)
			}
		}
	}
	mux.HandleFunc("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters", handler("fleet-default"))
	mux.HandleFunc("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-west/clusters", handler("fleet-west"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestLivePositionalDeleteProvisioningClusterCandidates asserts the management
// branch case for provisioning-cluster: `delete provisioning-cluster <TAB>`
// lists provisioning.cattle.io/v1 Clusters from the fleet-default namespace
// on the local management cluster.
func TestLivePositionalDeleteProvisioningClusterCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "delete provisioning-cluster"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Contains(t, got, "my-rke2", "provisioning-cluster name must be a candidate")
	assert.Contains(t, got, "my-k3s", "provisioning-cluster name must be a candidate")
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}

// TestLivePositionalGetProvisioningClusterCandidates confirms that
// `get provisioning-cluster <TAB>` uses the same fetch path as delete.
func TestLivePositionalGetProvisioningClusterCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get provisioning-cluster"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Contains(t, got, "my-rke2")
	assert.Contains(t, got, "my-k3s")
	assert.Equal(t, int32(1), hits.Load())
}

// TestLivePositionalProvisioningClusterNamespaceFlagOverride confirms that a
// --namespace flag in the arg tail overrides the fleet-default fallback.
func TestLivePositionalProvisioningClusterNamespaceFlagOverride(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get provisioning-cluster"}
	tail := []string{"--namespace", "fleet-west"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)

	assert.Contains(t, got, "ns-cluster", "explicit --namespace must override fleet-default")
	assert.Equal(t, int32(1), hits.Load())
}

// TestLivePositionalProvisioningClusterNamespacePathInjection confirms that a
// --namespace value containing a "/" is rejected before reaching fetchK8sResources,
// returning nil with zero requests (isValidSegment guard).
func TestLivePositionalProvisioningClusterNamespacePathInjection(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get provisioning-cluster"}
	tail := []string{"--namespace", "a/b"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)

	assert.Nil(t, got, "slash in --namespace must be rejected")
	assert.Zero(t, hits.Load(), "path injection attempt must issue zero requests")
}

// TestLivePositionalProvisioningClusterIDOnlyBoundary asserts the token boundary:
// a missing token is caught by fetchK8sResources' cap.Token=="" guard (zero
// requests); an exec-cred-only (IDOnly) capability is caught earlier by the
// IDOnly branch before any fetch is attempted.  Both must return nil with ZERO
// requests.
func TestLivePositionalProvisioningClusterIDOnlyBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "delete provisioning-cluster"}
	for _, cap := range []TokenCapability{
		{ServerURL: srv.URL}, // empty token: caught by cap.Token=="" in fetchK8sResources
		{Token: "downstream", IDOnly: true, ServerURL: srv.URL}, // exec-cred-only: caught by IDOnly branch
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		cancel()
		assert.Nil(t, got)
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
}

// TestLivePositionalCapiClusterNotCompletable confirms that the non-completable
// sibling capi-cluster yields zero candidates and zero requests.
func TestLivePositionalCapiClusterNotCompletable(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get capi-cluster"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)

	assert.Nil(t, got, "capi-cluster is not Completable: must return nil")
	assert.Zero(t, hits.Load(), "non-completable kind must issue zero requests")
}

// TestProvisioningClusterPositionalSiteFullCallback drives the real adaptEntry
// callback through BuildRoot/root.Run under the v2 protocol, mirroring
// TestNodeDriverPositionalSiteFullCallback: network on → live provisioning-cluster
// names; network off → empty.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestProvisioningClusterPositionalSiteFullCallback(t *testing.T) {
	srv, _ := provisioningClusterServer(t)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfig(cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	}

	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawProtoV2 = origProto
			rawV1Completion = origV1
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("delete provisioning-cluster positional → cluster names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"delete", "provisioning-cluster"}, "")
		assert.Contains(t, got, "my-rke2")
		assert.Contains(t, got, "my-k3s")
	})

	t.Run("get provisioning-cluster positional → cluster names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"get", "provisioning-cluster"}, "")
		assert.Contains(t, got, "my-rke2")
		assert.Contains(t, got, "my-k3s")
	})

	t.Run("network gate off → empty", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"delete", "provisioning-cluster"}, "")
		assert.Empty(t, got)
	})
}
