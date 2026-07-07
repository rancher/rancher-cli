package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

type authCheckCmd struct {
	dir   string
	store config.CredStore
}

// NewAuthCheck builds the `auth check` command (local; makes a server preflight call).
func NewAuthCheck(dir string, store config.CredStore) runtime.Command {
	return &authCheckCmd{dir: dir, store: store}
}

func (authCheckCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth check",
		Usage:       "Verify connectivity and credentials against the current server",
		CommandType: runtime.Local,
		Examples:    []string{"rancher auth check -o json"},
	}
}

func (authCheckCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}

func (a authCheckCmd) Execute(_ context.Context, _ runtime.Input, _ safety.Effect, _ *ui.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(a.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	eff, err := cfg.Effective(a.store)
	if err != nil {
		return runtime.Result{}, err
	}
	rc, err := kube.RESTConfig(eff)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, err.Error()).
			WithHint("run: rancher auth login --server <url>")
	}
	disc, err := kube.Discovery(rc)
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, err.Error())
	}
	ver, err := disc.ServerVersion()
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, "cannot reach server: "+err.Error()).
			WithHint("check the server URL, token, and CA")
	}
	if ver == nil {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, "server returned no version info")
	}
	return runtime.Result{Kind: "AuthCheck", Data: map[string]any{
		"ok": true, "server": eff.ServerName, "cluster": eff.Cluster, "serverVersion": ver.GitVersion,
	}}, nil
}
