package commands

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type authCheckCmd struct {
	dir   string
	store config.CredStore
	// preflight is the server probe, injectable for tests; defaults to
	// provider.Preflight in NewAuthCheck.
	preflight func(ctx context.Context, eff config.Effective) (string, error)
}

// NewAuthCheck builds the `auth check` command (local; makes a server preflight call).
func NewAuthCheck(dir string, store config.CredStore) runtime.Command {
	return &authCheckCmd{dir: dir, store: store, preflight: provider.Preflight}
}

func (authCheckCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth check",
		Usage:       "Verify connectivity and credentials against the current server",
		CommandType: runtime.Local,
		Examples:    []string{"rancher auth check -o json"},
	}
}

func (a authCheckCmd) Execute(ctx context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	cfg, err := config.Load(config.FilePath(a.dir))
	if err != nil {
		return runtime.Result{}, err
	}
	// Only the auto policy consults the pin; explicit policies have no pin.
	pinned := ""
	if config.EffectivePolicy(cfg.CredentialStore) == "auto" {
		pinned = config.ReadPin(a.dir)
	}
	eff, err := cfg.Effective(a.store)
	if err != nil {
		return runtime.Result{}, err
	}
	ver, err := a.preflight(ctx, eff)
	if err != nil {
		return runtime.Result{}, err
	}
	return runtime.Result{Kind: "AuthCheck", Data: map[string]any{
		"ok": true, "server": eff.ServerName, "cluster": eff.Cluster, "serverVersion": ver,
		"credentialStore": map[string]any{
			"policy": config.EffectivePolicy(cfg.CredentialStore),
			"pinned": pinned,
		},
	}}, nil
}
