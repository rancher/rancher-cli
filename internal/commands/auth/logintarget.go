package auth

import (
	"fmt"
	"maps"
	neturl "net/url"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	tokencomplete "github.com/rancher/rancher-cli/internal/token/complete"
)

// loginTarget is the server entry and the connection an auth login means:
// the entry's name, the server to connect to, and the CA to trust.
type loginTarget struct {
	// name is the server entry: the one named, the one derived from --url's
	// host, or the current one.
	name string
	// server is the canonical --url, or the entry's URL.
	server string
	// caSelector is --cacert, or the CA the entry saves: a path or an inline
	// PEM.
	caSelector    string
	caFingerprint string
	// variable is RANCHER_CLI_CACERT where it applies to the server, "" for
	// none. The login connects with it before caSelector, and never saves it.
	variable string
	// known reports that an entry of that name exists.
	known bool
}

// resolveLoginTarget decides which server entry and connection an auth
// login with this name, --url and --cacert means, over cfg and the
// environment, and refuses the inputs that mean none. The login and the
// completion of --auth-provider both call it.
func resolveLoginTarget(cfg *config.Config, name, url, cacert string) (loginTarget, error) {
	if url != "" {
		return urlLoginTarget(cfg, name, url, cacert)
	}

	if name != "" {
		if cacert != "" {
			// Trust changes belong to registration, not authentication. A
			// suggested command must be valid verbatim: only a KNOWN name
			// can receive a fully interpolated update-server hint; an
			// absent/unknown name falls through to VALIDATION/NOT_FOUND
			// below with no hint at all.
			if _, ok := cfg.Servers[name]; ok {
				return loginTarget{}, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("--cacert without --url changes trust for %q, which belongs to registration", name)).
					WithHintCommand(clierr.NewCommand("config update-server").Arg(name).FlagValue("--cacert", cacert))
			}
			return loginTarget{}, loginUnknownNameError(cfg, name)
		}
		existing, ok := cfg.Servers[name]
		if !ok {
			return loginTarget{}, loginUnknownNameError(cfg, name)
		}
		return entryLoginTarget(name, existing)
	}

	if cacert != "" {
		// Matches the --cacert-without-url matrix's absent-name row: body
		// guidance only, no machine hint (neither value is known here).
		return loginTarget{}, clierr.New(clierr.CodeValidation,
			clierr.Text("--cacert without --url and without a name has no entry to attach trust to; "+config.CAFileMeaning+"; pass a name to change an existing entry's trust",
				clierr.Lead(" with: "), clierr.NewCommand("config update-server").Placeholder("<name>").FlagText(config.CAFileFlag)))
	}

	cur, srv, cerr := cfg.CurrentServerEntry()
	if cerr == nil {
		return entryLoginTarget(cur, srv)
	}

	names := slices.Sorted(maps.Keys(cfg.Servers))
	switch len(names) {
	case 0:
		return loginTarget{}, clierr.New(clierr.CodeNotLoggedIn,
			"no servers configured; add one with: "+clierr.Rancher+" auth login --url <url>")
	case 1:
		// Login never selects implicitly: a success here would authenticate
		// an entry the pointer still doesn't name, so the next command
		// would fail NOT_LOGGED_IN anyway. Point at the switch verb
		// instead.
		return loginTarget{}, clierr.New(clierr.CodeNotLoggedIn, "no current server").
			WithHintCommand(configUseServerHint(names[0]))
	default:
		cands := make([]clierr.Candidate, 0, len(names))
		for _, n := range names {
			cands = append(cands, clierr.Candidate{ID: n, Name: n})
		}
		return loginTarget{}, clierr.New(clierr.CodeAmbiguousSelection,
			"multiple servers configured; specify a name").WithCandidates(cands)
	}
}

// entryLoginTarget is the target of a login to an entry already registered:
// its URL and saved CA, and RANCHER_CLI_CACERT where it applies to that URL.
// An entry whose stored URL is unusable (it holds userinfo or an @ after the
// host, or does not parse; only a hand-edited config.yaml can make one) is
// refused, naming the entry and never the URL.
func entryLoginTarget(name string, entry config.Server) (loginTarget, error) {
	if err := config.CheckServerURL(entry.URL); err != nil {
		return loginTarget{}, config.EntryOrigin(name).Refusal(err)
	}
	return loginTarget{name: name, server: entry.URL, caSelector: entry.CA, caFingerprint: entry.CAFingerprint,
		variable: config.EnvCACertFor(entry.URL), known: true}, nil
}

// urlLoginTarget is the target of a login that registers its server with
// --url.
func urlLoginTarget(cfg *config.Config, name, url, cacert string) (loginTarget, error) {
	// canonical is resolved once and reused to derive an absent name from
	// the URL's host, to detect a collision with an existing entry under
	// that name below, and as register.Prepare's input - so a --url
	// login parses the URL exactly once, and an invalid or
	// unsupported-scheme URL is refused before any other work.
	canonical, canonErr := register.CanonicalizeServerURL(url, "")
	if canonErr != nil {
		return loginTarget{}, canonErr
	}

	derived := false
	if name == "" {
		// canonical already round-tripped through url.Parse inside
		// CanonicalServerURL, so this reparse cannot fail.
		u, _ := neturl.Parse(canonical)
		name = u.Hostname()
		derived = true
		// An IPv6 literal survives Hostname() with its colons - only the
		// brackets are stripped - and a colon-bearing entry name reads as
		// a URL fragment everywhere a name is printed or quoted, so
		// derivation refuses it instead of coining one.
		if strings.Contains(name, ":") {
			return loginTarget{}, clierr.New(clierr.CodeValidation,
				"a server name cannot be derived from an IPv6 address; pass an explicit name: "+clierr.Rancher+" auth login <name> --url <url>")
		}
		// With colons refused above, a derived name cannot land in the
		// legacy: or user-id: namespace, but a host can begin with
		// exec-login-; the check refuses that, and keeps a future
		// derivation change from producing reserved names unchecked.
		if ce := config.ValidateEntryName(name); ce != nil {
			return loginTarget{}, ce
		}
	}

	existing, known := cfg.Servers[name]
	// An entry already stored under a name that prints as another name keeps
	// working; a name that is not an entry yet is refused, before any request.
	if !known {
		if ce := config.CheckNewEntryName(name); ce != nil {
			return loginTarget{}, ce
		}
	}
	// A known name whose canonicalized URL disagrees is a different server
	// hiding behind a trusted name - refuse before any network call, rather
	// than let a hijacking or a typo silently re-point the entry (or dial
	// the wrong host first and only then fail deep inside registration).
	// existing.URL is compared as stored: registration only ever stores it
	// canonical.
	if known && canonical != existing.URL {
		return loginTarget{}, nameCollisionError(name, existing.URL, canonical, derived)
	}

	// Known name, --cacert absent: prepare with the EXISTING entry's
	// effective selector - the flag's absence is never treated as a switch
	// to system roots (that would silently strip a private CA on a plain
	// re-login-with-add spelling).
	caSelector := cacert
	if cacert == "" && known {
		caSelector = existing.CA
	}
	// With no --cacert the login connects with RANCHER_CLI_CACERT where it
	// applies to this URL. The variable is never saved, so the entry keeps
	// caSelector, and a failure of that connection is the variable's, not
	// the entry's: no trust prompt, and no "already registered" pointer.
	variable := ""
	if cacert == "" {
		variable = config.EnvCACertFor(canonical)
	}
	return loginTarget{name: name, server: canonical, caSelector: caSelector, variable: variable, known: known}, nil
}

// LiveSource implements completion.Completer: --auth-provider completes
// the names of the server's auth configs.
func (loginCmd) LiveSource() completion.Source { return tokencomplete.AuthConfigSource() }

// CompletionTarget is the server and CA auth login would connect with for
// the line being completed: --url, else the entry named, else the current
// entry, with the CA that choice trusts (--cacert, RANCHER_CLI_CACERT where
// it applies, else the entry's saved CA). A line the command refuses gives
// none.
func (loginCmd) CompletionTarget(cfg *config.Config, req completion.Request) (server, caPEM string, ok bool) {
	name := ""
	if len(req.Positionals) > 0 {
		name = req.Positionals[0]
		if name == "" || config.ValidateEntryName(name) != nil {
			return "", "", false
		}
	}
	target, err := resolveLoginTarget(cfg, name, strings.TrimSpace(completion.TailFlag(req.Tail, "url")), completion.TailFlag(req.Tail, "cacert"))
	if err != nil {
		return "", "", false
	}
	selector := target.caSelector
	if target.variable != "" {
		selector = target.variable
	}
	caPEM, err = register.ResolveCA(target.server, selector)
	if err != nil {
		return "", "", false
	}
	return target.server, caPEM, true
}
