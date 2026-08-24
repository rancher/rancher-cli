package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/provider"
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
}

// NewLogin builds the `auth login` command (local). The store is resolved
// inside Execute from the config loaded under the config write lock — never
// from the dependency-resolution snapshot, which can predate a concurrent
// policy change (config-write-lock plan, review issue 2).
func NewLogin(dir string) runtime.Command {
	return &loginCmd{dir: dir, newStore: config.OpenStoreWithNotice, rancherVersion: provider.RancherVersion}
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

	// Read-only collision precheck BEFORE any interactive/network auth: a
	// conflicting --name must not launch an OAuth/SAML flow or mint a token
	// that can never be stored. Advisory only — the same decision repeats
	// authoritatively under the write lock; a load error here is ignored
	// (the locked path surfaces it).
	if cfg, lerr := config.Load(config.FilePath(l.dir)); lerr == nil {
		if _, ce := resolveServerEntryName(cfg.Servers, in.String("name"), serverURL); ce != nil {
			return runtime.Result{}, ce
		}
	}

	// Auth is outside the lock: interactive/network flows must not hold the
	// config write lock while waiting for user input or a network response —
	// that would starve every concurrent command.
	token, err := resolveLoginToken(ctx, serverURL, in, p)
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
		// The authoritative naming/collision decision, under the write
		// lock. A read-only twin ran before authentication (below, in
		// Execute) so a doomed login never launches an auth flow; state
		// may have changed since, so this one decides.
		resolved, ce := resolveServerEntryName(cfg.Servers, name, serverURL)
		if ce != nil {
			return ce
		}
		name = resolved
		// Only an explicit --cacert replaces a stored CA: a re-login that
		// reuses an entry (token expiry is the everyday case) must never
		// erase the CA the entry was verified with.
		ca := in.String("cacert")
		if existing, ok := cfg.Servers[name]; ok && ca == "" {
			ca = existing.CA
		}
		effectiveCA = ca
		cfg.Servers[name] = config.Server{URL: serverURL, CA: ca}
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
	if warning := l.warnBelowMinVersion(ctx, versionEff); warning != "" {
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
func (l loginCmd) warnBelowMinVersion(ctx context.Context, eff config.Effective) string {
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

func resolveLoginToken(ctx context.Context, server string, in runtime.Input, p runtime.Prompter) (string, error) {
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
		tok, err = promptForToken(ctx, server, in, p)
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
func promptForToken(ctx context.Context, server string, in runtime.Input, p runtime.Prompter) (string, error) {
	if in.String("auth-provider") != "" || in.String("auth-flow") != "" {
		return resolveProviderLoginToken(ctx, server, in, p)
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
		return resolveProviderLoginToken(ctx, server, in, p)
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
			WithHint(fmt.Sprintf("pick another name (--name), or remove the entry first: rancher auth logout %s", name))
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
