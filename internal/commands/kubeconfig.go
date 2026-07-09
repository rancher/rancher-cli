package commands

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type kubeconfigCmd struct {
	dir     string
	clients engine.ClusterClientFactory
}

// NewKubeconfig builds the top-level `kubeconfig` command (local): it emits an
// exec-credential kubeconfig on stdout, delegating token minting to `auth
// get-token` rather than embedding a long-lived credential.
func NewKubeconfig(dir string, clients engine.ClusterClientFactory) runtime.Command {
	return &kubeconfigCmd{dir: dir, clients: clients}
}

func (kubeconfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "kubeconfig",
		Usage: "Emit an exec-credential kubeconfig for a cluster",
		Flags: []runtime.ArgSpec{
			{Name: "cluster", Type: "string"},
		},
		Examples: []string{
			"rancher kubeconfig --cluster prod > prod.yaml",
		},
		CommandType: runtime.Local,
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c *kubeconfigCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(c.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	serverName, srv, err := cfg.CurrentServer()
	if err != nil {
		return runtime.Result{}, err
	}

	id := in.String("cluster")
	if id != "" {
		id, err = c.clients.ResolveCluster(ctx, id)
	} else {
		id, err = c.clients.ContextCluster()
	}
	if err != nil {
		return runtime.Result{}, err
	}
	if id == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"no cluster specified (no cluster set in the current context)").
			WithHint("pass --cluster <id|name> or set a context")
	}

	caPEM, err := config.ResolveCA(srv.CA)
	if err != nil {
		return runtime.Result{}, err
	}

	base := baseURL(srv.URL)
	entryName := fmt.Sprintf("%s-%s", serverName, id)

	cluster := &clientcmdapi.Cluster{Server: base + "/k8s/clusters/" + id}
	if caPEM != "" {
		cluster.CertificateAuthorityData = []byte(caPEM)
	}

	kc := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{entryName: cluster},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{entryName: {
			Exec: &clientcmdapi.ExecConfig{
				APIVersion:      "client.authentication.k8s.io/v1beta1",
				Command:         "rancher",
				Args:            []string{"auth", "get-token", "--server", base, "--cluster", id, "--name", entryName},
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

// baseURL returns scheme://host for a Rancher server URL, dropping a /v3
// suffix — the same normalization kube.BaseURL applies, kept local here so
// this command does not pull in a client/kube import edge for one helper.
func baseURL(serverURL string) string {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(strings.TrimSuffix(serverURL, "/"), "/v3")
	}
	return u.Scheme + "://" + u.Host
}
