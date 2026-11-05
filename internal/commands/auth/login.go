package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

type loginCmd struct {
	dir string
	// newStore opens the store the credential commits to, from the BACKEND
	// RESOLVED UNDER THE LOCK - a physical value, never a policy.
	// Production wiring uses config.OpenResolvedStore; tests inject a
	// factory returning their fake store. The string return carries any
	// store notice so Execute can route it into res.Warnings rather than
	// writing directly to stderr. Replaces the removed store field (the
	// dependency-resolution store snapshot this race was about).
	newStore func(backend, dir string) (config.CredStore, string, error)
	// tlsPreflight verifies the server's certificate against caPEM (or
	// system roots when caPEM is empty) before any credential is persisted.
	// Plain login (an existing entry) still runs this: it verifies against
	// the entry's saved CA, it just cannot change it - a certificate that
	// the CA no longer verifies fails TLS_TRUST with the remediation in the
	// message body, no machine hint (the new CA path is unknown). Delegated
	// login (--url) never calls this directly: register.Prepare already ran
	// it as part of establishing trust for the entry. nil is a valid,
	// deliberately inert value: tests that do not exercise TLS behavior
	// leave it unset.
	tlsPreflight register.TLSPreflightFunc
	// verify performs an authenticated self-read against the resolved token
	// before it is persisted, and returns the token's owner. Follows
	// tlsPreflight's own nil-skip idiom: production wiring sets it to
	// provider.Owner; a test-constructed
	// struct that leaves it nil never dials out for this check. Skipped
	// unconditionally for a provider-issued token (already authenticated by
	// construction) and whenever --no-verify is set.
	verify loginVerifyFunc
	// newIdPClient builds the client the provider flow dials the identity
	// provider with. Production wiring returns httpx.NewExternal (operating
	// system roots only); tests inject a client pinned to their IdP fixture.
	// nil means the production default.
	newIdPClient func() *http.Client
	// beforeCommitHook runs inside the config lock, before the commit reads
	// the current entry state, so a test can make a concurrent change land
	// exactly in the window the snapshot check guards. nil in production.
	beforeCommitHook func()
}

// idpClient resolves newIdPClient with its production default.
func (l loginCmd) idpClient() *http.Client {
	if l.newIdPClient != nil {
		return l.newIdPClient()
	}
	return httpx.NewExternal(httpx.DefaultTimeout)
}

// NewLogin builds the `auth login` command (local). The store is resolved
// inside Execute from the config loaded under the config write lock - never
// from the dependency-resolution snapshot, which can predate a concurrent
// policy change.
func NewLogin(dir string) runtime.Command {
	return &loginCmd{dir: dir, newStore: config.OpenResolvedStore, tlsPreflight: register.TLSPreflight, verify: provider.Owner}
}

// loginVerifyFunc matches provider.Owner's shape: an authenticated
// self-read against a resolved (URL, CA, token) triple that returns the
// token's owner.
type loginVerifyFunc func(ctx context.Context, eff config.Effective) (string, error)

// pinnedTrustFailure rewrites a TLS_TRUST error from plain login's preflight
// (an existing entry's saved CA no longer verifies the server, host being
// the server as the preflight named it). The preflight words its message
// for a command that takes --url and --cacert; a plain login takes
// neither, so the message is rebuilt with the wording of the login's own
// verify step: the server URL saved for the entry, a different CA, and
// config update-server for an unknown authority. The new CA path is
// unknown to the CLI, so no hint is emitted, per the executable-hint
// contract. Non-TLS_TRUST errors (including nil) pass through unchanged.
func pinnedTrustFailure(err error, name, host string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeTLSTrust {
		return err
	}
	msg := ce.Message
	if d, ok := ce.Details.(tlstrust.Details); ok {
		msg = tlstrust.Message(d, host, verifyTLSRemedy(false, false, name))
	}
	return ce.WithMessage(msg).WithHint("")
}

// loginVariableRemedy is the remedy an unknown-authority failure names
// when RANCHER_CLI_CACERT supplied the CA a login connected with: the CA
// came from the variable, so the variable is what to change. The --url
// path may be creating the entry, so it can pass the CA to the login; the
// plain path can only point at the entry's saved CA.
func loginVariableRemedy(urlPath bool, name string) string {
	if urlPath {
		return "the CA came from " + config.EnvCACert + " - pass --cacert <file> to this login, or unset " +
			config.EnvCACert + ", or set it to the server's CA"
	}
	return "the CA came from " + config.EnvCACert + " - unset it, or set it to the server's CA; to save a CA for this entry, run: " +
		clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --cacert <file>", name)
}

// loginSaveCARemedy is the remedy of an unknown-authority failure of a
// plain login's verify step: the CA is the one the entry saves, so the
// entry is where to change it.
func loginSaveCARemedy(name string) string {
	return "save it for this entry with: " + clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --cacert <file>", name)
}

// verifyTLSRemedy words a certificate failure of the verify step. The
// --url path takes its CA from a flag, a prompt or the variable and may be
// creating the entry; the plain path signs in to an existing entry, whose
// saved URL and CA are what to change.
func verifyTLSRemedy(delegated, viaVariable bool, name string) tlstrust.Remedy {
	if delegated {
		if viaVariable {
			return tlstrust.Remedy{CA: "; " + loginVariableRemedy(true, name)}
		}
		return tlstrust.Remedy{}
	}
	r := tlstrust.Remedy{URL: "the server URL saved for entry " + clierr.ShellQuote(name), Fix: "a different CA"}
	if viaVariable {
		r.CA = "; " + loginVariableRemedy(false, name)
	} else {
		r.CA = "; " + loginSaveCARemedy(name)
	}
	return r
}

// variableTrustFailure rewrites an unknown-authority TLS_TRUST error of a
// login that connected with RANCHER_CLI_CACERT: the error names the
// variable instead of --cacert as the CA to change. The remedy goes into
// the message the same way tlstrust.Classify builds its own, and the error
// carries no hint. Any other error, and every kind a CA cannot fix, passes
// through unchanged.
func variableTrustFailure(err error, remedy string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeTLSTrust {
		return err
	}
	d, ok := ce.Details.(tlstrust.Details)
	if !ok || d.Kind != tlstrust.KindUnknownAuthority {
		return err
	}
	return ce.WithMessage(tlstrust.UnknownAuthorityMessage(d.IsLeafCertificate(), "; "+remedy)).WithHint("")
}

// connectionCA is the CA a login connects with when it is not the CA the
// entry saves. On the plain path variable is the selector of
// RANCHER_CLI_CACERT where it applies to the entry's URL, resolved there
// after the saved CA. On the --url path the registration already resolved
// it: bytes is what the preflight connected with and fromEnvironment says
// the variable supplied it.
type connectionCA struct {
	variable        string
	bytes           string
	fromEnvironment bool
}

// variableNotice is the warning of a login that connected with
// RANCHER_CLI_CACERT and leaves an entry whose saved CA is not that CA:
// the variable is never saved, so a command run without it connects with
// the saved CA, or with none.
func variableNotice(name, savedSelector string) string {
	q := clierr.ShellQuote(name)
	save := clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --cacert <file>", name)
	if savedSelector == "" {
		return "server entry " + q + " has no saved CA and this login used " + config.EnvCACert +
			"; commands run without that variable will not trust the server (logout uses the variable for this entry); " +
			"to save the CA, run: " + save
	}
	return "server entry " + q + " keeps its saved CA, which differs from " + config.EnvCACert +
		" that this login used; commands run without that variable, and logout, use the saved CA, and fail if it does not fit the server; " +
		"to replace it, run: " + save
}

// replacedUserWarning is the warning of a login that leaves a server entry
// logged in as another user than the one its stored login recorded: the
// two user ids, in a list of one. Nothing is said for an entry that had no
// stored login, for the same user, or when either id is unknown.
func replacedUserWarning(name, was, now string) []string {
	if was == "" || now == "" || was == now {
		return nil
	}
	return []string{"server entry " + name + " is now logged in as user " + now + "; it was logged in as user " + was}
}

// replacedAuthConfigWarning is the warning of a login that leaves a server
// entry logged in through another auth config than the one its stored login
// recorded: the two auth config names, in a list of one. Nothing is said for
// an entry that had no stored login, for the same auth config, or when
// either is unknown (a pasted token records none). A login that changes the
// user too gets both warnings, one line each.
func replacedAuthConfigWarning(name, was, now string) []string {
	if was == "" || now == "" || was == now {
		return nil
	}
	return []string{"server entry " + name + " is now logged in through auth config " + now + "; it was logged in through auth config " + was}
}

func (loginCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:         "auth login",
		Usage:        "Login to a Rancher server",
		CommandType:  runtime.Local,
		ServerAccess: runtime.ServerAccessWrite,
		Args:         []runtime.ArgSpec{{Name: "name", Type: "string", Desc: "server entry `name`"}},
		Flags: []runtime.ArgSpec{
			{Name: "url", Type: "string", Desc: "Rancher server `url` to add and log into; when name is omitted, the server's hostname becomes the entry name"},
			{Name: "token-stdin", Type: "bool", Desc: "read the API token (id:secret) from stdin"},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM, saved with the entry (requires --url); when absent, RANCHER_CLI_CACERT connects the login with or without --url, and is never saved", Env: []string{config.EnvCACert}},
			{Name: "auth-provider", Type: "string", Desc: "auth config `name` to sign in with (for example corp-ldap); a provider name or type such as openldap also works; with several matches the CLI asks, or lists them in the error"},
			{Name: "auth-flow", Type: "string", Enum: token.AuthFlowValues(), Desc: "identity-provider `flow`", Env: []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow}},
			{Name: "ttl", Type: "string", Desc: "session-token lifetime `duration`, e.g. 90d"},
			{Name: "no-verify", Type: "bool", Desc: "store the token without an authenticated round-trip against the server"},
			{Name: "credential-store", Type: "string", Desc: "credential `store` this directory keeps its credentials in: auto, keychain, file or exec:<name>; a change of store takes only this entry's credential and needs the other entries logged out of the store being left first"},
		},
		Examples: []string{
			"rancher auth login prod",
			"rancher auth login prod --url https://rancher.ex --token-stdin < token.txt",
		},
		Shape:       runtime.ShapeText,
		SecretInput: true,
	}
}

// CompleteArgs implements runtime.ShellCompleter: `auth login <TAB>`
// completes server names from the already-loaded config snapshot.
func (loginCmd) CompleteArgs(_ context.Context, view runtime.ConfigView, _ []string) []string {
	return view.ServerNames()
}

func (l loginCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	name := ""
	if a := in.Args(); len(a) > 0 {
		name = a[0]
		// An explicitly passed empty string is a caller bug (usually an
		// unset shell variable), never a request to derive - deriving
		// here would silently register under a name the caller cannot
		// predict.
		if name == "" {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				"the server name must not be empty; pass a real name, or omit the argument to derive it from --url's host")
		}
	}
	url := in.String("url")
	cacert := in.String("cacert")

	// Names in the reserved namespaces - the legacy token shim's cache
	// slots (legacy:), the exec plugin's cache slots of calls with
	// --user-id (user-id:) and its own login entries (exec-login-) - are
	// refused unconditionally: none can be a real entry, and add-server
	// refuses them the same way at creation.
	if name != "" {
		if ce := config.ValidateEntryName(name); ce != nil {
			return runtime.Result{}, ce
		}
	}

	cfg0, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, "reading config: "+err.Error())
	}

	// A store this release does not know, or one the variable disagrees
	// with, is refused before anything is registered or written: it is an
	// answer about the input alone. A refusal that depends on the
	// directory's state - its other entries, an unfinished change, the
	// keychain probe - comes later, after a `--url` login has registered
	// its entry, and leaves that entry behind.
	if _, _, perr := config.LoginPolicy(in.String("credential-store"), cfg0.CredentialStore); perr != nil {
		return runtime.Result{}, perr
	}

	target, err := resolveLoginTarget(cfg0, name, url, cacert)
	if err != nil {
		return runtime.Result{}, err
	}
	if url != "" {
		return l.executeRegisterAndLogin(ctx, in, p, target, cacert)
	}
	return l.authenticatePlain(ctx, in, p, target.name, target.server, target.caSelector, target.caFingerprint)
}

// nameCollisionError is the refusal for a registration name already taken
// by an entry at a different URL. A derived name gets no hint: the name was
// chosen by this command, not the caller, so a suggested update-server
// command would re-point an entry the caller never named, and no single
// alternate name can be suggested with certainty - the remediation lives in
// the message body instead. existingURL is the entry's stored URL; a URL that
// cannot be used is never shown, and the refusal says so instead.
func nameCollisionError(name, existingURL, canonical string, derived bool) *clierr.Error {
	update := clierr.Rancher + " config update-server" + clierr.ShellQuoteArgs(" --url "+clierr.ShellQuote(canonical), name)
	if problem := config.CheckServerURL(existingURL); problem != nil {
		if derived {
			return clierr.New(clierr.CodeValidation, fmt.Sprintf(
				"the server URL in server entry %s cannot be used: %s; the name was derived from the host of --url; change that entry's URL before a login, or pass an explicit name to add this server as a separate entry",
				clierr.ShellQuote(name), problem))
		}
		return clierr.New(clierr.CodeValidation, config.EntryOrigin(name).Text(problem)).WithHint(update)
	}
	if derived {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("the name %s, derived from the host of --url, is already registered at %s; pass an explicit name to add this server as a separate entry",
				clierr.ShellQuote(name), config.DisplayServerURL(existingURL)))
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("server %s is registered at %s", name, config.DisplayServerURL(existingURL))).
		WithHint(update)
}

// configUseServerHint returns the bare, shell-quoted `rancher config
// use-server <name>` command - the pointer-switch hint every no-pointer
// login outcome below emits for a concrete name.
func configUseServerHint(name string) string {
	return clierr.Rancher + " config use-server" + clierr.ShellQuoteArgs("", name)
}

// loginUnknownNameError builds NOT_FOUND for an explicit name absent from
// cfg's servers: candidates for a typo, plus body guidance toward the
// registering spelling. No machine hint - the URL isn't known here.
func loginUnknownNameError(cfg *config.Config, name string) *clierr.Error {
	ce := register.ServerNotFound(cfg, name)
	return ce.WithMessage(ce.Message + "; re-run with --url <url> to add it")
}

// executeRegisterAndLogin is the `--url` path: register (add-if-missing,
// same shared commit seam add-server uses) THEN authenticate against the
// persisted entry - registration completes, atomic, before any auth flow
// starts. A failed auth after this leaves a valid registered entry, a
// first-class state, not a bug to roll back.
func (l loginCmd) executeRegisterAndLogin(ctx context.Context, in runtime.Input, p runtime.Prompter, target loginTarget, cacert string) (runtime.Result, error) {
	name, canonical, known := target.name, target.server, target.known
	caSelector, variableCA := target.caSelector, target.variable
	viaVariable := variableCA != "" && strings.HasPrefix(canonical, "https://")

	probe, err := register.PrepareConnecting(ctx, l.tlsPreflight, canonical, caSelector, variableCA)
	switch {
	case err != nil && viaVariable:
		// The "is already registered; sign in to it directly" pointer is
		// dropped for every kind of failure here, a known entry included:
		// it points at the plain login, which connects with the same
		// variable and fails the same way.
		err = variableTrustFailure(err, loginVariableRemedy(true, name))
	case err != nil && !known && cacert == "":
		// Guided trust on the delegated path applies only to a genuinely new
		// entry with no explicit --cacert: an already-registered entry's
		// rotated CA is a plain-login concern (authenticateAndStore runs the
		// same offerGuidedTrust against the STORED selector, with the
		// combined token+CA commit protocol that entry's re-prompt needs),
		// and an explicit --cacert failure is never eligible for guided
		// replacement - the user made an explicit trust statement. A new
		// entry has no prior recorded identity, so there is never a
		// changed-identity warning to show here.
		accepted, version, _, gerr := offerGuidedTrust(ctx, err, p, l.tlsPreflight, canonical, "")
		if accepted == nil {
			err = gerr
			break
		}
		// offerGuidedTrust already proved the candidate re-verifies; building
		// the probe directly (rather than calling register.Prepare again
		// with accepted.pem) skips a second, redundant network round trip
		// against the exact same candidate. caBytes equals caSelector here
		// because an inline PEM selector resolves to itself unchanged
		// (register.ResolveCA/config.ResolveCA).
		warning := config.UnknownServerVersionWarning
		if version != "" {
			_, warning = config.EvalMinVersionSupport(version)
		}
		probe = register.Probe{
			CanonicalURL:    canonical,
			CASelector:      accepted.pem,
			CABytes:         accepted.pem,
			ConnectionBytes: accepted.pem,
			VersionWarning:  warning,
			CAFingerprint:   accepted.fingerprint,
		}
		err = nil
	case err != nil && known && cacert == "":
		// A known entry's rotated CA is a plain-login concern (see above);
		// point there instead of leaving the operator retrying the --url
		// spelling, which can never itself re-establish trust for an
		// existing entry.
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeTLSTrust {
			err = ce.WithMessage(ce.Message +
				fmt.Sprintf("; %s is already registered; sign in to it directly: %s", clierr.ShellQuote(name), config.LoginCommand+clierr.ShellQuoteArgs("", name))).
				WithHint(config.LoginHintFor(name))
		}
	}
	if err != nil {
		return runtime.Result{}, err
	}

	out, err := register.CommitCreateUnderLock(ctx, l.dir, name, probe)
	if err != nil {
		return runtime.Result{}, err
	}

	var early []string
	if probe.VersionWarning != "" {
		early = append(early, probe.VersionWarning)
	}
	early = append(early, out.Notices...)
	return l.authenticateDelegated(ctx, in, p, out.Name, out.CanonicalURL, out.CASelector, probe.CABytes,
		connectionCA{bytes: probe.ConnectionBytes, fromEnvironment: probe.FromEnvironment}, early, out.Created)
}

// authenticatePlain authenticates an already-registered entry: no
// registration ran for this call, so it re-verifies the pinned-trust
// preflight itself, and an auth failure is final - there is no unlocked
// registration window behind it to recheck for staleness.
func (l loginCmd) authenticatePlain(ctx context.Context, in runtime.Input, p runtime.Prompter, name, serverURL, caSelector, caFingerprint string) (runtime.Result, error) {
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, "",
		connectionCA{variable: config.EnvCACertFor(serverURL)}, nil, caFingerprint, false, false)
}

// authenticateDelegated authenticates immediately after this login's own
// delegated registration committed: caBytes (the saved selector's bytes),
// conn and the warnings (the probe's
// version warning, and any credential the registration removed from under
// the new name) are already known from that registration (re-running the
// preflight would repeat the same network round trip, and dropping the
// warnings here would silently lose them - registration produced them,
// login must still surface them), and a failure gets the stale-hint recheck scoped to the
// just-closed unlocked registration window. added is threaded straight
// into LoginResult.
func (l loginCmd) authenticateDelegated(ctx context.Context, in runtime.Input, p runtime.Prompter, name, serverURL, caSelector, caBytes string, conn connectionCA, warnings []string, added bool) (runtime.Result, error) {
	return l.authenticateAndStore(ctx, in, p, name, serverURL, caSelector, caBytes, conn, warnings, "", true, added)
}

// authenticateAndStore runs the auth flow against (name, serverURL,
// caSelector) and, on success, commits the credential under a fresh
// config.WithLock acquisition. Only reached through authenticatePlain/
// authenticateDelegated, which each fix delegated to the right constant
// for their caller - no call site passes a bare bool literal.
func (l loginCmd) authenticateAndStore(
	ctx context.Context, in runtime.Input, p runtime.Prompter,
	name, serverURL, caSelector, caBytes string, conn connectionCA, earlyWarnings []string, caFingerprint string,
	delegated, added bool,
) (runtime.Result, error) {
	// The credential store is resolved before the sign-in, not under the
	// commit lock: the declaration the commit writes has to be the one this
	// login resolved against, and a variable that disagrees with the
	// directory must stop the login before it asks anyone to authenticate.
	// Every refusal here keeps the early warnings: a registration that
	// removed a credential left under the new name has already done so.
	declCfg, err := config.Load(config.FilePath(l.dir))
	if err != nil {
		return runtime.Result{}, config.WithNotices(clierr.New(clierr.CodeGeneric, "reading config: "+err.Error()), earlyWarnings)
	}
	observedDeclaration := declCfg.CredentialStore
	requested := in.String("credential-store")
	policy, _, policyErr := config.LoginPolicy(requested, observedDeclaration)
	if policyErr != nil {
		return runtime.Result{}, config.WithNotices(policyErr, earlyWarnings)
	}
	// What this login would do to the directory's credential store,
	// answered before anyone is asked to authenticate: a change that
	// cannot be made, or a change record that cannot be read, costs no
	// server-issued token this way. Steps an earlier change left are
	// replayed under the lock, which then decides; they are not refused
	// here.
	if _, serr := config.LoginStoreChange(l.dir, declCfg, name,
		policy, config.DeclarationFor(observedDeclaration, requested, policy)); serr != nil {
		return runtime.Result{}, config.WithNotices(serr, earlyWarnings)
	}

	isHTTPS := strings.HasPrefix(serverURL, "https://")
	var pendingTrust *trustCandidate
	// caBytes is the saved selector's bytes, which the snapshot below binds
	// the commit to; connBytes is what every request of this login is
	// verified against: the same bytes, unless RANCHER_CLI_CACERT supplied
	// the CA.
	connBytes, viaVariable := conn.bytes, conn.fromEnvironment
	if !delegated {
		// register.ResolveCA is the one place "resolve CA bytes only
		// under https, and only when the selector names something" is
		// encoded - sharing it here keeps this gate from drifting against
		// register.Prepare's copy.
		b, raerr := register.ResolveCA(serverURL, caSelector)
		if raerr != nil {
			return runtime.Result{}, clierr.New(clierr.CodeValidation,
				fmt.Sprintf(
					"reading stored CA for server entry %q: %s; restore the file, or run: %s",
					name, config.CAFileProblem(raerr), config.DeleteServerCommand+clierr.ShellQuoteArgs("", name)))
		}
		caBytes, connBytes = b, b
		if conn.variable != "" {
			vb, verr := register.ResolveCA(serverURL, conn.variable)
			if verr != nil {
				return runtime.Result{}, clierr.New(clierr.CodeValidation, "reading "+config.EnvCACert+": "+config.CAFileProblem(verr))
			}
			connBytes, viaVariable = vb, vb != ""
		}
		if isHTTPS && l.tlsPreflight != nil {
			if _, perr := l.tlsPreflight(ctx, serverURL, connBytes); perr != nil {
				if viaVariable {
					// The failure is the variable's: no trust prompt, and no
					// statement about trust the entry pinned.
					return runtime.Result{}, variableTrustFailure(perr, loginVariableRemedy(false, name))
				}
				accepted, _, declined, gerr := offerGuidedTrust(ctx, perr, p, l.tlsPreflight, serverURL, caFingerprint)
				if accepted == nil {
					if declined {
						return runtime.Result{}, pinnedTrustFailure(gerr, name, register.ServerLabel(serverURL))
					}
					// A consent was given but the candidate itself then
					// failed to re-verify: gerr already describes THAT
					// candidate, never the entry's still-failing stored
					// trust - pinnedTrustFailure's message about the entry's
					// saved CA would misdescribe it.
					return runtime.Result{}, gerr
				}
				pendingTrust = accepted
			}
		}
	}
	if viaVariable && (caSelector == "" || caBytes != connBytes) {
		// The entry is saved either way, so the notice does not wait for the
		// preflight to reach the server.
		earlyWarnings = append(slices.Clone(earlyWarnings), variableNotice(name, caSelector))
	}

	// snap binds the commit below to the entry state THIS preflight verified
	// against - caSelector/caBytes here are always the pre-consent, stored
	// values (a guided-trust replacement never appears in the snapshot), so
	// a concurrent writer that changed the entry between this preflight and
	// the commit is still caught as drift, exactly as an unmodified plain
	// login already is.
	snap := register.AuthSnapshot{Name: name, CanonicalURL: serverURL, CASelector: caSelector, CABytes: caBytes}
	if pendingTrust != nil {
		// The rest of THIS operation (token acquisition, verify) authenticates
		// against the just-accepted candidate, never the still-failing stored
		// CA.
		connBytes = pendingTrust.pem
	}

	// st accumulates what each persistence system holds as the login
	// proceeds, plus every advisory learned so far, so any failure reports
	// the state reached and keeps the warnings that belong to steps that
	// did happen.
	st := token.LoginStates{Config: clierr.ConfigNotWritten}
	st.Warnings = append(st.Warnings, earlyWarnings...)
	cred, authErr := resolveLoginToken(ctx, serverURL, connBytes, l.idpClient, in, p)
	bearer := cred.Token
	viaProvider := cred.Provenance != token.ProvenanceUserSupplied
	if ae, ok := errors.AsType[*token.AcquireError](authErr); ok {
		authErr = token.FailAcquire(ae, name)
	}
	// carriesWarnings is set when authErr is built from st, whose warnings
	// already hold the early ones; any other failure of the sign-in has
	// none yet, and the early warnings (the variable notice, a removed
	// orphan credential) belong to steps that did happen.
	carriesWarnings := false
	if authErr == nil {
		id, secret, ok := strings.Cut(bearer, ":")
		if !ok || id == "" || secret == "" {
			// The credential this operation created is unusable: revoke it and
			// report it with the same shape every later failure carries.
			carriesWarnings = true
			authErr = token.FailLogin(clierr.New(clierr.CodeValidation,
				"invalid token (expected non-empty id:secret); get one from the Rancher UI"),
				clierr.StageCommit, st, cred, name)
		}
	}
	if authErr != nil {
		if !carriesWarnings {
			authErr = config.WithNotices(authErr, earlyWarnings)
		}
		// The remedy is a piped login for the entry, which names the entry's
		// own CA or none (never RANCHER_CLI_CACERT's), since the entry is
		// registered at this URL by now. A delegated login adds it only if
		// its recheck finds the entry as registered: for an entry that
		// changed since, no command is valid, and the decoration says to
		// check the entry.
		if delegated {
			remedy, _ := token.AuthLoginRemedyOf(authErr, declCfg, name, serverURL, caSelector)
			return runtime.Result{}, decorateDelegatedAuthFailure(ctx, l.dir, authErr, snap, remedy)
		}
		return runtime.Result{}, token.AuthLoginRemedy(authErr, declCfg, name, serverURL, caSelector)
	}

	// A provider-issued token is authenticated by construction - the server
	// just issued it during the sign-in dance - so a second round-trip here
	// would be pure overhead, never a real check. --no-verify skips the
	// round-trip for a pasted/piped token instead; the success text below
	// says so.
	noVerify := in.Bool("no-verify")
	pastedTokenChecked := false
	if !viaProvider && !noVerify && l.verify != nil {
		owner, verr := l.verify(ctx, config.Effective{
			URL: serverURL, CA: connBytes, Token: bearer, ServerName: name,
			TLSRemedy: verifyTLSRemedy(delegated, viaVariable, name),
		})
		if verr != nil {
			// A token for a server whose certificate fails verification helps
			// no later command, so a certificate failure never suggests
			// storing it unchecked.
			if ce, ok := errors.AsType[*clierr.Error](verr); ok && !delegated && ce.Code != clierr.CodeTLSTrust {
				verr = ce.WithMessage(ce.Message + "; re-run with --no-verify to store the token without checking it").
					WithHint("")
			}
			composed := token.FailLogin(verr, clierr.StageCommit, st, cred, name)
			if delegated {
				return runtime.Result{}, decorateDelegatedVerifyFailure(ctx, l.dir, composed, snap)
			}
			return runtime.Result{}, composed
		}
		// The request that proved the token also named its owner, which
		// the login records as a create response's owner is recorded.
		cred.UserID = owner
		pastedTokenChecked = true
	}

	var pendingCA *config.PendingTrust
	if pendingTrust != nil {
		pendingCA = &config.PendingTrust{CA: pendingTrust.pem, Fingerprint: pendingTrust.fingerprint}
	}
	out, perr := config.PersistLoginCredential(ctx, l.dir, config.PersistLoginInput{
		Name: name, CanonicalURL: serverURL, CAFromEnvironment: viaVariable,
		Provenance: cred.Provenance, TokenRef: cred.Ref, TokenUserID: cred.UserID, TokenAuthConfig: cred.AuthConfig,
		Bearer: bearer, ExpiresAt: cred.ExpiresAt,
		Policy: policy, ObservedDeclaration: observedDeclaration,
		Declaration:  config.DeclarationFor(observedDeclaration, requested, policy),
		OpenStore:    l.newStore,
		PendingTrust: pendingCA,
		PreCheck: func(cfg *config.Config) error {
			if l.beforeCommitHook != nil {
				l.beforeCommitHook()
				// The hook stands in for a writer that lands inside the lock
				// window, after the primitive read the config: the drift check
				// below must see what it wrote, so re-read it.
				fresh, err := config.Load(config.FilePath(l.dir))
				if err != nil {
					return err
				}
				cfg = fresh
			}
			if !register.SnapshotMatchesCurrent(cfg, snap) {
				return clierr.New(clierr.CodeConflict,
					fmt.Sprintf("server entry %q changed during login", name)).
					WithHint(config.LoginHintFor(name))
			}
			return nil
		},
	})
	st.Warnings = append(st.Warnings, out.Warnings...)
	st.Config, st.Commit, st.ConfigRecovery = out.ConfigState, out.Commit, out.ConfigRecovery
	st.Compensate, st.CompensationError = "", out.CompensationError
	if out.CredentialState == clierr.CredentialCompensated || out.CredentialState == clierr.CredentialCompensationFailed {
		st.Compensate = out.CredentialState
	}
	residual := []clierr.TokenFate{}
	persisted := perr == nil
	if persisted {
		st.Warnings = append(st.Warnings, replacedUserWarning(name, out.ReplacedUserID, cred.UserID)...)
		st.Warnings = append(st.Warnings, replacedAuthConfigWarning(name, out.ReplacedAuthConfig, cred.AuthConfig)...)
	}
	// After the run the CLI may ask the server it reached for its install
	// id with the token the store just took: a token the server issued in
	// this run, or a pasted token the verify call accepted. A pasted token
	// stored with --no-verify was never checked and is not sent; neither is a
	// token the store refused, because no stored login holds it. The request
	// is sent after the output, and asks again whatever failure the server's
	// facts file holds for an earlier token.
	if persisted && (viaProvider || pastedTokenChecked) {
		if base, fetch, err := provider.InstallIDFetchFor(serverURL, connBytes, bearer); err == nil {
			httpx.RegisterInstallIDFetcher(base, fetch, true)
		}
	}
	if perr != nil {
		if !errors.Is(perr, config.ErrTokenStoreUnsupported) {
			// A cancelled operation is reported as cancellation: the local
			// symptom (a lock timeout, a half-read config) describes the
			// cancellation, not a fault of its own.
			primary := perr
			if cerr := ctx.Err(); cerr != nil {
				primary = cerr
			}
			return runtime.Result{}, token.FailLogin(primary, clierr.StageCommit, st, cred, name)
		}
		residual = append(residual, token.FinishUnsupportedStore(cred, name, &st)...)
		if out.ConfigState == clierr.ConfigRollbackFailed {
			// A read-only store is warn-and-succeed only when the entry went
			// back to what it was. One that still names the token this
			// finalizer just revoked is a failure carrying its own recovery.
			return runtime.Result{}, token.FailLogin(perr, clierr.StageCommit, st, cred, name)
		}
	}
	currentServer, currentValid := out.CurrentServer, out.CurrentServerValid

	// verified mirrors the headline choice below exactly: false only when
	// the round-trip was explicitly skipped via --no-verify for a
	// pasted/piped token. A provider-issued token is authenticated by
	// construction, so it reads true even though no explicit round-trip ran.
	verified := !noVerify || viaProvider

	// A token this login created and could not revoke outlives the login
	// that succeeded: it is listed with how it ends.
	if persisted {
		f := token.SessionTokenFate(cred)
		if w, ok := token.LoginTokenWarning(f, func(f clierr.TokenFate) string {
			text, _ := token.ResidualRecovery(f, name, cred.Marker, true)
			return text
		}); ok {
			st.Warnings = append(st.Warnings, w)
			residual = append(residual, *f)
		}
	}

	var retired []clierr.TokenRef
	if len(out.SupersededTokens) > 0 {
		rep := token.RetireSuperseded(ctx, l.dir, name, serverURL, cred, out.SupersededTokens)
		retired = rep.Retired
		st.Warnings = append(st.Warnings, rep.Warnings...)
	}

	res := runtime.Result{Kind: "LoginResult", Data: map[string]any{
		"server": name, "url": config.DisplayServerURL(serverURL), "currentServer": currentServer, "added": added, "verified": verified,
		"residualTokens": residual, "provenance": string(cred.Provenance), "tokenUserId": cred.UserID,
		"tokenAuthConfig": cred.AuthConfig, "retiredTokens": retired,
	}}
	res.Warnings = st.Warnings
	var lines []string
	if added {
		lines = append(lines, "server "+name+" added")
	}
	// Login never switches the pointer - only registration's
	// bootstrap/repair rule does, and that never reaches THIS path (an
	// already-registered entry). Advise whenever the pointer doesn't name
	// what was just logged into, empty/dangling or a DIFFERENT valid
	// entry alike - don't silently leave the operator wondering why the
	// next command still operates against a different server.
	switch {
	case !currentValid:
		lines = append(lines, "the current server is not set; switch to it with: "+configUseServerHint(name))
	case currentServer != name:
		lines = append(lines, fmt.Sprintf("the current server remains %s; switch to %s with: %s",
			clierr.ShellQuote(currentServer), clierr.ShellQuote(name), configUseServerHint(name)))
	}
	shownURL := config.DisplayServerURL(serverURL)
	headline := fmt.Sprintf("logged in to %s (%s)", name, shownURL)
	if !verified {
		headline = fmt.Sprintf("token stored for %s (%s) - not verified", name, shownURL)
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
// read-only - it writes nothing - and never replaces authErr, which stays
// the primary error either way. remedy is the piped login of a refusal that
// has one (the zero value for any other failure): it is the alternative to
// the undo while the entry is as registered, and its hint is the hint.
func decorateDelegatedAuthFailure(ctx context.Context, dir string, authErr error, snap register.AuthSnapshot, remedy token.LoginRefusalRemedy) error {
	return decorateDelegatedFailure(ctx, dir, authErr, snap, "", remedy)
}

// decorateDelegatedVerifyFailure is decorateDelegatedAuthFailure for the
// verify round-trip specifically: an orphaned registration is the bigger
// problem, so the undo-the-registration guidance still owns the hint, but
// --no-verify is a real way out of a verify failure (unlike a plain auth
// failure, where it would just fail again the same way), so the message names
// it as the first alternative to the undo instead of fighting the undo
// guidance for the one hint slot. A TLS_TRUST failure gets no --no-verify
// option: a token stored for a server whose certificate fails helps no later
// command.
func decorateDelegatedVerifyFailure(ctx context.Context, dir string, verifyErr error, snap register.AuthSnapshot) error {
	extra := "bypass verification with --no-verify"
	if ce, ok := errors.AsType[*clierr.Error](verifyErr); ok && ce.Code == clierr.CodeTLSTrust {
		extra = ""
	}
	return decorateDelegatedFailure(ctx, dir, verifyErr, snap, extra, token.LoginRefusalRemedy{})
}

// decorateDelegatedFailure is the shared body behind decorateDelegatedAuthFailure
// and decorateDelegatedVerifyFailure: it rechecks CURRENT state before
// deciding whether the retry hint is still the undo-the-registration command
// (entry unchanged) or has to fall back to a plain "check it" pointer (entry
// changed or vanished since registration). The message is the failure's own
// text, then extra - a caller's own clause, or "" when there is none - then
// the remedy, and the undo command or the "check it" pointer last. While the
// entry is as registered, the remedy (a piped login) adds its command as one
// more alternative to the undo, and its hint is the hint; once the entry
// changed it adds only its command-free part, since the command would name
// the entry as it was. A failure that is not a structured error, or a
// context already cancelled, skips the recheck and returns failure
// unchanged, its own hint included, with no remedy.
func decorateDelegatedFailure(ctx context.Context, dir string, failure error, snap register.AuthSnapshot, extra string, remedy token.LoginRefusalRemedy) error {
	ce, ok := errors.AsType[*clierr.Error](failure)
	if !ok || ctx.Err() != nil {
		// Not a structured error, or cancellation preempts the recheck:
		// preserve the original error verbatim, its own hint included.
		return failure
	}

	var current bool
	lockErr := config.WithLock(ctx, dir, func() error {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return err
		}
		current = register.SnapshotMatchesCurrent(cfg, snap)
		return nil
	})
	if lockErr != nil || !current {
		if remedy.Instruction != "" {
			extra = joinClauses(extra, remedy.Fallback)
		}
		if extra != "" {
			extra = "; " + extra
		}
		return ce.WithMessage(ce.Message + extra +
			fmt.Sprintf("; server entry %q may have changed since registration; check it with: ", snap.Name) + clierr.Rancher + " config list-servers").
			WithHint("")
	}
	hint := config.LoginHintFor(snap.Name)
	if remedy.Instruction != "" {
		extra, hint = joinClauses(extra, remedy.Instruction), remedy.Hint
	}
	undo := "; undo the registration with: "
	if extra != "" {
		undo = "; " + extra + "; or undo the registration with: "
	}
	return ce.WithMessage(ce.Message + undo + config.DeleteServerCommand + clierr.ShellQuoteArgs("", snap.Name)).
		WithHint(hint)
}

// joinClauses joins the clauses that are not empty with "; ".
func joinClauses(clauses ...string) string {
	return strings.Join(slices.DeleteFunc(clauses, func(c string) bool { return c == "" }), "; ")
}

// resolveLoginToken returns the credential to store. Its provenance says
// where it came from: only a credential the provider sign-in flow produced
// needs no second verification round-trip, since a server-issued token is
// authenticated by construction.
func resolveLoginToken(ctx context.Context, server, caPEM string, newIdPClient func() *http.Client, in runtime.Input, p runtime.Prompter) (token.LoginCredential, error) {
	if in.Bool("token-stdin") {
		if in.String("auth-provider") != "" || in.String("auth-flow") != "" || in.String("ttl") != "" {
			return token.LoginCredential{}, clierr.New(clierr.CodeValidation,
				"--auth-provider/--auth-flow/--ttl apply to the provider sign-in flow and are incompatible with --token-stdin; "+
					"drop --token-stdin to use the provider flow, or drop the provider flags to paste/pipe a token")
		}
	}
	if in.Bool("no-verify") && (in.String("auth-provider") != "" || in.String("auth-flow") != "") {
		return token.LoginCredential{}, clierr.New(clierr.CodeValidation,
			"--no-verify has no effect on the provider sign-in flow, which is already authenticated by construction; "+
				"drop --no-verify, or drop the provider flags to skip verification for a pasted token")
	}

	var (
		cred token.LoginCredential
		err  error
	)
	switch {
	case in.Bool("token-stdin"):
		var pasted string
		pasted, err = p.ReadLine(ctx)
		cred = token.UserSupplied(pasted)
	// The provider flow is allowed without a TTY when --auth-provider is
	// explicitly named: the finer-grained gates already inside it (SAML's
	// NoInput check, basicAuth's per-prompt CanPrompt check, OAuth's lack of
	// any TTY requirement) are the real gate - mirroring how `auth get-token`
	// already runs its OAuth device-code/auth-code flows headlessly. Without
	// an explicit provider, the paste-or-provider menu itself needs a TTY.
	case p.CanPrompt() || in.String("auth-provider") != "":
		cred, err = promptForToken(ctx, server, caPEM, newIdPClient, in, p)
	default:
		return token.LoginCredential{}, clierr.New(clierr.CodeValidation, "token required; pipe it via --token-stdin, or run in a terminal")
	}
	if err != nil {
		// A typed clierr (from the provider flow, or an invalid menu pick) is
		// already the right error to surface - the io.EOF/generic mapping
		// below is for the plain-paste read paths only. An AcquireError
		// carries its own primary and all the cleanup evidence, so it is
		// surfaced whole.
		if _, ok := errors.AsType[*token.AcquireError](err); ok {
			return token.LoginCredential{}, err
		}
		if _, ok := errors.AsType[*clierr.Error](err); ok {
			return token.LoginCredential{}, err
		}
		if errors.Is(err, io.EOF) {
			return token.LoginCredential{}, clierr.New(clierr.CodeValidation, "no token provided; pipe it via --token-stdin, or enter it at the prompt")
		}
		return token.LoginCredential{}, clierr.New(clierr.CodeGeneric, "reading token: "+err.Error())
	}
	cred.Token = strings.TrimSpace(cred.Token)
	if cred.Token == "" {
		return token.LoginCredential{}, clierr.New(clierr.CodeValidation, "no token provided; pipe it via --token-stdin, or enter it at the prompt")
	}
	return cred, nil
}

// promptForToken offers the paste-or-provider choice: --auth-provider
// or --auth-flow already signal provider intent and skip straight to it,
// mirroring get-token's own explicit-flag shortcut past its provider menu.
// newIdPClient is a constructor, not a client: the identity-provider client
// is built only on the provider branches, so a pasted or piped token never
// pays for a transport it will not use.
func promptForToken(ctx context.Context, server, caPEM string, newIdPClient func() *http.Client, in runtime.Input, p runtime.Prompter) (token.LoginCredential, error) {
	if in.String("auth-provider") != "" || in.String("auth-flow") != "" {
		return resolveProviderLoginToken(ctx, server, caPEM, newIdPClient(), in, p)
	}
	p.Notice("1) paste an API token\n2) sign in via an identity provider")
	choice, err := p.Prompt(ctx, "Select [1]: ")
	if err != nil {
		return token.LoginCredential{}, err
	}
	switch strings.TrimSpace(choice) {
	case "", "1":
		pasted, err := p.PromptSecret(ctx, "Enter token (id:secret): ")
		return token.UserSupplied(pasted), err
	case "2":
		return resolveProviderLoginToken(ctx, server, caPEM, newIdPClient(), in, p)
	default:
		return token.LoginCredential{}, clierr.New(clierr.CodeAmbiguousSelection, "select 1 (paste) or 2 (identity provider); re-run and answer 1 or 2")
	}
}
