package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type loginCmd struct {
	dir string
	// newStore resolves the credential store FROM THE POLICY READ UNDER
	// THE LOCK. Production wiring uses config.OpenStoreWithNotice; tests
	// inject a factory returning their fake store. The string return carries
	// any first-pin notice so Execute can route it into res.Warnings rather
	// than writing directly to stderr. Replaces the removed store field (the
	// dependency-resolution store snapshot this race was about).
	newStore func(policy, dir string) (config.CredStore, string, error)
	// tlsPreflight verifies the server's certificate against caPEM (or
	// system roots when caPEM is empty) before any credential is persisted.
	// Plain login (an existing entry) still runs this: it enforces already-
	// pinned trust, it just cannot change it — a certificate that no longer
	// matches fails TLS_TRUST with the remediation in the message body, no
	// machine hint (the new CA path is unknown). Delegated login (--url)
	// never calls this directly: prepareRegistration already ran it as part
	// of establishing trust for the entry. nil is a valid, deliberately
	// inert value: tests that do not exercise TLS behavior leave it unset.
	tlsPreflight tlsPreflightFunc
}

// NewLogin builds the `auth login` command (local). The store is resolved
// inside Execute from the config loaded under the config write lock — never
// from the dependency-resolution snapshot, which can predate a concurrent
// policy change (config-write-lock plan, review issue 2).
func NewLogin(dir string) runtime.Command {
	return &loginCmd{dir: dir, newStore: config.OpenStoreWithNotice, tlsPreflight: tlsPreflightDefault}
}

// tlsPreflightDefault performs the every-https-path TLS preflight: an
// unauthenticated GET against the same /rancherversion endpoint the PS-8
// version probe uses, built with caPEM as the trusted root (system roots
// when empty). Only the TLS handshake matters here — any response status
// proves the certificate verified. A classified certificate-verification
// failure returns tlstrust's structured TLS_TRUST error. Every other
// failure (DNS, connection refused, timeout — the server is unreachable for
// reasons that have nothing to do with certificate trust) is deliberately
// NOT surfaced as a failure here: that stays warn-only, folded into the
// registration min-version check's "cannot determine" posture.
func tlsPreflightDefault(ctx context.Context, serverURL, caPEM string) (string, error) {
	v, err := provider.RancherVersion(ctx, config.Effective{URL: serverURL, CA: caPEM})
	if err != nil {
		if ce := tlstrust.Classify(err, config.ServerHost(serverURL), time.Now()); ce != nil {
			return "", ce
		}
		return "", nil
	}
	return v, nil
}

// pinnedTrustFailure rewrites a TLS_TRUST error from plain login's preflight
// (an existing entry's pinned CA no longer matches): the remediation is
// config update-server, not a fully-known machine command — the new CA path
// is unknown to the CLI — so the suggestion moves to the message body and no
// hint is emitted, per the executable-hint contract. Non-TLS_TRUST errors
// (including nil) pass through unchanged.
func pinnedTrustFailure(err error, name string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeTLSTrust {
		return err
	}
	return ce.WithMessage(ce.Message +
		fmt.Sprintf("; %s's pinned trust no longer matches — to change it, run: rancher config update-server %s --cacert <path>",
			shquote(name), shquote(name))).
		WithHint("")
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth login",
		Usage:       "Login to a Rancher server",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Desc: "server entry `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Desc: "Rancher server `url` to add and log into (requires <name>)"},
			{Name: "token-stdin", Type: "bool", Desc: "read the API token (id:secret) from stdin"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM (requires --url)"},
			{Name: "auth-provider", Type: "string", Desc: "auth provider `name` to sign in with"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}, Desc: "identity-provider `flow`", Env: []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow}},
			{Name: "ttl", Type: "string", Desc: "session-token lifetime `duration`, e.g. 90d"},
		},
		Examples: []string{
			"rancher auth login prod",
			"rancher auth login prod --url https://rancher.ex --token-stdin < token.txt",
		},
	}
}

// CompleteArgs implements runtime.ShellCompleter: `auth login <TAB>`
// completes server names from the already-loaded config snapshot.
func (loginCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	return view.ServerNames()
}

func (l loginCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	name := ""
	if a := in.Args(); len(a) > 0 {
		name = a[0]
	}
	url := in.String("url")
	cacert := in.String("cacert")

	// The "legacy:" prefix is reserved for the legacy token shim's cache
	// slots — checked unconditionally (a name in this namespace can never
	// be a real entry; add-server rejects it the same way at creation).
	if name != "" {
		if ce := validateEntryName(name); ce != nil {
			return runtime.Result{}, ce
		}
	}

	cfg0, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, "reading config: "+err.Error())
	}

	if url != "" {
		return l.executeRegisterAndLogin(ctx, in, p, cfg0, name, url, cacert)
	}

	if name != "" {
		if cacert != "" {
			// Trust changes belong to registration, not authentication. A
			// suggested command must be valid verbatim: only a KNOWN name
			// can receive a fully interpolated update-server hint; an
			// absent/unknown name falls through to VALIDATION/NOT_FOUND
			// below with no hint at all.
			if _, ok := cfg0.Servers[name]; ok {
				return runtime.Result{}, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--cacert without --url changes trust for %q, which belongs to registration", name)).
					WithHint(fmt.Sprintf("rancher config update-server %s %s", shquote(name), caUpdateArg(cacert)))
			}
			return runtime.Result{}, loginUnknownNameError(cfg0, name)
		}
		existing, ok := cfg0.Servers[name]
		if !ok {
			return runtime.Result{}, loginUnknownNameError(cfg0, name)
		}
		return l.authenticatePlain(ctx, in, p, name, existing.URL, existing.CA)
	}

	if cacert != "" {
		// Matches the --cacert-without-url matrix's absent-name row: body
		// guidance only, no machine hint (neither value is known here).
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cacert without --url and without a name has no entry to attach trust to; pass a name to change an existing entry's trust with rancher config update-server")
	}

	cur, srv, cerr := cfg0.CurrentServerEntry()
	if cerr == nil {
		return l.authenticatePlain(ctx, in, p, cur, srv.URL, srv.CA)
	}

	names := sortedKeys(cfg0.Servers)
	switch len(names) {
	case 0:
		return runtime.Result{}, clierr.New(clierr.CodeNotLoggedIn,
			"no servers configured; add one with: rancher auth login <name> --url <url>")
	case 1:
		// Login never selects implicitly: a success here would authenticate
		// an entry the pointer still doesn't name, so the next command
		// would fail NOT_LOGGED_IN anyway. Point at the switch verb
		// instead.
		return runtime.Result{}, clierr.New(clierr.CodeNotLoggedIn, "no current server").
			WithHint(configUseServerHint(names[0]))
	default:
		cands := make([]clierr.Candidate, 0, len(names))
		for _, n := range names {
			cands = append(cands, clierr.Candidate{ID: n, Name: n})
		}
		return runtime.Result{}, clierr.New(clierr.CodeAmbiguousSelection,
			"multiple servers configured; specify a name").WithCandidates(cands)
	}
}

// configUseServerHint returns the bare, shell-quoted `rancher config
// use-server <name>` command — the pointer-switch hint every no-pointer
// login outcome below emits for a concrete name.
func configUseServerHint(name string) string {
	return "rancher config use-server " + shquote(name)
}

// loginUnknownNameError builds NOT_FOUND for an explicit name absent from
// cfg's servers: candidates for a typo, plus body guidance toward the
// registering spelling. No machine hint — the URL isn't known here.
func loginUnknownNameError(cfg *config.Config, name string) *clierr.Error {
	ce := serverNotFoundWithCandidates(cfg, name)
	return ce.WithMessage(ce.Message + "; re-run with --url <url> to add it")
}

// executeRegisterAndLogin is the `--url` path: register (add-if-missing,
// same shared commit seam add-server uses) THEN authenticate against the
// persisted entry — registration completes, atomic, before any auth flow
// starts. A failed auth after this leaves a valid registered entry, a
// first-class state, not a bug to roll back.
func (l loginCmd) executeRegisterAndLogin(ctx context.Context, in runtime.Input, p runtime.Prompter, cfg0 *config.Config, name, url, cacert string) (runtime.Result, error) {
	if name == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--url requires an explicit name: rancher auth login <name> --url <url>")
	}

	existing, known := cfg0.Servers[name]
	// Known name, --cacert absent: prepare with the EXISTING entry's
	// effective selector — the flag's absence is never treated as a switch
	// to system roots (that would silently strip a private CA on a plain
	// re-login-with-add spelling).
	caSelector := cacert
	if cacert == "" && known {
		caSelector = existing.CA
	}

	probe, err := prepareRegistration(ctx, l.tlsPreflight, url, caSelector)
	if err != nil {
		return runtime.Result{}, err
	}

	out, err := commitCreateUnderLock(ctx, l.dir, name, probe)
	if err != nil {
		return runtime.Result{}, err
	}

	return l.authenticateDelegated(ctx, in, p, out.name, out.canonicalURL, out.caSelector, probe.caBytes, probe.versionWarning, out.created)
}

// authenticatePlain authenticates an already-registered entry: no
// registration ran for this call, so it re-verifies the pinned-trust
// preflight itself, and an auth failure is final — there is no unlocked
// registration window behind it to recheck for staleness.
func (l loginCmd) authenticatePlain(ctx context.Context, in runtime.Input, p runtime.Prompter, name, serverURL, caSelector string) (runtime.Result, error) {
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, "", "", false, false)
}

// authenticateDelegated authenticates immediately after this login's own
// delegated registration committed: caBytes and versionWarning are already
// known from that registration's probe (re-running the preflight would
// repeat the same network round trip, and dropping versionWarning here
// would silently lose it — registration ran it, login must still surface
// it), and a failure gets the stale-hint recheck scoped to the
// just-closed unlocked registration window. added is threaded straight
// into LoginResult.
func (l loginCmd) authenticateDelegated(ctx context.Context, in runtime.Input, p runtime.Prompter, name, serverURL, caSelector, caBytes, versionWarning string, added bool) (runtime.Result, error) {
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, caBytes, versionWarning, true, added)
}

// authenticateAndStore runs the auth flow against (name, serverURL,
// caSelector) and, on success, commits the credential under a fresh
// config.WithLock acquisition. Only reached through authenticatePlain/
// authenticateDelegated, which each fix delegated to the right constant
// for their caller — no call site passes a bare bool literal.
func (l loginCmd) authenticateAndStore(
	ctx context.Context, in runtime.Input, p runtime.Prompter,
	name, serverURL, caSelector, caBytes, versionWarning string,
	delegated, added bool,
) (runtime.Result, error) {
	isHTTPS := strings.HasPrefix(serverURL, "https://")
	if !delegated {
		// resolveRegistrationCA is the one place "resolve CA bytes only
		// under https, and only when the selector names something" is
		// encoded — sharing it here keeps this gate from drifting against
		// prepareRegistration's copy.
		b, raerr := resolveRegistrationCA(serverURL, caSelector)
		if raerr != nil {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf(
					"reading stored CA for server entry %q: %s; restore the file, or run: rancher config delete-server %s",
					name, raerr.Error(), shquote(name)))
		}
		caBytes = b
		if isHTTPS && l.tlsPreflight != nil {
			if _, perr := l.tlsPreflight(ctx, serverURL, caBytes); perr != nil {
				return runtime.Result{}, pinnedTrustFailure(perr, name)
			}
		}
	}

	snap := authSnapshot{name: name, canonicalURL: serverURL, caSelector: caSelector, caBytes: caBytes}

	token, authErr := resolveLoginToken(ctx, serverURL, caBytes, in, p)
	if authErr == nil {
		id, secret, ok := strings.Cut(token, ":")
		if !ok || id == "" || secret == "" {
			authErr = clierr.New(clierr.CodeValidation,
				"invalid token (expected non-empty id:secret)").
				WithHint("get a token from the Rancher UI (format id:secret)")
		}
	}
	if authErr != nil {
		if delegated {
			return runtime.Result{}, decorateDelegatedAuthFailure(ctx, l.dir, authErr, snap)
		}
		return runtime.Result{}, authErr
	}

	warnings := []string{}
	if versionWarning != "" {
		warnings = append(warnings, versionWarning)
	}
	var (
		currentServer string
		currentValid  bool
	)
	if err := config.WithLock(ctx, l.dir, func() error {
		cfg, err := config.Load(config.FilePath(l.dir))
		if err != nil {
			return err
		}
		if !snapshotMatchesCurrent(cfg, snap) {
			return clierr.New(clierr.CodeConflict,
				fmt.Sprintf("server entry %q changed during login", name)).
				WithHint(fmt.Sprintf("rancher auth login %s", shquote(name)))
		}
		rawStore, storeNotice, err := l.newStore(cfg.CredentialStore, l.dir)
		if err != nil {
			return err
		}
		if storeNotice != "" {
			warnings = append(warnings, storeNotice)
		}
		// Login is the one place auto may re-probe and upgrade a stale "file"
		// pin to the keychain; the returned store is the one to write into.
		// Call-order coupling: l.newStore (OpenStoreWithNotice) MUST run
		// before RepinForLogin — see RepinForLogin's else-branch comment.
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
			warnings = append(warnings,
				"credential helper is read-only; token not persisted — future commands will re-acquire it through the helper")
		}
		currentServer = cfg.CurrentServer
		if currentServer != "" {
			_, currentValid = cfg.Servers[currentServer]
		}
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}

	res := runtime.Result{Kind: "LoginResult", Data: map[string]any{
		"server": name, "url": serverURL, "currentServer": currentServer, "added": added,
	}}
	res.Warnings = warnings
	// Login never switches the pointer — only registration's
	// bootstrap/repair rule does, and that never reaches THIS path (an
	// already-registered entry). Advise whenever the pointer doesn't name
	// what was just logged into, empty/dangling or a DIFFERENT valid
	// entry alike — don't silently leave the operator wondering why the
	// next command still operates against a different server.
	switch {
	case !currentValid:
		res.Summary = fmt.Sprintf("the current server is not set — switch to it with: rancher config use-server %s", shquote(name))
	case currentServer != name:
		res.Summary = fmt.Sprintf("the current server remains %s — switch to %s with: rancher config use-server %s",
			shquote(currentServer), shquote(name), shquote(name))
	}
	return res, nil
}

// decorateDelegatedAuthFailure scopes a failed delegated login's retry hint
// to CURRENT state, never historical success: registration and
// authentication are separated by an unlocked window, so a concurrent
// delete or repoint during that window must not produce a hint that
// immediately fails NOT_FOUND or targets changed trust. This recheck is
// read-only — it writes nothing — and never replaces authErr, which stays
// the primary error either way.
func decorateDelegatedAuthFailure(ctx context.Context, dir string, authErr error, snap authSnapshot) error {
	ce, ok := errors.AsType[*clierr.Error](authErr)
	if !ok || ctx.Err() != nil {
		// Not a structured error, or cancellation preempts the recheck:
		// preserve the original error verbatim, no hint.
		return authErr
	}

	var current bool
	lockErr := config.WithLock(ctx, dir, func() error {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return err
		}
		current = snapshotMatchesCurrent(cfg, snap)
		return nil
	})
	if lockErr != nil || !current {
		return ce.WithMessage(ce.Message +
			fmt.Sprintf(" (server entry %q may have changed since registration; check it with: rancher config list-servers)", snap.name)).
			WithHint("")
	}
	return ce.WithMessage(ce.Message +
		fmt.Sprintf("; undo the registration with: rancher config delete-server %s", shquote(snap.name))).
		WithHint(config.LoginHintFor(snap.name))
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
