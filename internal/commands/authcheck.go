package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
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
	// identity is the PS-2 identity probe (POST an empty SelfUser, read back
	// status.userID), injectable for tests; defaults to provider.Identity.
	identity func(ctx context.Context, eff config.Effective) (string, error)
	// token reads back the effective token's ext.cattle.io/v1 Token metadata
	// by name (the id after its "ext/" prefix), injectable for tests;
	// defaults to provider.EffectiveTokenMetadata.
	token func(ctx context.Context, eff config.Effective, name string) (provider.TokenMetadata, error)
}

// NewAuthCheck builds the `auth check` command (local; makes a server preflight call).
func NewAuthCheck(dir string, store config.CredStore) runtime.Command {
	return &authCheckCmd{
		dir: dir, store: store,
		preflight:      provider.Preflight,
		rancherVersion: provider.ServerVersionSetting,
		identity:       provider.Identity,
		token:          provider.EffectiveTokenMetadata,
	}
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
	identity, idWarning := a.checkIdentity(ctx, eff)
	token, tokenWarning := a.checkToken(ctx, eff)

	res := runtime.Result{Kind: "AuthCheck", Data: map[string]any{
		"ok": true, "server": eff.ServerName, "cluster": eff.Cluster, "serverVersion": ver,
		"rancherVersion": rancherVersion, "supported": supported,
		"identity": identity, "token": token,
		"credentialStore": map[string]any{
			"policy": config.EffectivePolicy(cfg.CredentialStore),
			"pinned": pinned,
		},
	}}
	for _, w := range []string{warning, idWarning, tokenWarning} {
		if w != "" {
			res.Warnings = append(res.Warnings, w)
		}
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

// checkIdentity runs the PS-2 identity probe. A nil identity field (only
// possible when a test builds authCheckCmd directly without going through
// NewAuthCheck) is treated as "not wired for this check" and stays silently
// null — auth check's own success never depends on it either way. Any other
// failure (RBAC denial, timeout, the ext.cattle.io group/resource being
// absent) degrades to null plus a warning, never a check failure.
func (a authCheckCmd) checkIdentity(ctx context.Context, eff config.Effective) (any, string) {
	if a.identity == nil {
		return nil, ""
	}
	userID, err := a.identity(ctx, eff)
	if err != nil {
		return nil, degradeWarning("identity", err)
	}
	return map[string]any{"userID": userID}, ""
}

// checkToken runs the PS-2 token probe for the EFFECTIVE token (eff.Token —
// never the stored credential directly, so a RANCHER_TOKEN override is
// always the one reported on). The "ext/" prefix sniff mirrors
// login_ext.go's revokeSessionToken, applied here to the long-lived
// effective credential rather than the transient session token. A
// non-ext (legacy/norman) token nulls the field with a naming warning and
// never reaches the probe at all — no v3/legacy fallback lookup. An empty
// eff.Token (no token at all) is not "legacy" — there is nothing to report
// on, so this stays silently null: in practice unreachable once preflight
// has actually succeeded (RESTConfig refuses an empty token before any of
// this runs), but reachable in tests that stub preflight without a token.
func (a authCheckCmd) checkToken(ctx context.Context, eff config.Effective) (any, string) {
	if eff.Token == "" {
		return nil, ""
	}
	id, _, _ := strings.Cut(eff.Token, ":")
	name, ok := strings.CutPrefix(id, "ext/")
	if !ok {
		return nil, "legacy token — no ext metadata"
	}
	if a.token == nil {
		return nil, ""
	}
	meta, err := a.token(ctx, eff, name)
	if err != nil {
		return nil, degradeWarning("token", err)
	}
	kind := "derived"
	if meta.Kind == "session" {
		kind = "session"
	}
	return map[string]any{
		"name": meta.Name, "kind": kind, "provider": meta.Provider,
		"expiresAt": meta.ExpiresAt, "current": meta.Current,
	}, ""
}

// degradeWarning turns a PS-2 probe failure into the null-plus-warning
// posture both identity and token metadata degrade to: RBAC denial, a
// timeout, and the ext.cattle.io group/resource being missing (below the
// 2.14 floor, or genuinely absent for some other reason on a >= 2.14
// server) are all warn-only — auth check's own success (`ok`) never depends
// on either probe. The missing-group wording deliberately does not reuse
// EvalMinVersionSupport's below-floor text: a server the version check
// already reported as >= 2.14 must not also be told it is below the
// minimum just because this group happens to be absent.
func degradeWarning(field string, err error) string {
	var ce *clierr.Error
	if errors.As(err, &ce) {
		switch ce.Code {
		case clierr.CodeRBAC:
			return fmt.Sprintf("%s metadata unavailable: denied by RBAC", field)
		case clierr.CodeNotFound:
			return fmt.Sprintf("%s metadata unavailable: ext.cattle.io not served by this server (requires Rancher >= %s, the supported minimum)", field, config.MinRancherVersion)
		}
	}
	return fmt.Sprintf("%s metadata unavailable: %s", field, err)
}
