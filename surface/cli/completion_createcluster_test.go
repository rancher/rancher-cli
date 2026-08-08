package cli

import (
	"bytes"
	"context"
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

// machineConfigServer serves the fleet-default amazonec2configs list,
// counting hits. One name carries shell metacharacters to exercise the
// AC-21 trust boundary (server-derived NAME segments are sanitized before
// the trusted "kind/" prefix is composed on).
func machineConfigServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/rke-machine-config.cattle.io/v1/namespaces/fleet-default/amazonec2configs",
		func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"cfg-a"}},
				{"metadata":{"name":"cfg-b"}},
				{"metadata":{"name":"evil;rm -rf"}}
			]}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// cloudCredServer serves the cattle-global-data secrets list, counting hits.
// The mix covers: a credential with a unique display name, two credentials
// sharing a display name (ambiguous), a display name colliding with another
// credential's id, a hostile display name, and a non-credential secret.
func cloudCredServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets",
		func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"cc-abc12","annotations":{"field.cattle.io/name":"aws-prod"}},"data":{"amazonec2credentialConfig-accessKey":"UkVEQUNURUQ="}},
				{"metadata":{"name":"cc-dup1","annotations":{"field.cattle.io/name":"dup"}}},
				{"metadata":{"name":"cc-dup2","annotations":{"field.cattle.io/name":"dup"}}},
				{"metadata":{"name":"cc-collide","annotations":{"field.cattle.io/name":"cc-abc12"}}},
				{"metadata":{"name":"cc-hostile","annotations":{"field.cattle.io/name":"bad name!"}}},
				{"metadata":{"name":"marker-only"},"data":{"azurecredentialConfig-clientId":"UkVEQUNURUQ="}},
				{"metadata":{"name":"other-secret"},"data":{"password":"UkVEQUNURUQ="}}
			]}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestMachineConfigPrefixCandidatesStatic pins the compiled-in `<kind>/`
// prefix layer: served for create cluster's --machine-config when the typed
// value has no "/" yet, in both space and equals form, and only there.
func TestMachineConfigPrefixCandidatesStatic(t *testing.T) {
	t.Parallel()
	create := runtime.Descriptor{Name: "create cluster"}

	t.Run("space-form empty partial → five kind/ prefixes", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config"}
		got := machineConfigPrefixCandidates(site, create)
		assert.Equal(t, []string{
			"amazonec2config/", "azureconfig/", "digitaloceanconfig/",
			"harvesterconfig/", "vmwarevsphereconfig/",
		}, got)
	})

	t.Run("equals-form → full --machine-config=kind/ tokens", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", EqualsForm: true, Partial: "ama"}
		got := machineConfigPrefixCandidates(site, create)
		assert.Equal(t, []string{
			"--machine-config=amazonec2config/", "--machine-config=azureconfig/",
			"--machine-config=digitaloceanconfig/", "--machine-config=harvesterconfig/",
			"--machine-config=vmwarevsphereconfig/",
		}, got)
	})

	t.Run("partial with a slash → nil (the live layer owns kind/partial)", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", Partial: "amazonec2config/cf"}
		assert.Nil(t, machineConfigPrefixCandidates(site, create))
	})

	t.Run("other flag or other command → nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, machineConfigPrefixCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential"}, create))
		assert.Nil(t, machineConfigPrefixCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config"},
			runtime.Descriptor{Name: "get pod"}))
	})
}

// TestMachineConfigPrefixesServedWithoutNetwork pins the static layer's
// position BEFORE the network gate: the prefixes are compiled-in kind-table
// literals (not server data), so they are served with completion.network
// off and zero HTTP requests — same doctrine as static enums.
func TestMachineConfigPrefixesServedWithoutNetwork(t *testing.T) {
	// NOT parallel: mutates storedConfig.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: false},
	}
	setStoredConfig(cfg, t.TempDir())

	d := runtime.Descriptor{
		Name:  "create cluster",
		Flags: []runtime.ArgSpec{{Name: "machine-config", Type: "string"}},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{"node-1", "--machine-config"})
	assert.Contains(t, got, "amazonec2config/")
	assert.Len(t, got, 5)
	assert.Zero(t, hits.Load(), "compiled-in prefixes must cost zero requests")
}

// TestLiveFlagValueMachineConfigNameComposition pins the live `<kind>/partial`
// site: the kind's CRs are listed in fleet-default, server NAME segments are
// sanitized with the AC-21 charset, and the trusted "kind/" prefix is
// composed on afterwards.
func TestLiveFlagValueMachineConfigNameComposition(t *testing.T) {
	t.Parallel()

	t.Run("space-form kind/ → composed kind/name tokens, hostile name dropped", func(t *testing.T) {
		t.Parallel()
		srv, hits := machineConfigServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", Partial: "amazonec2config/"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil)
		assert.Equal(t, []string{"amazonec2config/cfg-a", "amazonec2config/cfg-b"}, got)
		assert.Equal(t, int32(1), hits.Load())
	})

	t.Run("equals-form → full --machine-config=kind/name tokens", func(t *testing.T) {
		t.Parallel()
		srv, _ := machineConfigServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", EqualsForm: true, Partial: "amazonec2config/cf"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil)
		assert.Equal(t, []string{"--machine-config=amazonec2config/cfg-a", "--machine-config=amazonec2config/cfg-b"}, got)
	})

	t.Run("unknown kind token → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := machineConfigServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", Partial: "nonesuch/cf"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		assert.Nil(t, liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil))
		assert.Zero(t, hits.Load())
	})

	t.Run("kind-table kind outside the machine-config group → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := machineConfigServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", Partial: "pod/x"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		assert.Nil(t, liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil))
		assert.Zero(t, hits.Load())
	})

	t.Run("IDOnly exec-cred capability → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := machineConfigServer(t)
		cap := TokenCapability{Token: "downstream", IDOnly: true, ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", Partial: "amazonec2config/"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		assert.Nil(t, liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil))
		assert.Zero(t, hits.Load())
	})
}

// TestLiveFlagValueCloudCredentialCandidates pins the --cloud-credential
// site: ids (secret names) always; display names only when they resolve
// unambiguously (ResolveCloudCredential's doctrine — a suggestion must never
// fail with AMBIGUOUS_SELECTION at execution time); non-credential secrets
// excluded; hostile display names dropped by the charset.
func TestLiveFlagValueCloudCredentialCandidates(t *testing.T) {
	t.Parallel()

	t.Run("ids always, unique display names only, non-credentials excluded", func(t *testing.T) {
		t.Parallel()
		srv, hits := cloudCredServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential", FlagSpelling: "--cloud-credential"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil)
		assert.Equal(t, []string{"aws-prod", "cc-abc12", "cc-collide", "cc-dup1", "cc-dup2", "cc-hostile", "marker-only"}, got)
		assert.NotContains(t, got, "dup", "ambiguous display name must be suppressed")
		assert.NotContains(t, got, "other-secret", "non-credential secret must be excluded")
		assert.Equal(t, int32(1), hits.Load())
	})

	t.Run("equals-form → full --cloud-credential=<value> tokens", func(t *testing.T) {
		t.Parallel()
		srv, _ := cloudCredServer(t)
		cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential", FlagSpelling: "--cloud-credential", EqualsForm: true}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil)
		assert.Contains(t, got, "--cloud-credential=cc-abc12")
		assert.Contains(t, got, "--cloud-credential=aws-prod")
	})

	t.Run("no token / IDOnly → nil zero requests", func(t *testing.T) {
		t.Parallel()
		srv, hits := cloudCredServer(t)
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential"}
		for _, cap := range []TokenCapability{
			{ServerURL: srv.URL},
			{Token: "downstream", IDOnly: true, ServerURL: srv.URL},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			assert.Nil(t, liveFlagValueCandidates(ctx, srv.Client(), site, cap, "", "", nil))
			cancel()
		}
		assert.Zero(t, hits.Load())
	})
}

// TestLiveCandidatesNodeDriverFlagSitesBypassClusterPreflight mirrors the
// role/subject bypass regression: an inherited cluster value that fails
// isValidSegment ("prod cluster" has a space) must NOT suppress the
// management-server-only --machine-config/--cloud-credential sites.
func TestLiveCandidatesNodeDriverFlagSitesBypassClusterPreflight(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	t.Setenv(config.EnvToken, "mgmt-token")

	t.Run("--machine-config kind/ site", func(t *testing.T) {
		srv, hits := machineConfigServer(t)
		cfg := &config.Config{
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "prod cluster"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: true},
		}
		setStoredConfig(cfg, t.TempDir())

		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", Partial: "amazonec2config/"}
		d := runtime.Descriptor{Name: "create cluster"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, d, nil, []string{"node-1", "--machine-config"})
		assert.Equal(t, []string{"amazonec2config/cfg-a", "amazonec2config/cfg-b"}, got)
		assert.Equal(t, int32(1), hits.Load(), "preflight bypass: machine-config endpoint fires exactly once")
	})

	t.Run("--cloud-credential site", func(t *testing.T) {
		srv, hits := cloudCredServer(t)
		cfg := &config.Config{
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv", Cluster: "prod cluster"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: true},
		}
		setStoredConfig(cfg, t.TempDir())

		site := CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential", FlagSpelling: "--cloud-credential"}
		d := runtime.Descriptor{Name: "create cluster"}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, d, nil, []string{"node-1", "--cloud-credential"})
		assert.Contains(t, got, "cc-abc12")
		assert.Equal(t, int32(1), hits.Load(), "preflight bypass: secrets endpoint fires exactly once")
	})
}

// TestCreateClusterFlagSitesFullCallback drives the real adaptEntry callback
// through BuildRoot/root.Run under the v2 protocol, mirroring
// TestRoleValueSiteFullCallback. The network-gate-off sub-tests pin the
// split: live sites go empty, while the compiled-in kind prefixes survive.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestCreateClusterFlagSitesFullCallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/rke-machine-config.cattle.io/v1/namespaces/fleet-default/amazonec2configs",
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"cfg-a"}}]}`))
		})
	mux.HandleFunc("/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets",
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"cc-abc12","annotations":{"field.cattle.io/name":"aws-prod"}}}]}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: networkOn},
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
		_ = root.Run(context.Background(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("--machine-config bare word → kind/ prefixes", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"create", "cluster", "node-1", "--machine-config"}, "")
		assert.Contains(t, got, "amazonec2config/\n")
		assert.Contains(t, got, "vmwarevsphereconfig/\n")
	})

	t.Run("--machine-config kind/ → live kind/name tokens", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"create", "cluster", "node-1", "--machine-config"}, "amazonec2config/")
		assert.Contains(t, got, "amazonec2config/cfg-a")
	})

	t.Run("--cloud-credential → id and unique display name", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"create", "cluster", "node-1", "--cloud-credential"}, "")
		assert.Contains(t, got, "cc-abc12")
		assert.Contains(t, got, "aws-prod")
	})

	t.Run("network gate off → live sites empty", func(t *testing.T) {
		setup(t, false)
		assert.Empty(t, run(t, []string{"create", "cluster", "node-1", "--machine-config"}, "amazonec2config/"))
		assert.Empty(t, run(t, []string{"create", "cluster", "node-1", "--cloud-credential"}, ""))
	})

	t.Run("network gate off → compiled-in kind prefixes survive", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"create", "cluster", "node-1", "--machine-config"}, "")
		assert.Contains(t, got, "amazonec2config/\n")
	})
}
