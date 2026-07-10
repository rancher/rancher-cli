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
	// rancherVersion reads the server-version Setting for the PS-8 floor
	// check, injectable for tests; defaults to provider.ServerVersionSetting.
	rancherVersion func(ctx context.Context, eff config.Effective) (string, error)
}

// NewAuthCheck builds the `auth check` command (local; makes a server preflight call).
func NewAuthCheck(dir string, store config.CredStore) runtime.Command {
	return &authCheckCmd{dir: dir, store: store, preflight: provider.Preflight, rancherVersion: provider.ServerVersionSetting}
}

func (authCheckCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth check",
		Usage:       "Verify connectivity and credentials against the current server",
		CommandType: runtime.Local,
		Examples:    []string{"rancher auth check -o json"},
		Toolset:     "core",
		Network:     true,
		Auth:        true,
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

	rancherVersion, supported, warning := a.checkMinRancherVersion(ctx, eff)

	res := runtime.Result{Kind: "AuthCheck", Data: map[string]any{
		"ok": true, "server": eff.ServerName, "cluster": eff.Cluster, "serverVersion": ver,
		"rancherVersion": rancherVersion, "supported": supported,
		"credentialStore": map[string]any{
			"policy": config.EffectivePolicy(cfg.CredentialStore),
			"pinned": pinned,
		},
	}}
	if warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, nil
}

// checkMinRancherVersion evaluates the server-version Setting against
// config.MinRancherVersion (PS-8, warn-only floor). Any failure to read it
// at all — RBAC denial, a missing setting, connectivity — is folded into
// the same "cannot determine" posture as an unparseable value (a dev/head
// build): auth check's own success never depends on this read.
// rancherVersion is nil only on a read failure; supported is nil whenever
// the version cannot be numerically compared (a read failure OR a
// successfully read but unparseable value).
func (a authCheckCmd) checkMinRancherVersion(ctx context.Context, eff config.Effective) (rancherVersion *string, supported *bool, warning string) {
	if a.rancherVersion == nil {
		return nil, nil, config.UnknownServerVersionWarning
	}
	raw, err := a.rancherVersion(ctx, eff)
	if err != nil {
		return nil, nil, config.UnknownServerVersionWarning
	}
	supported, warning = config.EvalMinVersionSupport(raw)
	return &raw, supported, warning
}
