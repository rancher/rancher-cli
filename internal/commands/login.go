package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type loginCmd struct {
	dir string
	// rancherVersion reads the server's stamped version from GET /rancherversion
	// for the PS-8 floor check, injectable for tests; defaults to
	// provider.RancherVersion — the same PS-8 read `auth status` uses,
	// unauthenticated and requiring no engine client factory.
	rancherVersion func(ctx context.Context, eff config.Effective) (string, error)
	// newStore resolves the credential store FROM THE POLICY READ UNDER
	// THE LOCK. Production wiring uses config.OpenStoreWithNotice; tests
	// inject a factory returning their fake store. The string return carries
	// any first-pin notice so Execute can route it into res.Warnings rather
	// than writing directly to stderr. Replaces the removed store field (the
	// dependency-resolution store snapshot this race was about).
	newStore func(policy, dir string) (config.CredStore, string, error)
	// tlsPreflight verifies the server's certificate against caPEM (or
	// system roots when caPEM is empty) before any credential or config is
	// persisted — the every-https-login-path check that closes the gap
	// where a token-stdin/pasted-token login made no network call at all
	// before writing an entry (the only unconditional post-login request,
	// warnBelowMinVersion, degrades every connection failure to a warning).
	// nil is a valid, deliberately inert value: tests that do not exercise
	// TLS behavior leave it unset and Execute skips the check, the same
	// nil-is-inert convention rancherVersion already uses. NewLogin always
	// wires the real check; only NewLogin's production wiring result can
	// reach a real server.
	tlsPreflight func(ctx context.Context, serverURL, caPEM string) (string, error)
}

// NewLogin builds the `auth login` command (local). The store is resolved
// inside Execute from the config loaded under the config write lock — never
// from the dependency-resolution snapshot, which can predate a concurrent
// policy change (config-write-lock plan, review issue 2).
func NewLogin(dir string) runtime.Command {
	return &loginCmd{
		dir: dir, newStore: config.OpenStoreWithNotice, rancherVersion: provider.RancherVersion,
		tlsPreflight: tlsPreflightDefault,
	}
}

// tlsPreflightDefault performs the every-https-path TLS preflight: an
// unauthenticated GET against the same /rancherversion endpoint the PS-8
// version probe uses, built with caPEM as the trusted root (system roots
// when empty). Only the TLS handshake matters here — any response status
// proves the certificate verified. A classified certificate-verification
// failure returns tlstrust's structured TLS_TRUST error. Every other
// failure (DNS, connection refused, timeout — the server is unreachable for
// reasons that have nothing to do with certificate trust) is deliberately
// NOT surfaced as a login failure here: that stays the existing warn-only
// posture the post-login version probe already has for an unreachable
// server, unchanged by this preflight. This narrows the check to exactly
// the gap this preflight exists for (an untrusted certificate silently accepted)
// rather than making login stricter about plain connectivity than it has
// ever been.
func tlsPreflightDefault(ctx context.Context, serverURL, caPEM string) (string, error) {
	// One primitive owns the GET /rancherversion round trip (request
	// construction, CA resolution, timeout, non-200 and malformed-body
	// handling): provider.RancherVersion — the preflight only adds the
	// TLS-failure classification on top. A certificate failure returns the
	// structured TLS_TRUST error; every other failure (DNS, refused,
	// timeout, non-JSON body) keeps the existing warn-only posture for an
	// unreachable server. The version comes back on success so the
	// post-login floor check never re-fetches the same endpoint.
	v, err := provider.RancherVersion(ctx, config.Effective{URL: serverURL, CA: caPEM})
	if err != nil {
		if ce := tlstrust.Classify(err, config.ServerHost(serverURL), time.Now()); ce != nil {
			return "", ce
		}
		return "", nil
	}
	return v, nil
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth login",
		Usage:       "Login to a Rancher server",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "server", Type: "string", Required: true, Desc: "Rancher server `url` (scheme required)"},
			{Name: "token-stdin", Type: "bool", Desc: "read the API token (id:secret) from stdin"},
			{Name: "name", Type: "string", Desc: "`name` for the saved server entry; defaults to the existing entry for this URL, else the server's host"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM"},
			{Name: "auth-provider", Type: "string", Desc: "auth provider `name` to sign in with"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}, Desc: "identity-provider `flow`", Env: []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow}},
			{Name: "ttl", Type: "string", Desc: "session-token lifetime `duration`, e.g. 90d"},
		},
		Examples: []string{
			"rancher auth login --server https://rancher.ex --token-stdin --name prod < token.txt",
		},
	}
}

func (l loginCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	// The "legacy:" prefix is reserved for the legacy token shim's cache
	// slots. A server entry with that name would win the shim's exact-name
	// stored-login tier and defeat its documented server-ranking guarantee,
	// so reject it here — before any interactive auth runs — the same way
	// auth get-token rejects it as a --name.
	if config.IsLegacyCacheSlot(in.String("name")) {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--name values starting with '"+config.LegacyCachePrefix+"' are reserved for the legacy token shim's cache slots").
			WithHint("use a different name; 'rancher token' (the legacy shim) uses this prefix internally")
	}

	// Defense-in-depth: the pipeline's validateInput normally rejects a missing
	// --server first, but Execute must be safe when called directly (tests).
	server := in.String("server")
	if server == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--server is required").
			WithHint("pass --server <url>")
	}
	// Canonicalize once (scheme://host only — lowercase scheme/host, default
	// port stripped) and use the canonical form everywhere below —
	// config.yaml, the credential-store ref, and the echoed result — so the
	// persisted spelling always matches the identity credential helpers key
	// by. CanonicalServerURL discards path, query, fragment, AND userinfo;
	// dropping userinfo is a security requirement: https://admin:secret@host
	// must not persist credentials into config.yaml (secrets live only in
	// the credential store). It also rejects a schemeless or hostless value,
	// so it is the sole validation this input needs.
	serverURL, cerr := credhelper.CanonicalServerURL(server)
	if cerr != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "invalid server URL: "+server)
	}

	// Read-only collision precheck + CA-selector resolution, both advisory,
	// BEFORE any interactive/network auth: a conflicting --name must not
	// launch an OAuth/SAML flow or mint a token that can never be stored,
	// and provider auth below must dial with the CA this login would
	// actually persist with (explicit --cacert, else the entry the naming
	// algorithm would select's stored CA), never silently fall back to
	// system roots. Both decisions repeat authoritatively under the write
	// lock and are compared against this plan there; a load error here is
	// fatal — this read feeds the trust decision.
	preCfg, lerr := config.Load(config.FilePath(l.dir))
	if lerr != nil {
		// This read now feeds the TRUST decision (which stored CA backs the
		// preflight and provider auth) — a failed load must never silently
		// collapse to system roots for a server with a stored custom CA.
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, "reading config: "+lerr.Error())
	}
	servers := preCfg.Servers
	planName, ce := resolveServerEntryName(servers, in.String("name"), serverURL)
	if ce != nil {
		return runtime.Result{}, ce
	}
	_, planExisted := servers[planName]
	caSelector := in.String("cacert")
	explicitCA := caSelector != ""
	if !explicitCA && planExisted {
		caSelector = servers[planName].CA
	}
	// CA resolution matters only for https: an http server never verifies
	// a certificate, and a stale stored CA file must not break an http (or
	// any) login that never touches TLS. The SELECTOR is still persisted
	// unchanged either way.
	isHTTPS := strings.HasPrefix(serverURL, "https://")
	var caPEM string
	var raerr error
	if isHTTPS {
		caPEM, raerr = config.ResolveCA(caSelector)
	}
	if raerr != nil {
		if explicitCA {
			return runtime.Result{}, clierr.New(clierr.CodeValidation, "reading --cacert: "+raerr.Error())
		}
		// A missing/unreadable STORED path fails before any handshake — no
		// unknown-authority classification is possible, so this is a hard
		// local error naming the entry, never a silent system-roots
		// fallback and never a guided-trust state machine in v1.
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("reading stored CA for server entry %q: %s", planName, raerr.Error())).
			WithHint(fmt.Sprintf("restore the file, or remove the entry and log in again with --cacert: rancher config delete-server %s", planName))
	}

	// The every-https-login-path TLS preflight runs BEFORE any
	// authentication flow: the provider flow MINTS a live server-side
	// token, and a preflight failing after that mint would orphan it (no
	// local record, never revoked). Token paths gain the same check —
	// without it they make no verified network call at all before
	// persisting, so a self-signed server could "log in successfully" and
	// store an entry nothing can ever use. A successful preflight also
	// returns the server's version, saving the post-login floor probe a
	// second round trip to the same endpoint.
	var preflightVersion string
	preflightRan := false
	if isHTTPS && l.tlsPreflight != nil {
		v, perr := l.tlsPreflight(ctx, serverURL, caPEM)
		if perr != nil {
			return runtime.Result{}, perr
		}
		preflightVersion = v
		preflightRan = true
	}

	// Auth is outside the lock: interactive/network flows must not hold the
	// config write lock while waiting for user input or a network response —
	// that would starve every concurrent command. caPEM is resolved ONCE
	// above and threaded through unchanged from here on — the provider flow
	// must verify against the same snapshot the preflight checked, never
	// rediscover trust independently by re-reading a path.
	token, err := resolveLoginToken(ctx, serverURL, caPEM, in, p)
	if err != nil {
		return runtime.Result{}, err
	}
	id, secret, ok := strings.Cut(token, ":")
	if !ok || id == "" || secret == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"invalid token (expected non-empty id:secret)").
			WithHint("get a token from the Rancher UI (format id:secret)")
	}

	name := in.String("name")

	// Load → decide → write under the config write lock. The store is
	// resolved from the policy read HERE, not from the dependency-resolution
	// snapshot, which can predate a concurrent policy change
	// (config-write-lock plan, review issue 2).
	var warnings []string
	// effectiveCA escapes the closure: the version probe below must use the
	// CA the entry was actually persisted with (a nameless re-login
	// preserves the stored CA), never the raw --cacert flag, which is empty
	// exactly in that everyday case.
	var effectiveCA string
	if err := config.WithLock(ctx, l.dir, func() error {
		cfg, err := config.Load(config.FilePath(l.dir))
		if err != nil {
			return err
		}
		// The authoritative naming decision, under the write lock,
		// COMPARED AGAINST THE PRE-AUTH PLAN — authentication ran against
		// planName's CA, so a drifted selection (a concurrent writer
		// inserting an earlier alias) or drifted trust state (the selected
		// entry's CA replaced or the entry deleted) must conflict, never
		// be silently adopted: the credentials were verified under a
		// different trust target.
		resolved, ce := resolveServerEntryName(cfg.Servers, name, serverURL)
		if ce != nil {
			return ce
		}
		if resolved != planName {
			return clierr.New(clierr.CodeConflict,
				fmt.Sprintf("server entries changed during login: planned to write %q, would now write %q", planName, resolved)).
				WithHint("re-run the login")
		}
		// Existence drift conflicts on BOTH edges: a concurrently DELETED
		// entry must never be silently resurrected with the CA snapshotted
		// before the deletion (the explicit --name path resolves to the
		// same name whether or not the entry exists, so the name check
		// alone cannot catch it), and an entry APPEARING under the planned
		// name means authentication ran under trust that is not the
		// entry's.
		existing, nowExists := cfg.Servers[resolved]
		if nowExists != planExisted {
			return clierr.New(clierr.CodeConflict,
				fmt.Sprintf("server entry %q was %s during login", resolved,
					map[bool]string{true: "created", false: "deleted"}[nowExists])).
				WithHint("re-run the login")
		}
		if !explicitCA && nowExists && existing.CA != caSelector {
			return clierr.New(clierr.CodeConflict,
				fmt.Sprintf("stored CA for server entry %q changed during login", resolved)).
				WithHint("re-run the login")
		}
		// Selector string matching is not enough for a PATH-valued CA:
		// its file content can rotate under the unchanged path while the
		// login is in flight, silently swapping the trust the next command
		// will read for the trust this login was verified under — compare
		// the resolved BYTES against the pre-auth snapshot, for the
		// EXPLICIT --cacert path exactly as for a stored one (an inline
		// PEM selector resolves to itself and can never conflict; an
		// unresolvable path = the file vanished mid-login, the same
		// conflict).
		if isHTTPS && caSelector != "" {
			nowPEM, rerr := config.ResolveCA(caSelector)
			if rerr != nil || nowPEM != caPEM {
				return clierr.New(clierr.CodeConflict,
					fmt.Sprintf("CA content for server entry %q changed during login", resolved)).
					WithHint("re-run the login")
			}
		}
		name = resolved
		// The persisted CA is exactly the selector authentication was
		// verified under (explicit --cacert, else the planned entry's
		// stored CA) — one precedence decision, made pre-auth, never
		// re-derived here.
		effectiveCA = caSelector
		cfg.Servers[name] = config.Server{URL: serverURL, CA: caSelector}
		cfg.CurrentServer = name

		rawStore, storeNotice, err := l.newStore(cfg.CredentialStore, l.dir)
		if err != nil {
			return err
		}
		if storeNotice != "" {
			warnings = append(warnings, storeNotice)
		}
		// Login is the one place auto may re-probe and upgrade a stale "file" pin
		// to the keychain; the returned store is the one to write into.
		// Call-order coupling: l.newStore (OpenStoreWithNotice) MUST run before
		// RepinForLogin. OpenStoreWithNotice pins the dir on a fresh machine, so
		// RepinForLogin sees a non-empty pin and takes the re-pin branch (not the
		// first-pin else branch). Replacing l.newStore with bare OpenStore, or
		// reordering the two calls, would cause RepinForLogin to emit a second
		// NoticeFirstPinKeychain alongside the one already in storeNotice. See
		// RepinForLogin's else-branch comment for the full coupling explanation.
		store, pinNotice, err := config.RepinForLogin(l.dir, cfg.CredentialStore, rawStore)
		if err != nil {
			return err
		}
		if pinNotice != "" {
			warnings = append(warnings, pinNotice)
		}
		if err := store.SetToken(config.ServerRef{Name: name, URL: serverURL}, token); err != nil {
			if !errors.Is(err, config.ErrTokenStoreUnsupported) {
				return err
			}
			// Read-only helper: a declared, expected outcome — proceed exactly as
			// on success, but say the token was not persisted.
			warnings = append(warnings,
				"credential helper is read-only; token not persisted — future commands will re-acquire it through the helper")
		}
		return cfg.Write()
	}); err != nil {
		return runtime.Result{}, err
	}

	res := runtime.Result{Kind: "LoginResult", Data: map[string]string{
		"server": name, "url": serverURL,
	}}
	res.Warnings = warnings
	versionEff := config.Effective{ServerName: name, URL: serverURL, CA: effectiveCA, Token: token}
	if warning := l.warnBelowMinVersion(ctx, versionEff, preflightVersion, preflightRan); warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, nil
}

// warnBelowMinVersion evaluates the server-version Setting read via
// l.rancherVersion against config.MinRancherVersion (PS-8, warn-only floor).
// Any failure to read it at all — connectivity, a non-200 response,
// unrecognized body — is folded into the same "cannot determine" posture as
// an unparseable value (a dev/head build): this probe never turns a
// successful login into a failure.
func (l loginCmd) warnBelowMinVersion(ctx context.Context, eff config.Effective, known string, alreadyChecked bool) string {
	// known is the version the TLS preflight already fetched from the same
	// endpoint. When the preflight RAN and got no version (unreachable, or
	// a 200 whose body is not the expected JSON), a second round trip to
	// the identical endpoint would only reproduce the same outcome — skip
	// it and report unknown directly.
	if known != "" {
		_, warning := config.EvalMinVersionSupport(known)
		return warning
	}
	if alreadyChecked {
		return config.UnknownServerVersionWarning
	}
	if l.rancherVersion == nil {
		return config.UnknownServerVersionWarning
	}
	raw, err := l.rancherVersion(ctx, eff)
	if err != nil {
		return config.UnknownServerVersionWarning
	}
	_, warning := config.EvalMinVersionSupport(raw)
	return warning
}

func resolveLoginToken(ctx context.Context, server, caPEM string, in runtime.Input, p runtime.Prompter) (string, error) {
	if in.Bool("token-stdin") {
		if in.String("auth-provider") != "" || in.String("auth-flow") != "" || in.String("ttl") != "" {
			return "", clierr.New(clierr.CodeValidation,
				"--auth-provider/--auth-flow/--ttl apply to the provider sign-in flow and are incompatible with --token-stdin").
				WithHint("drop --token-stdin to use the provider flow, or drop the provider flags to paste/pipe a token")
		}
	}

	var (
		tok string
		err error
	)
	switch {
	case in.Bool("token-stdin"):
		tok, err = p.ReadLine(ctx)
	// The provider flow is allowed without a TTY when --auth-provider is
	// explicitly named: the finer-grained gates already inside it (SAML's
	// NoInput check, basicAuth's per-prompt CanPrompt check, OAuth's lack of
	// any TTY requirement) are the real gate — mirroring how `auth get-token`
	// already runs its OAuth device-code/auth-code flows headlessly. Without
	// an explicit provider, the paste-or-provider menu itself needs a TTY.
	case p.CanPrompt() || in.String("auth-provider") != "":
		tok, err = promptForToken(ctx, server, caPEM, in, p)
	default:
		return "", clierr.New(clierr.CodeValidation, "token required").
			WithHint("pipe it via --token-stdin, or run in a terminal")
	}
	if err != nil {
		// A typed clierr (from the provider flow, or an invalid menu pick) is
		// already the right error to surface — the io.EOF/generic mapping
		// below is for the plain-paste read paths only.
		if _, ok := errors.AsType[*clierr.Error](err); ok {
			return "", err
		}
		if errors.Is(err, io.EOF) {
			return "", clierr.New(clierr.CodeValidation, "no token provided").
				WithHint("pipe the token via --token-stdin, or enter it at the prompt")
		}
		return "", clierr.New(clierr.CodeGeneric, "reading token: "+err.Error())
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", clierr.New(clierr.CodeValidation, "no token provided").
			WithHint("pipe the token via --token-stdin, or enter it at the prompt")
	}
	return tok, nil
}

// promptForToken offers the paste-or-provider choice (EXT-R-8): --auth-provider
// or --auth-flow already signal provider intent and skip straight to it,
// mirroring get-token's own explicit-flag shortcut past its provider menu.
func promptForToken(ctx context.Context, server, caPEM string, in runtime.Input, p runtime.Prompter) (string, error) {
	if in.String("auth-provider") != "" || in.String("auth-flow") != "" {
		return resolveProviderLoginToken(ctx, server, caPEM, in, p)
	}
	p.Notice("1) paste an API token\n2) sign in via an identity provider")
	choice, err := p.Prompt(ctx, "Select [1]: ")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(choice) {
	case "", "1":
		return p.PromptSecret(ctx, "Enter token (id:secret): ")
	case "2":
		return resolveProviderLoginToken(ctx, server, caPEM, in, p)
	default:
		return "", clierr.New(clierr.CodeAmbiguousSelection, "select 1 (paste) or 2 (identity provider)").
			WithHint("re-run and answer 1 or 2")
	}
}

// resolveServerEntryName decides which entry a login writes to.
// Explicit name: allowed when the entry is absent or already points at
// the same canonical server (config.SameServerURL — stored spellings
// are not guaranteed byte-identical to a fresh canonicalization); a
// different server is a refusal, never an overwrite. Nameless: reuse
// the lexicographically first entry with the same canonical URL (a
// user-chosen name is never shadowed by a derived duplicate), else the
// first free derived candidate — host[:port], then scheme-host[:port]
// (http://X and https://X are different server identities and need
// different entries) — skipping any candidate the legacy shim's
// reserved "legacy:" name prefix would claim (a host literally named
// legacy with a port would collide with its cache-slot namespace).
func resolveServerEntryName(servers map[string]config.Server, explicit, serverURL string) (string, *clierr.Error) {
	refusal := func(name, existingURL string) *clierr.Error {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("server entry %q already points at %s", name, existingURL)).
			WithHint(fmt.Sprintf("pick another name (--name), or delete the entry first: rancher config delete-server %s", name))
	}
	if explicit != "" {
		if existing, ok := servers[explicit]; ok && !config.SameServerURL(existing.URL, serverURL) {
			return "", refusal(explicit, existing.URL)
		}
		return explicit, nil
	}
	match := ""
	for entryName, s := range servers {
		// A hand-edited entry inside the legacy shim's reserved namespace
		// is never reused — the CLI must not write into it even when its
		// URL matches (the derive ladder and the explicit --name check
		// enforce the same guard on their paths).
		if config.IsLegacyCacheSlot(entryName) {
			continue
		}
		if config.SameServerURL(s.URL, serverURL) && (match == "" || entryName < match) {
			match = entryName
		}
	}
	if match != "" {
		return match, nil
	}
	scheme, host, _ := strings.Cut(serverURL, "://")
	for _, candidate := range []string{host, scheme + "-" + host} {
		if config.IsLegacyCacheSlot(candidate) {
			continue
		}
		if _, taken := servers[candidate]; !taken {
			return candidate, nil
		}
	}
	return "", clierr.New(clierr.CodeValidation,
		fmt.Sprintf("cannot derive an entry name for %s", serverURL)).
		WithHint("pass --name <name>")
}
