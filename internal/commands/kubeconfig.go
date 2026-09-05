package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type kubeconfigCmd struct {
	dir     string
	store   config.CredStore
	clients engine.ClusterClientFactory
}

// NewKubeconfig builds the top-level `kubeconfig` command (local): it emits an
// exec-credential kubeconfig on stdout, delegating token creation to `auth
// get-token` rather than embedding a long-lived credential.
func NewKubeconfig(dir string, store config.CredStore, clients engine.ClusterClientFactory) runtime.Command {
	return &kubeconfigCmd{dir: dir, store: store, clients: clients}
}

func (kubeconfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "kubeconfig",
		Usage: "Print an exec-credential kubeconfig for a cluster",
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` to generate the kubeconfig for"},
		},
		Examples: []string{
			"rancher kubeconfig --cluster prod > prod.yaml",
		},
		CommandType: runtime.Local,
		Network:     true,
		Auth:        true,
		// RawBytes is a bare kubeconfig YAML document - it never varies with
		// -o, so every format but the default table would silently accept
		// and then ignore it (the create kubeconfig precedent).
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table"}},
		Shape:        runtime.ShapeBytes,
	}
}

func (c *kubeconfigCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	// Effective (not CurrentServerEntry) so an env-only session (RANCHER_CLI_URL/
	// RANCHER_CLI_TOKEN, no stored server) works exactly like every other
	// command - kubeconfig used to be the one command that didn't.
	eff, err := cfg.Effective(c.store)
	if err != nil {
		return runtime.Result{}, err
	}
	if eff.URL == "" {
		// Config-aware: after a logout the server entries survive; name
		// the re-login shortcut instead of implying a blank machine.
		return runtime.Result{}, cfg.ErrNotLoggedIn()
	}

	// Defense-in-depth: the pipeline's validateInput normally rejects a
	// missing --cluster first, but Execute must be safe when called directly.
	id := in.String("cluster")
	if id == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--cluster is required")
	}
	id, err = c.clients.ResolveCluster(ctx, id)
	if err != nil {
		return runtime.Result{}, err
	}

	// Effective already cleared eff.CA/eff.ServerName if RANCHER_CLI_URL diverged
	// from the stored server - every command sharing this choke point, not
	// just kubeconfig, so there is nothing kubeconfig-specific left to check
	// here.
	caPEM, err := config.ResolveCA(eff.CA)
	if err != nil {
		return runtime.Result{}, err
	}

	base := kube.BaseURL(eff.URL)
	if !strings.HasPrefix(strings.ToLower(base), "https://") {
		// client-go silently refuses to wire exec-credential auth for a non-https
		// server (no error, no Authorization header) - warn so the resulting
		// kubeconfig's silent 401s are explainable (CW-7). Inverted (warn on
		// NOT-https, case-insensitive) rather than matching "http://" literally:
		// BaseURL's error-fallback path can return a malformed or differently
		// cased scheme that a literal "http://" prefix check would miss.
		p.Notice("server URL is not https; kubectl will silently skip exec-credential auth - use https")
	}
	// eff.ServerName is empty for an env-only session, or when the env
	// override just moved the URL away from the stored server - synthesize a
	// stable entry name from the server host instead.
	serverName := eff.ServerName
	if serverName == "" {
		serverName = config.ServerHost(base)
	}
	entryName := fmt.Sprintf("%s-%s", serverName, id)

	cluster := &clientcmdapi.Cluster{Server: base + "/k8s/clusters/" + id}
	execArgs := []string{"auth", "get-token", "--server", base, "--cluster", id, "--name", entryName}
	if caPEM != "" {
		cluster.CertificateAuthorityData = []byte(caPEM)
		// The exec plugin resolves its own CA from the current server at
		// invocation time, which may not be this server if the user has since
		// switched servers. Pin the CA baked into this kubeconfig explicitly so
		// the cached exec plugin can never authenticate against the wrong trust
		// root.
		execArgs = append(execArgs, "--cacert", caPEM)
	}

	kc := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{entryName: cluster},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{entryName: {
			Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1beta1",
				Command:         "rancher",
				Args:            execArgs,
				InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
			},
		}},
		Contexts: map[string]*clientcmdapi.Context{entryName: {
			Cluster:  entryName,
			AuthInfo: entryName,
		}},
		CurrentContext: entryName,
	}

	b, err := clientcmd.Write(kc)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{RawBytes: b}, nil
}
