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
	"golang.org/x/sync/errgroup"
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

	// The three probes below are independent network calls (version, identity,
	// token) with nothing but eff in common; run them concurrently via
	// errgroup (already a repo dependency — internal/engine/inspect/images.go
	// uses the same pattern) rather than tripling/quadrupling auth check's
	// wall-clock latency running them one after another. Each probe writes
	// only to its own dedicated result variables, so there is no data race
	// between goroutines; none of them ever returns a real error to the
	// group (they self-degrade to null+warning), so g.Wait()'s return is
	// always nil. The result/warning ASSEMBLY below happens only after
	// g.Wait() joins all three, in a FIXED field order (version, identity,
	// token) — so the payload and warnings list stay deterministic
	// regardless of which goroutine happens to finish first.
	var (
		rancherVersion *string
		supported      *bool
		warning        string
		identity       any
		idWarning      string
		tokenRaw       any
		tokenErr       error
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		rancherVersion, supported, warning = a.checkMinRancherVersion(gctx, eff)
		return nil
	})
	g.Go(func() error {
		identity, idWarning = a.checkIdentity(gctx, eff)
		return nil
	})
	g.Go(func() error {
		tokenRaw, tokenErr = a.probeToken(gctx, eff)
		return nil
	})
	_ = g.Wait()

	// tokenWarning needs supported (the version probe's PS-8 floor verdict,
	// only known once the goroutines above have joined) to classify a
	// token-probe 404, so it happens here rather than inside probeToken.
	token := tokenRaw
	tokenWarn := tokenWarning(tokenErr, supported)

	res := runtime.Result{Kind: "AuthCheck", Data: map[string]any{
		"ok": true, "server": eff.ServerName, "cluster": eff.Cluster, "serverVersion": ver,
		"rancherVersion": rancherVersion, "supported": supported,
		"identity": identity, "token": token,
		"credentialStore": map[string]any{
			"policy": config.EffectivePolicy(cfg.CredentialStore),
			"pinned": pinned,
		},
	}}
	for _, w := range []string{warning, idWarning, tokenWarn} {
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

// errLegacyToken signals probeToken's no-v3-fallback case: a non-ext
// (legacy/norman) effective token skips the probe entirely and degrades
// with a fixed naming warning, never the ext.cattle.io lookup.
var errLegacyToken = errors.New("legacy token — no ext metadata")

// probeToken runs the PS-2 token probe for the EFFECTIVE token (eff.Token —
// never the stored credential directly, so a RANCHER_TOKEN override is
// always the one reported on). The "ext/" prefix sniff mirrors
// login_ext.go's revokeSessionToken, applied here to the long-lived
// effective credential rather than the transient session token. A
// non-ext (legacy/norman) token returns errLegacyToken and never reaches the
// probe at all — no v3/legacy fallback lookup. An empty eff.Token (no token
// at all) is not "legacy" — there is nothing to report on, so this returns
// (nil, nil): in practice unreachable once preflight has actually succeeded
// (RESTConfig refuses an empty token before any of this runs), but reachable
// in tests that stub preflight without a token. A nil token field is the
// same "not wired for this check" case checkIdentity documents for
// a.identity == nil.
//
// probeToken returns the raw probe outcome rather than a classified
// warning: classifying a NotFound (see tokenWarning) needs the version
// probe's result, which — now that all three probes run concurrently
// (finding 4) — is only known once every probe has joined, not from inside
// this one.
func (a authCheckCmd) probeToken(ctx context.Context, eff config.Effective) (any, error) {
	if eff.Token == "" {
		return nil, nil
	}
	id, _, _ := strings.Cut(eff.Token, ":")
	name, ok := strings.CutPrefix(id, "ext/")
	if !ok {
		return nil, errLegacyToken
	}
	if a.token == nil {
		return nil, nil
	}
	meta, err := a.token(ctx, eff, name)
	if err != nil {
		return nil, err
	}
	kind := "derived"
	if meta.Kind == "session" {
		kind = "session"
	}
	return map[string]any{
		"name": meta.Name, "kind": kind, "provider": meta.Provider,
		"expiresAt": meta.ExpiresAt, "current": meta.Current,
	}, nil
}

// tokenWarning turns probeToken's raw outcome into the null-plus-warning
// text (finding 1): a CodeNotFound on the token GET is ambiguous by error
// code alone — it means either the ext.cattle.io group itself is absent
// (below the 2.14 floor) or the group is present but the specific named
// token object was deleted/rotated server-side (a stale RANCHER_TOKEN or
// credential-store id). supported (the version probe's PS-8 floor verdict)
// resolves the ambiguity: once it has confirmed the server IS at or above
// the floor, the same 404 can only be the latter, and the caller must
// re-authenticate — it must never also be told the group requires a newer
// Rancher when the version probe just said otherwise. When supported is nil
// or false, the group-absent wording (shared with degradeWarning's identity
// case) is the only thing that can honestly be said.
func tokenWarning(err error, supported *bool) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errLegacyToken) {
		return err.Error()
	}
	var ce *clierr.Error
	if errors.As(err, &ce) && ce.Code == clierr.CodeNotFound && supported != nil && *supported {
		return "token metadata unavailable: stored token not found on the server (revoked or rotated) — re-authenticate with rancher auth login"
	}
	return degradeWarning("token", err)
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
