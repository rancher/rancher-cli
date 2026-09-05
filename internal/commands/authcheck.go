package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"golang.org/x/sync/errgroup"
)

type authCheckCmd struct {
	dir   string
	store config.CredStore
	// preflight is the server probe, injectable for tests; defaults to
	// provider.Preflight in NewAuthCheck.
	preflight func(ctx context.Context, eff config.Effective) (string, error)
	// rancherVersion reads the server's stamped version from GET /rancherversion
	// for the PS-8 floor check, injectable for tests; defaults to
	// provider.RancherVersion in NewAuthCheck.
	rancherVersion func(ctx context.Context, eff config.Effective) (string, error)
	// identity is the PS-2 identity probe (POST an empty SelfUser, read back
	// status.userID), injectable for tests; defaults to provider.Identity.
	identity func(ctx context.Context, eff config.Effective) (string, error)
	// token reads back the effective token's ext.cattle.io/v1 Token metadata
	// by name (the id after its "ext/" prefix), injectable for tests;
	// defaults to provider.EffectiveTokenMetadata.
	token func(ctx context.Context, eff config.Effective, name string) (provider.TokenMetadata, error)
	// v3token reads back the effective token's management.cattle.io/v3 Token
	// metadata by name (the id before the ':') for non-ext tokens, injectable
	// for tests; defaults to provider.V3TokenMetadata in NewAuthCheck.
	v3token func(ctx context.Context, eff config.Effective, name string) (provider.TokenMetadata, error)
}

// NewAuthCheck builds the `auth status` command (local; makes a server preflight call).
func NewAuthCheck(dir string, store config.CredStore) runtime.Command {
	return &authCheckCmd{
		dir: dir, store: store,
		preflight:      provider.Preflight,
		rancherVersion: provider.RancherVersion,
		identity:       provider.Identity,
		token:          provider.EffectiveTokenMetadata,
		v3token:        provider.V3TokenMetadata,
	}
}

func (authCheckCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth status",
		Usage:       "Verify connectivity and credentials against the current server",
		CommandType: runtime.Local,
		Examples:    []string{"rancher auth status -o json"},
		Network:     true,
		Auth:        true,
		Shape:       runtime.ShapeText,
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
	// Answer "am I logged in" in state terms, not plumbing: with no
	// effective URL the preflight would fail with the generic
	// NOT_LOGGED_IN; the config-aware form names the configured servers
	// and the re-login shortcut (the common shape right after a logout).
	if eff.URL == "" {
		return runtime.Result{}, cfg.ErrNotLoggedIn()
	}
	ver, err := a.preflight(ctx, eff)
	if err != nil {
		return runtime.Result{}, err
	}

	// credential describes the stored credential when it is the effective one
	// (no RANCHER_CLI_TOKEN override) and has provenance (CLI-created).
	var credential any
	var credentialLine string
	if os.Getenv(config.EnvToken) == "" {
		if s, ok := cfg.Servers[eff.ServerName]; ok && s.Provenance != "" {
			credential, credentialLine = credentialReport(s)
		}
	}

	// The three probes below are independent network calls (version, identity,
	// token) with nothing but eff in common; run them concurrently via
	// errgroup (already a repo dependency - internal/engine/inspect/images.go
	// uses the same pattern) rather than tripling/quadrupling auth status's
	// wall-clock latency running them one after another. Each probe writes
	// only to its own dedicated result variables, so there is no data race
	// between goroutines; none of them ever returns a real error to the
	// group (they self-degrade to null+warning), so g.Wait()'s return is
	// always nil. The result/warning ASSEMBLY below happens only after
	// g.Wait() joins all three, in a FIXED field order (version, identity,
	// token) - so the payload and warnings list stay deterministic
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

	credentialPolicy := config.EffectivePolicy(cfg.CredentialStore)
	text := renderAuthCheckText(true, eff.ServerName, ver, rancherVersion, supported, identity, token, credentialPolicy, pinned, credentialLine)
	data := map[string]any{
		"ok": true, "server": eff.ServerName, "serverVersion": ver,
		"rancherVersion": rancherVersion, "supported": supported,
		"identity": identity, "token": token,
		"credentialStore": map[string]any{
			"policy": credentialPolicy,
			"pinned": pinned,
		},
	}
	if credential != nil {
		data["credential"] = credential
	}
	res := runtime.Result{
		Kind:       "AuthCheck",
		Data:       data,
		Text:       text,
		StyledText: func(s style.Style) string { return styleAuthCheckText(text, true, s) },
	}
	for _, w := range []string{warning, idWarning, tokenWarn} {
		if w != "" {
			res.Warnings = append(res.Warnings, w)
		}
	}
	return res, nil
}

// renderAuthCheckText produces a multi-section key/value text view for the
// default/table output format, following the inspect-cluster convention:
// uppercase section headers, two-space-indented "key: value" lines. Nil
// pointer fields render as "-" (honest null); nil sub-objects render as an
// "<field>: unavailable" line in their section. A plain empty string
// (pinned) also renders as "-" for the same "nothing here" reason: a
// non-auto credential policy and an auto policy on a fresh machine before
// any pin is recorded both produce a legitimately empty value that warrants
// the same honest marker.
// Warnings are NOT duplicated here - they arrive on stderr via the
// pipeline's stderr mirror.
func renderAuthCheckText(
	ok bool,
	serverName, serverVersion string,
	rancherVersion *string,
	supported *bool,
	identity, token any,
	credentialPolicy, pinned string,
	credentialLine string,
) string {
	ptrStr := func(p *string) string {
		if p == nil {
			return "-"
		}
		return *p
	}
	ptrBool := func(p *bool) string {
		if p == nil {
			return "-"
		}
		if *p {
			return "true"
		}
		return "false"
	}
	emptyStr := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}

	var b strings.Builder
	fmt.Fprintf(&b, "AUTH STATUS\n")
	fmt.Fprintf(&b, "  ok: %v\n", ok)
	fmt.Fprintf(&b, "  server: %s\n", serverName)

	fmt.Fprintf(&b, "\nVERSIONS\n")
	fmt.Fprintf(&b, "  serverVersion: %s\n", serverVersion)
	fmt.Fprintf(&b, "  rancherVersion: %s\n", ptrStr(rancherVersion))
	fmt.Fprintf(&b, "  supported: %s\n", ptrBool(supported))

	fmt.Fprintf(&b, "\nIDENTITY\n")
	if idMap, idOK := identity.(map[string]any); idOK {
		userID, _ := idMap["userID"].(string)
		fmt.Fprintf(&b, "  userID: %s\n", userID)
	} else {
		fmt.Fprintf(&b, "  identity: unavailable\n")
	}

	fmt.Fprintf(&b, "\nTOKEN\n")
	if tokMap, tokOK := token.(map[string]any); tokOK {
		name, _ := tokMap["name"].(string)
		kind, _ := tokMap["kind"].(string)
		prov, _ := tokMap["provider"].(string)
		expiresAt, _ := tokMap["expiresAt"].(string)
		current, _ := tokMap["current"].(bool)
		fmt.Fprintf(&b, "  name: %s\n", name)
		fmt.Fprintf(&b, "  kind: %s\n", kind)
		fmt.Fprintf(&b, "  provider: %s\n", prov)
		fmt.Fprintf(&b, "  expiresAt: %s\n", expiresAt)
		fmt.Fprintf(&b, "  current: %v\n", current)
	} else {
		fmt.Fprintf(&b, "  token: unavailable\n")
	}
	if credentialLine != "" {
		fmt.Fprintf(&b, "  %s\n", credentialLine)
	}

	fmt.Fprintf(&b, "\nCREDENTIAL STORE\n")
	fmt.Fprintf(&b, "  policy: %s\n", credentialPolicy)
	fmt.Fprintf(&b, "  pinned: %s\n", emptyStr(pinned))

	return b.String()
}

// authCheckSectionHeaders is the fixed, always-present section header set
// renderAuthCheckText emits - every one of them, every call, regardless of
// which sub-objects came back null.
var authCheckSectionHeaders = map[string]bool{
	"AUTH STATUS": true, "VERSIONS": true, "IDENTITY": true,
	"TOKEN": true, "CREDENTIAL STORE": true,
}

// styleAuthCheckText applies auth status's bold spans to text, the plain
// rendering renderAuthCheckText already produced: every section header
// (bold, whole line), plus the "  ok: false" row in yellow - only the false
// case; a healthy "  ok: true" stays plain, matching what an operator would
// scan for.
func styleAuthCheckText(text string, ok bool, s style.Style) string {
	out := runtime.BoldExactLines(text, authCheckSectionHeaders, s)
	if ok {
		return out
	}
	return runtime.StyleLines(out, s, func(_ int, body string) (string, bool) {
		if body != "  ok: false" {
			return "", false
		}
		return s.Yellow(body), true
	})
}

// checkMinRancherVersion evaluates the server's stamped version (from GET
// /rancherversion) against config.MinRancherVersion (PS-8, warn-only floor).
// Any failure to read it at all - connectivity, a non-200 response, an
// unrecognized body - is folded into the same "cannot determine" posture as
// an unparseable value (a dev/head build): auth status's own success never
// depends on this read.
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
// null - auth status's own success never depends on it either way. Any other
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

// errV3TokenNotFound signals that the management.cattle.io/v3 Token object was
// absent - the management API is always present on a running Rancher server, so
// this unambiguously means the token was revoked or rotated, never a missing
// API group. tokenWarning treats this specially to emit the "re-authenticate"
// advisory rather than the ext.cattle.io group-absent wording.
var errV3TokenNotFound = errors.New("v3 token not found on the server")

// rotatedTokenAdvisory is the warning text for a token whose server-side
// record is absent - shared by the v3 and ext NotFound branches of
// tokenWarning so the two messages cannot drift independently as the probe
// grows.
const rotatedTokenAdvisory = "token metadata unavailable: stored token not found on the server (revoked or rotated) - re-authenticate with rancher auth login"

// probeToken runs the PS-2 token probe for the EFFECTIVE token (eff.Token -
// never the stored credential directly, so a RANCHER_CLI_TOKEN override is
// always the one reported on). The "ext/" prefix sniff is the one the
// token package's bearer validation uses, applied here to the long-lived
// effective credential rather than a token the login created.
//
// For ext/ tokens the probe delegates to a.token (EffectiveTokenMetadata).
// For all other tokens (management.cattle.io/v3, the common case for Rancher
// 2.15 UI-created sessions) the probe delegates to a.v3token (V3TokenMetadata)
// and reports the same five-field shape - name, kind (derived/session),
// provider, expiresAt, current.
//
// A nil a.token or a.v3token (possible when a test builds authCheckCmd directly
// without going through NewAuthCheck) is treated as "not wired for this check"
// and stays silently null - the same "not wired" posture checkIdentity documents
// for a.identity == nil.
//
// An empty eff.Token (no token at all) returns (nil, nil): in practice
// unreachable once preflight has actually succeeded (RESTConfig refuses an
// empty token before any of this runs), but reachable in tests that stub
// preflight without a token.
//
// probeToken returns the raw probe outcome rather than a classified warning:
// classifying a NotFound (see tokenWarning) needs the version probe's result,
// which - now that all three probes run concurrently - is only known once every
// probe has joined, not from inside this one.
func (a authCheckCmd) probeToken(ctx context.Context, eff config.Effective) (any, error) {
	if eff.Token == "" {
		return nil, nil
	}
	id, _, _ := strings.Cut(eff.Token, ":")
	name, ok := strings.CutPrefix(id, "ext/")
	if !ok {
		// Non-ext (management.cattle.io/v3) token: fall through to the v3 probe.
		if a.v3token == nil {
			return nil, nil
		}
		meta, err := a.v3token(ctx, eff, id)
		if err != nil {
			if clierr.IsNotFound(err) {
				return nil, errV3TokenNotFound
			}
			return nil, err
		}
		// V3TokenMetadata maps isDerived → Kind ("derived"/"session") before
		// returning, so no further translation is needed here.
		return map[string]any{
			"name": meta.Name, "kind": meta.Kind, "provider": meta.Provider,
			"expiresAt": meta.ExpiresAt, "current": meta.Current,
		}, nil
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

// tokenWarning turns probeToken's raw outcome into the null-plus-warning text.
// errV3TokenNotFound (a missing management.cattle.io/v3 Token object) always
// maps to the "re-authenticate" advisory - the management API is always present,
// so the absent object is unambiguously stale/revoked, not a missing API group.
//
// A CodeNotFound on the ext token GET is ambiguous by error code alone - it
// means either the ext.cattle.io group itself is absent (below the 2.14 floor)
// or the group is present but the specific named token object was
// deleted/rotated server-side (a stale RANCHER_CLI_TOKEN or credential-store
// id). supported (the version probe's PS-8 floor verdict) resolves the
// ambiguity: once it has confirmed the server IS at or above the floor, the
// same 404 can only be the latter. When supported is nil or false, the
// group-absent wording is the only thing that can honestly be said.
func tokenWarning(err error, supported *bool) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errV3TokenNotFound) {
		return rotatedTokenAdvisory
	}
	if clierr.IsNotFound(err) && supported != nil && *supported {
		return rotatedTokenAdvisory
	}
	return degradeWarning("token", err)
}

// degradeWarning turns a PS-2 probe failure into the null-plus-warning
// posture both identity and token metadata degrade to: RBAC denial, a
// timeout, and the ext.cattle.io group/resource being missing (below the
// 2.14 floor, or genuinely absent for some other reason on a >= 2.14
// server) are all warn-only - auth status's own success (`ok`) never depends
// on either probe. The missing-group wording deliberately does not reuse
// EvalMinVersionSupport's below-floor text: a server the version check
// already reported as >= 2.14 must not also be told it is below the
// minimum just because this group happens to be absent.
func degradeWarning(field string, err error) string {
	if ce, ok := errors.AsType[*clierr.Error](err); ok {
		switch ce.Code {
		case clierr.CodeRBAC:
			return fmt.Sprintf("%s metadata unavailable: denied by RBAC", field)
		case clierr.CodeNotFound:
			return fmt.Sprintf("%s metadata unavailable: ext.cattle.io not served by this server (requires Rancher >= %s, the supported minimum)", field, config.MinRancherVersion)
		}
	}
	return fmt.Sprintf("%s metadata unavailable: %s", field, err)
}
