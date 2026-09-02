package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
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
	// verify performs an authenticated self-read against the resolved token
	// before it is persisted. Follows tlsPreflight's own nil-skip idiom:
	// production wiring sets it to provider.Preflight; a test-constructed
	// struct that leaves it nil never dials out for this check. Skipped
	// unconditionally for a provider-issued token (already authenticated by
	// construction) and whenever --no-verify is set.
	verify loginVerifyFunc
	// newIdPClient builds the client the provider flow dials the identity
	// provider with. Production wiring returns httpx.NewExternal (operating
	// system roots only); tests inject a client pinned to their IdP fixture.
	// nil means the production default.
	newIdPClient func() *http.Client
}

// idpClient resolves newIdPClient with its production default.
func (l loginCmd) idpClient() *http.Client {
	if l.newIdPClient != nil {
		return l.newIdPClient()
	}
	return httpx.NewExternal(httpx.DefaultTimeout)
}

// NewLogin builds the `auth login` command (local). The store is resolved
// inside Execute from the config loaded under the config write lock — never
// from the dependency-resolution snapshot, which can predate a concurrent
// policy change (config-write-lock plan, review issue 2).
func NewLogin(dir string) runtime.Command {
	return &loginCmd{dir: dir, newStore: config.OpenStoreWithNotice, tlsPreflight: tlsPreflightDefault, verify: provider.Preflight}
}

// loginVerifyFunc matches provider.Preflight's shape: an authenticated
// self-read against a resolved (URL, CA, token) triple.
type loginVerifyFunc func(ctx context.Context, eff config.Effective) (string, error)

// trustCommitConfigWriteHookForTest replaces the guided-trust commit's
// config.Write call when non-nil, so a test can force the config-write
// half of the commit protocol to fail WITHOUT ever touching disk (the real
// Write would otherwise already have succeeded by the time a test could
// observe an injected failure). Nil in production.
var trustCommitConfigWriteHookForTest func() error

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
		if ce := tlstrust.Classify(err, serverLabel(serverURL), time.Now()); ce != nil {
			return "", ce
		}
		return "", nil
	}
	return v, nil
}

// serverLabel renders the server as host[:port] with a scheme-default port
// omitted, the spelling the origin-attributed classifier uses, so the plain
// and provider login paths name the same server identically in a
// certificate error. Falls back to the URL's host, lowercased like the
// origin spelling, when the URL has no usable origin.
func serverLabel(serverURL string) string {
	if u, err := neturl.Parse(serverURL); err == nil {
		if origin, oerr := httpx.OriginOf(u); oerr == nil {
			return origin.HostPort()
		}
	}
	return strings.ToLower(config.ServerHost(serverURL))
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
		Network:     true,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Desc: "server entry `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Desc: "Rancher server `url` to add and log into; when name is omitted, the server's hostname becomes the entry name"},
			{Name: "token-stdin", Type: "bool", Desc: "read the API token (id:secret) from stdin"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM (requires --url)"},
			{Name: "auth-provider", Type: "string", Desc: "auth provider `name` to sign in with"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}, Desc: "identity-provider `flow`", Env: []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow}},
			{Name: "ttl", Type: "string", Desc: "session-token lifetime `duration`, e.g. 90d"},
			{Name: "no-verify", Type: "bool", Desc: "store the token without an authenticated round-trip against the server"},
		},
		Examples: []string{
			"rancher auth login prod",
			"rancher auth login prod --url https://rancher.ex --token-stdin < token.txt",
		},
		Shape: runtime.ShapeText,
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
		// An explicitly passed empty string is a caller bug (usually an
		// unset shell variable), never a request to derive — deriving
		// here would silently register under a name the caller cannot
		// predict.
		if name == "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"the server name must not be empty; pass a real name, or omit the argument to derive it from --url's host")
		}
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
		return l.authenticatePlain(ctx, in, p, name, existing.URL, existing.CA, existing.CAFingerprint)
	}

	if cacert != "" {
		// Matches the --cacert-without-url matrix's absent-name row: body
		// guidance only, no machine hint (neither value is known here).
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--cacert without --url and without a name has no entry to attach trust to; pass a name to change an existing entry's trust with rancher config update-server")
	}

	cur, srv, cerr := cfg0.CurrentServerEntry()
	if cerr == nil {
		return l.authenticatePlain(ctx, in, p, cur, srv.URL, srv.CA, srv.CAFingerprint)
	}

	names := sortedKeys(cfg0.Servers)
	switch len(names) {
	case 0:
		return runtime.Result{}, clierr.New(clierr.CodeNotLoggedIn,
			"no servers configured; add one with: rancher auth login --url <url>")
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

// nameCollisionError is the refusal for a registration name already taken
// by an entry at a different URL. A derived name gets no hint: the name was
// chosen by this command, not the caller, so a suggested update-server
// command would re-point an entry the caller never named, and no single
// alternate name can be suggested with certainty — the remediation lives in
// the message body instead.
func nameCollisionError(name, existingURL, canonical string, derived bool) *clierr.Error {
	if derived {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("the name %s, derived from the URL's host, is already registered at %s; pass an explicit name to add this server as a separate entry",
				shquote(name), existingURL))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("server %s is registered at %s", name, existingURL)).
		WithHint(fmt.Sprintf("rancher config update-server %s --url %s", shquote(name), shquote(canonical)))
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
	// canonical is resolved once and reused to derive an absent name from
	// the URL's host, to detect a collision with an existing entry under
	// that name below, and as prepareRegistration's input — so a --url
	// login parses the URL exactly once, and an invalid or
	// unsupported-scheme URL is refused before any other work.
	canonical, canonErr := canonicalizeServerURL(url)
	if canonErr != nil {
		return runtime.Result{}, canonErr
	}

	derived := false
	if name == "" {
		// canonical already round-tripped through url.Parse inside
		// CanonicalServerURL, so this reparse cannot fail.
		u, _ := neturl.Parse(canonical)
		name = u.Hostname()
		derived = true
		// An IPv6 literal survives Hostname() with its colons — only the
		// brackets are stripped — and a colon-bearing entry name reads as
		// a URL fragment everywhere a name is printed or quoted, so
		// derivation refuses it instead of coining one.
		if strings.Contains(name, ":") {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"a server name cannot be derived from an IPv6 address; pass an explicit name: rancher auth login <name> --url <url>")
		}
		// With colons refused above, a derived name cannot land in the
		// reserved legacy: namespace; the check stays so a future
		// derivation change cannot start producing reserved names
		// unchecked.
		if ce := validateEntryName(name); ce != nil {
			return runtime.Result{}, ce
		}
	}

	existing, known := cfg0.Servers[name]
	// A known name whose canonicalized URL disagrees is a different server
	// hiding behind a trusted name — refuse before any network call, rather
	// than let a hijacking or a typo silently re-point the entry (or dial
	// the wrong host first and only then fail deep inside registration).
	// existing.URL is compared as stored: registration only ever stores it
	// canonical.
	if known && canonical != existing.URL {
		return runtime.Result{}, nameCollisionError(name, existing.URL, canonical, derived)
	}

	// Known name, --cacert absent: prepare with the EXISTING entry's
	// effective selector — the flag's absence is never treated as a switch
	// to system roots (that would silently strip a private CA on a plain
	// re-login-with-add spelling).
	caSelector := cacert
	if cacert == "" && known {
		caSelector = existing.CA
	}

	probe, err := prepareRegistration(ctx, l.tlsPreflight, canonical, caSelector)
	switch {
	case err != nil && !known && cacert == "":
		// Guided trust on the delegated path applies only to a genuinely new
		// entry with no explicit --cacert: an already-registered entry's
		// rotated CA is a plain-login concern (authenticateAndStore runs the
		// same offerGuidedTrust against the STORED selector, with the
		// combined token+CA commit protocol that entry's re-prompt needs),
		// and an explicit --cacert failure is never eligible for guided
		// replacement — the user made an explicit trust statement. A new
		// entry has no prior recorded identity, so there is never a
		// changed-identity warning to show here.
		accepted, version, _, gerr := offerGuidedTrust(ctx, err, p, l.tlsPreflight, canonical, "")
		if accepted == nil {
			err = gerr
			break
		}
		// offerGuidedTrust already proved the candidate re-verifies; building
		// the probe directly (rather than calling prepareRegistration again
		// with accepted.pem) skips a second, redundant network round trip
		// against the exact same candidate. caBytes equals caSelector here
		// because an inline PEM selector resolves to itself unchanged
		// (resolveRegistrationCA/config.ResolveCA).
		warning := config.UnknownServerVersionWarning
		if version != "" {
			_, warning = config.EvalMinVersionSupport(version)
		}
		probe = registrationProbe{
			canonicalURL:   canonical,
			caSelector:     accepted.pem,
			caBytes:        accepted.pem,
			versionWarning: warning,
			caFingerprint:  accepted.fingerprint,
		}
		err = nil
	case err != nil && known && cacert == "":
		// A known entry's rotated CA is a plain-login concern (see above);
		// point there instead of leaving the operator retrying the --url
		// spelling, which can never itself re-establish trust for an
		// existing entry.
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeTLSTrust {
			err = ce.WithMessage(ce.Message +
				fmt.Sprintf("; %s is already registered — sign in to it directly: rancher auth login %s", shquote(name), shquote(name))).
				WithHint(config.LoginHintFor(name))
		}
	}
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
func (l loginCmd) authenticatePlain(ctx context.Context, in runtime.Input, p runtime.Prompter, name, serverURL, caSelector, caFingerprint string) (runtime.Result, error) {
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, "", "", caFingerprint, false, false)
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
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, caBytes, versionWarning, "", true, added)
}

// authenticateAndStore runs the auth flow against (name, serverURL,
// caSelector) and, on success, commits the credential under a fresh
// config.WithLock acquisition. Only reached through authenticatePlain/
// authenticateDelegated, which each fix delegated to the right constant
// for their caller — no call site passes a bare bool literal.
func (l loginCmd) authenticateAndStore(
	ctx context.Context, in runtime.Input, p runtime.Prompter,
	name, serverURL, caSelector, caBytes, versionWarning, caFingerprint string,
	delegated, added bool,
) (runtime.Result, error) {
	isHTTPS := strings.HasPrefix(serverURL, "https://")
	var pendingTrust *trustCandidate
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
				accepted, _, declined, gerr := offerGuidedTrust(ctx, perr, p, l.tlsPreflight, serverURL, caFingerprint)
				if accepted == nil {
					if declined {
						return runtime.Result{}, pinnedTrustFailure(gerr, name)
					}
					// A consent was given but the candidate itself then
					// failed to re-verify: gerr already describes THAT
					// candidate, never the entry's still-failing stored
					// trust — pinnedTrustFailure's "pinned trust no longer
					// matches" framing would misdescribe it.
					return runtime.Result{}, gerr
				}
				pendingTrust = accepted
			}
		}
	}

	// snap binds the commit below to the entry state THIS preflight verified
	// against — caSelector/caBytes here are always the pre-consent, stored
	// values (a guided-trust replacement never appears in the snapshot), so
	// a concurrent writer that changed the entry between this preflight and
	// the commit is still caught as drift, exactly as an unmodified plain
	// login already is.
	snap := authSnapshot{name: name, canonicalURL: serverURL, caSelector: caSelector, caBytes: caBytes}
	if pendingTrust != nil {
		// The rest of THIS operation (token acquisition, verify) authenticates
		// against the just-accepted candidate, never the still-failing stored
		// CA.
		caBytes = pendingTrust.pem
	}

	token, viaProvider, authErr := resolveLoginToken(ctx, serverURL, caBytes, l.idpClient, in, p)
	if authErr == nil {
		id, secret, ok := strings.Cut(token, ":")
		if !ok || id == "" || secret == "" {
			authErr = clierr.New(clierr.CodeValidation,
				"invalid token (expected non-empty id:secret); get one from the Rancher UI")
		}
	}
	if authErr != nil {
		if delegated {
			return runtime.Result{}, decorateDelegatedAuthFailure(ctx, l.dir, authErr, snap)
		}
		return runtime.Result{}, authErr
	}

	// A provider-issued token is authenticated by construction — the server
	// just issued it during the sign-in dance — so a second round-trip here
	// would be pure overhead, never a real check. --no-verify skips the
	// round-trip for a pasted/piped token instead; the success text below
	// says so.
	noVerify := in.Bool("no-verify")
	if !viaProvider && !noVerify && l.verify != nil {
		if _, verr := l.verify(ctx, config.Effective{URL: serverURL, CA: caBytes, Token: token, ServerName: name}); verr != nil {
			if delegated {
				return runtime.Result{}, decorateDelegatedVerifyFailure(ctx, l.dir, verr, snap)
			}
			if ce, ok := errors.AsType[*clierr.Error](verr); ok {
				return runtime.Result{}, ce.WithMessage(ce.Message + "; re-run with --no-verify to store the token without checking it").
					WithHint("")
			}
			return runtime.Result{}, verr
		}
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
		ref := config.ServerRef{Name: name, URL: serverURL}
		compensate, tokenErr := config.CommitToken(store, ref, token)
		tokenCommitted := tokenErr == nil
		if tokenErr != nil {
			if !errors.Is(tokenErr, config.ErrTokenStoreUnsupported) {
				return tokenErr
			}
			warnings = append(warnings,
				"credential helper is read-only; token not persisted — future commands will re-acquire it through the helper")
		}
		if pendingTrust != nil {
			// The token write (or its declared ErrTokenStoreUnsupported
			// exception) already ran; the CA/fingerprint write is the second
			// half of the same commit. A failure here, after a real token
			// commit, is compensated when the backend can — never a silent
			// orphan.
			cfg.Servers[name] = config.Server{URL: serverURL, CA: pendingTrust.pem, CAFingerprint: pendingTrust.fingerprint}
			writeConfig := cfg.Write
			if trustCommitConfigWriteHookForTest != nil {
				writeConfig = trustCommitConfigWriteHookForTest
			}
			if werr := writeConfig(); werr != nil {
				compensated, compensateErr := false, ""
				if tokenCommitted && compensate != nil {
					if cerr := compensate(); cerr != nil {
						compensateErr = cerr.Error()
					} else {
						compensated = true
					}
				}
				return clierr.New(clierr.CodeGeneric,
					fmt.Sprintf("storing the accepted certificate for %s: %s", name, werr.Error())).
					WithDetails(clierr.TrustCommitFailureDetails{
						TokenCommitted:   tokenCommitted,
						Compensated:      compensated,
						CompensateError:  compensateErr,
						ConfigWriteError: werr.Error(),
					}).
					WithHint(config.LoginHintFor(name))
			}
		}
		currentServer = cfg.CurrentServer
		if currentServer != "" {
			_, currentValid = cfg.Servers[currentServer]
		}
		return nil
	}); err != nil {
		return runtime.Result{}, err
	}

	// verified mirrors the headline choice below exactly: false only when
	// the round-trip was explicitly skipped via --no-verify for a
	// pasted/piped token. A provider-issued token is authenticated by
	// construction, so it reads true even though no explicit round-trip ran.
	verified := !noVerify || viaProvider

	res := runtime.Result{Kind: "LoginResult", Data: map[string]any{
		"server": name, "url": serverURL, "currentServer": currentServer, "added": added, "verified": verified,
	}}
	res.Warnings = warnings
	var lines []string
	if added {
		lines = append(lines, "server "+name+" added")
	}
	// Login never switches the pointer — only registration's
	// bootstrap/repair rule does, and that never reaches THIS path (an
	// already-registered entry). Advise whenever the pointer doesn't name
	// what was just logged into, empty/dangling or a DIFFERENT valid
	// entry alike — don't silently leave the operator wondering why the
	// next command still operates against a different server.
	switch {
	case !currentValid:
		lines = append(lines, fmt.Sprintf("the current server is not set — switch to it with: rancher config use-server %s", shquote(name)))
	case currentServer != name:
		lines = append(lines, fmt.Sprintf("the current server remains %s — switch to %s with: rancher config use-server %s",
			shquote(currentServer), shquote(name), shquote(name)))
	}
	headline := fmt.Sprintf("logged in to %s (%s)", name, serverURL)
	if !verified {
		headline = fmt.Sprintf("token stored for %s (%s) — not verified", name, serverURL)
	}
	tr := runtime.TextLines(headline, lines...)
	if !verified {
		tr = runtime.AccentLine(tr, headline)
	}
	res.Text, res.StyledText = tr.Text, tr.StyledText
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
	return decorateDelegatedFailure(ctx, dir, authErr, snap, "")
}

// decorateDelegatedVerifyFailure is decorateDelegatedAuthFailure for the
// verify round-trip specifically: an orphaned registration is the bigger
// problem, so the undo-the-registration guidance still owns the message
// body and the hint, but --no-verify is a real way out of a verify failure
// (unlike a plain auth failure, where it would just fail again the same
// way), so it is folded into the body as a secondary option instead of
// fighting the undo guidance for the one hint slot.
func decorateDelegatedVerifyFailure(ctx context.Context, dir string, verifyErr error, snap authSnapshot) error {
	return decorateDelegatedFailure(ctx, dir, verifyErr, snap, ", or bypass verification with --no-verify")
}

// decorateDelegatedFailure is the shared body behind decorateDelegatedAuthFailure
// and decorateDelegatedVerifyFailure: it rechecks CURRENT state before
// deciding whether the retry hint is still the undo-the-registration command
// (entry unchanged) or has to fall back to a plain "check it" pointer (entry
// changed or vanished since registration), then appends extra — a caller's
// own secondary-option sentence, or "" when there is none — to the message
// body either way. A failure that is not a structured error, or a context
// already cancelled, skips the recheck and returns failure unchanged.
func decorateDelegatedFailure(ctx context.Context, dir string, failure error, snap authSnapshot, extra string) error {
	ce, ok := errors.AsType[*clierr.Error](failure)
	if !ok || ctx.Err() != nil {
		// Not a structured error, or cancellation preempts the recheck:
		// preserve the original error verbatim, no hint.
		return failure
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
			fmt.Sprintf(" (server entry %q may have changed since registration; check it with: rancher config list-servers)", snap.name) + extra).
			WithHint("")
	}
	return ce.WithMessage(ce.Message +
		fmt.Sprintf("; undo the registration with: rancher config delete-server %s", shquote(snap.name)) + extra).
		WithHint(config.LoginHintFor(snap.name))
}

// resolveLoginToken returns the resolved token and whether it came from the
// provider/OAuth sign-in flow (viaProvider) — true only when
// resolveProviderLoginToken produced it, since a server-issued token is
// authenticated by construction and must never take a second verification
// round-trip.
func resolveLoginToken(ctx context.Context, server, caPEM string, newIdPClient func() *http.Client, in runtime.Input, p runtime.Prompter) (string, bool, error) {
	if in.Bool("token-stdin") {
		if in.String("auth-provider") != "" || in.String("auth-flow") != "" || in.String("ttl") != "" {
			return "", false, clierr.New(clierr.CodeValidation,
				"--auth-provider/--auth-flow/--ttl apply to the provider sign-in flow and are incompatible with --token-stdin; "+
					"drop --token-stdin to use the provider flow, or drop the provider flags to paste/pipe a token")
		}
	}
	if in.Bool("no-verify") && (in.String("auth-provider") != "" || in.String("auth-flow") != "") {
		return "", false, clierr.New(clierr.CodeValidation,
			"--no-verify has no effect on the provider sign-in flow, which is already authenticated by construction; "+
				"drop --no-verify, or drop the provider flags to skip verification for a pasted token")
	}

	var (
		tok         string
		viaProvider bool
		err         error
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
		tok, viaProvider, err = promptForToken(ctx, server, caPEM, newIdPClient, in, p)
	default:
		return "", false, clierr.New(clierr.CodeValidation, "token required; pipe it via --token-stdin, or run in a terminal")
	}
	if err != nil {
		// A typed clierr (from the provider flow, or an invalid menu pick) is
		// already the right error to surface — the io.EOF/generic mapping
		// below is for the plain-paste read paths only.
		if _, ok := errors.AsType[*clierr.Error](err); ok {
			return "", false, err
		}
		if errors.Is(err, io.EOF) {
			return "", false, clierr.New(clierr.CodeValidation, "no token provided; pipe it via --token-stdin, or enter it at the prompt")
		}
		return "", false, clierr.New(clierr.CodeGeneric, "reading token: "+err.Error())
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", false, clierr.New(clierr.CodeValidation, "no token provided; pipe it via --token-stdin, or enter it at the prompt")
	}
	return tok, viaProvider, nil
}

// promptForToken offers the paste-or-provider choice (EXT-R-8): --auth-provider
// or --auth-flow already signal provider intent and skip straight to it,
// mirroring get-token's own explicit-flag shortcut past its provider menu.
// The bool return is viaProvider, true exactly when resolveProviderLoginToken
// produced the token.
// newIdPClient is a constructor, not a client: the identity-provider client
// is built only on the provider branches, so a pasted or piped token never
// pays for a transport it will not use.
func promptForToken(ctx context.Context, server, caPEM string, newIdPClient func() *http.Client, in runtime.Input, p runtime.Prompter) (string, bool, error) {
	if in.String("auth-provider") != "" || in.String("auth-flow") != "" {
		tok, err := resolveProviderLoginToken(ctx, server, caPEM, newIdPClient(), in, p)
		return tok, true, err
	}
	p.Notice("1) paste an API token\n2) sign in via an identity provider")
	choice, err := p.Prompt(ctx, "Select [1]: ")
	if err != nil {
		return "", false, err
	}
	switch strings.TrimSpace(choice) {
	case "", "1":
		tok, err := p.PromptSecret(ctx, "Enter token (id:secret): ")
		return tok, false, err
	case "2":
		tok, err := resolveProviderLoginToken(ctx, server, caPEM, newIdPClient(), in, p)
		return tok, true, err
	default:
		return "", false, clierr.New(clierr.CodeAmbiguousSelection, "select 1 (paste) or 2 (identity provider); re-run and answer 1 or 2")
	}
}
