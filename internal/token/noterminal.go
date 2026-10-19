package token

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// needsTerminal is why a sign-in was refused: it has to ask someone, and
// nobody can answer here. The four sign-in prompts give it, and both auth
// login and the exec plugin reach them; each says what to do about it in
// its own terms. authConfig is the name of the auth config the sign-in
// was for.
type needsTerminal struct{ authConfig string }

func (e *needsTerminal) Error() string {
	return "interactive login required for auth config " + e.authConfig
}

// providerChoiceNeedsTerminal is why a sign-in was refused before any auth
// config was chosen: the server offers several, and nobody can answer the
// menu here. text is the refusal's message without the note; configs are
// the names --auth-provider can take for the offered entries, in the
// server's order; noTerminal are those among them that sign in with no
// terminal (SAML and OAuth, and only without --no-input), as candidates;
// note names the providers the server advertises that this CLI cannot use,
// "" when there are none.
type providerChoiceNeedsTerminal struct {
	text       string
	configs    []string
	noTerminal []clierr.Candidate
	note       string
}

func (e *providerChoiceNeedsTerminal) Error() string { return e.text }

// interactiveLoginRequired is the refusal a sign-in prompt gives, with the
// remedy of a command run by someone who could answer it. authConfig names
// the auth config the sign-in was for.
func interactiveLoginRequired(authConfig, remedy string) *clierr.Error {
	return clierr.New(clierr.CodeAuth, "interactive login required for auth config "+authConfig+"; "+remedy).
		WithCause(&needsTerminal{authConfig: authConfig})
}

// noTerminalCall is what the plugin's no-terminal remedies are filled in
// with: the call's --name and --cluster, the server URL, the CA the call
// resolved - its --cacert, or else RANCHER_CLI_CACERT, or else the CA of
// the entry the call means or the matching entry in config.yaml; a path
// or an inline PEM - and where it came from, whether the
// call is the legacy shim's, and whether a sign-in would store a login
// credential - token reuse on and a cluster or a user named, the
// plugin-created login's lane - which later calls reuse with a --name or
// without.
type noTerminalCall struct {
	name, clusterID, serverURL, caCert string
	caFrom                             config.CASource
	shim                               bool
	keepsLogin                         bool
	// userID is the call's --user-id: the login and the sign-in the
	// remedies name are that user's.
	userID string
}

// withNoTerminalRemedies is the exec plugin's form of that refusal. kubectl
// may run the plugin with no terminal, and from a bare install, so the
// error names the ways to a credential that need none:
//   - a token piped to auth login (NoTerminalLogin): the entry the call
//     means, the server and the CA the call resolved - none for an entry
//     already registered here, whose own CA auth login uses. With no
//     --name, or one auth login refuses, the message asks for a name; with
//     an inline CA, for the CA's file. Either way no hint is given, since a
//     hint is a command that runs as written;
//   - kubectl run once in a terminal, only where what it makes is kept: a
//     login credential in the plugin-created login's lane, or else, with a
//     --name or a --user-id, the cluster token until it expires. With
//     neither the message says the exec entry needs a --name;
//   - a kubeconfig that carries its own token, create kubeconfig with the
//     cluster and both of its gates, only when the current server's login
//     would serve the call (currentLoginServes), and never for a call with
//     --user-id and no cluster, which asks for a token for Rancher itself.
//
// The refusal to choose between several auth configs with no terminal to
// answer the menu gets the same remedies. It offers, as candidates and in
// the message, the auth configs that sign in with no terminal, and is
// AMBIGUOUS_SELECTION; when none does, or under --no-input, it is the
// AUTH refusal of a sign-in prompt for all of them.
//
// While a store change is unfinished the message names it too, and while
// its record cannot be read the status report is the hint. Every other
// error passes through.
func (t tokenCmd) withNoTerminalRemedies(err error, call noTerminalCall) error {
	if nt, ok := errors.AsType[*needsTerminal](err); ok {
		head := "interactive login required for auth config " + nt.authConfig + "; this call has no terminal to answer it"
		return t.noTerminalError(err, call, clierr.CodeAuth, head, "", nil, "")
	}
	pc, ok := errors.AsType[*providerChoiceNeedsTerminal](err)
	if !ok {
		return err
	}
	if len(pc.noTerminal) == 0 {
		head := "interactive login required for one of the auth configs " + strings.Join(pc.configs, ", ") + "; this call has no terminal to answer it"
		return t.noTerminalError(err, call, clierr.CodeAuth, head, "", nil, pc.note)
	}
	names := make([]string, 0, len(pc.noTerminal))
	for _, c := range pc.noTerminal {
		names = append(names, c.ID)
	}
	choice := "or add --auth-provider " + names[0] + " to the command (in a kubeconfig, to the exec entry's args) to sign in through " + names[0] + " in a browser"
	if len(names) > 1 {
		choice = "or add --auth-provider with one of " + strings.Join(names, ", ") + " to the command (in a kubeconfig, to the exec entry's args) and sign in through it in a browser"
	}
	head := "this server has several auth configs and this call has no terminal to choose one"
	return t.noTerminalError(err, call, clierr.CodeAmbiguousSelection, head, choice, pc.noTerminal, pc.note)
}

// noTerminalError builds the remedies both refusals share: head says what
// was refused, then the login that takes a piped API token, choice (when
// set) the auth config to name, the sentence about a terminal sign-in, the
// kubeconfig the call could use, and note (when set). cause is kept as the
// new error's cause.
func (t tokenCmd) noTerminalError(cause error, call noTerminalCall, code, head, choice string, candidates []clierr.Candidate, note string) error {
	var (
		pending config.UnfinishedChange
		cfg     *config.Config
	)
	if loaded, lerr := config.Load(config.FilePath(t.dir)); lerr == nil {
		cfg = loaded
		pending = config.UnfinishedChangeOf(t.dir, cfg)
	}
	login := NoTerminalLogin(cfg, call.name, call.clusterID, call.serverURL, call.caCert, call.caFrom, call.shim)
	of, signIn := "", "sign in"
	if call.userID != "" {
		of, signIn = " of Rancher user "+clierr.ShellQuote(call.userID), "sign in as "+clierr.ShellQuote(call.userID)
	}
	msg := fmt.Sprintf("%s; to log in without one, pipe an API token%s to %s", head, of, login.Text())
	if choice != "" {
		msg += "; " + choice
	}
	cluster := "<id>"
	if call.clusterID != "" {
		cluster = clierr.ShellQuote(call.clusterID)
	}
	switch {
	case call.keepsLogin:
		msg += "; or run kubectl once in a terminal and " + signIn
	case call.name != "" || call.userID != "":
		// Only the cluster token is kept, under this call's --name or, for
		// a call with --user-id, under its user.
		msg += "; or run kubectl once in a terminal and " + signIn + ", which lasts until the cached token expires"
	default:
		msg += "; this exec entry sets no --name, so a sign-in in a terminal would not be kept: give it one, for example by regenerating the kubeconfig with " + clierr.Rancher + " kubeconfig --cluster " + cluster
	}
	if (call.userID == "" || call.clusterID != "") && t.currentLoginServes(cfg, call) {
		msg += "; or use " + CreateKubeconfigRemedy(call.clusterID) + " for a kubeconfig that carries its own token"
	}
	if note != "" {
		msg += "; " + note
	}
	out := clierr.New(code, msg).WithCause(cause).WithCandidates(candidates)
	if h := login.Hint(); h != "" {
		out = out.WithHint(h)
	}
	return pending.Apply(out)
}

// currentLoginServes reports whether create kubeconfig, which uses the
// directory's current server and its login, would give the call a
// kubeconfig for the right server and user: the current server answers for
// the call's URL and holds a login, and, for a call with --user-id, that
// login records the call's user. A store that cannot be read says no.
func (t tokenCmd) currentLoginServes(cfg *config.Config, call noTerminalCall) bool {
	if cfg == nil || t.store == nil {
		return false
	}
	s, ok := cfg.Servers[cfg.CurrentServer]
	if !ok || !config.SameServerURL(s.URL, call.serverURL) {
		return false
	}
	if call.userID != "" && s.TokenUserID != call.userID {
		return false
	}
	tok, err := t.store.GetToken(config.ServerRef{Name: cfg.CurrentServer, URL: s.URL})
	return err == nil && strings.TrimSpace(tok) != ""
}

// entryOfName is the server entry an exec entry's --name stands for, as
// the plugin can tell: the entry it identifies; else, when it identifies
// none here and none at another URL (which the plugin refuses before
// anything else), the <entry> of a --name of the form <entry>-<cluster>,
// which the plugin then identifies for every cluster's kubeconfig; else
// the --name. cfg may be nil (config.yaml could not be read). The legacy
// shim names no entry.
func entryOfName(cfg *config.Config, server, name, clusterID string, shim bool) string {
	unidentified := cfg == nil
	if cfg != nil {
		id, err := identifyEntry(cfg, server, name, clusterID, shim)
		switch {
		case err == nil && id != "":
			return id
		case err == nil:
			unidentified = true
		}
	}
	if unidentified && !shim && clusterID != "" {
		if entry, ok := strings.CutSuffix(name, "-"+clusterID); ok && entry != "" {
			return entry
		}
	}
	return name
}

// CreateKubeconfigRemedy is the command that writes a kubeconfig carrying
// its own token for the cluster, with both of the gates create kubeconfig
// needs to run: it is destructive. An empty cluster names the placeholder.
func CreateKubeconfigRemedy(clusterID string) string {
	cluster := "<id>"
	if clusterID != "" {
		cluster = clierr.ShellQuote(clusterID)
	}
	return clierr.Rancher + " create kubeconfig --clusters " + cluster + " --apply --yes"
}

// LoginRemedy is the auth login that stores a login for an exec entry's
// call without a terminal: an API token piped to it.
type LoginRemedy struct {
	// Command is the auth login, a remedy command.
	Command string
	// Named reports that the entry name is filled in; otherwise <name>
	// stands for one the operator chooses.
	Named bool
	// InlineCA reports that the call carries its CA inline, so auth login
	// needs it from a file the command cannot name.
	InlineCA bool
	// CASource is where an inline CA came from: RANCHER_CLI_CACERT, or the
	// server entry whose config.yaml record carries it, for a login of
	// another server than the exec entry's; otherwise the exec entry
	// carries it.
	CASource config.CASource
	// Then are the steps after the login that make the exec entry use it,
	// for an exec entry whose --name stands for an entry auth login
	// refuses: the new entry made current, then the kubeconfig written
	// again from it. Empty when the login alone is enough.
	Then string
}

// Text is the login as a sentence names it, with what the operator has to
// fill in.
func (l LoginRemedy) Text() string {
	s := l.Command
	if !l.Named {
		s += ", with a name of your choice for <name>"
	}
	if l.InlineCA {
		s += config.InlineCAClause(l.CASource)
	}
	if l.Then != "" {
		s += ", then " + l.Then
	}
	return s
}

// Hint is the login when it runs as written, "" otherwise: a hint is a
// command and nothing else.
func (l LoginRemedy) Hint() string {
	if l.Named && !l.InlineCA {
		return l.Command
	}
	return ""
}

// NoTerminalLogin builds the login the exec plugin's no-terminal error
// names for a call with --name name, --cluster clusterID, the server URL
// and the CA the call resolved (a path or an inline PEM) with where it
// came from, over cfg (nil when config.yaml could not be read). The entry is the one
// the call's --name identifies, which may carry another name than the
// --name; else, for a --name of the form <entry>-<cluster>, <entry>,
// which the plugin then identifies for every cluster's kubeconfig; else
// the --name. A name auth login refuses, or one registered at another URL,
// becomes <name>. An entry registered here uses its own CA, so no --cacert
// is named for it. When the entry is one of the exec plugin's own login
// entries, which auth login refuses, the new entry the login makes is not
// the one the exec entry names, so the remedy goes on to make it current
// and write the kubeconfig again from it (such a name is never Named, so
// the remedy is no hint).
func NoTerminalLogin(cfg *config.Config, name, clusterID, serverURL, caCert string, caFrom config.CASource, shim bool) LoginRemedy {
	loginEntry := entryOfName(cfg, serverURL, name, clusterID, shim)
	named := loginEntry != "" && config.ValidateEntryName(loginEntry) == nil
	existing := false
	if cfg != nil {
		if s, ok := cfg.Servers[loginEntry]; ok {
			// auth login refuses a name registered at another server, and
			// for one registered here it uses the entry's own CA and
			// refuses a --cacert that differs from it.
			existing = config.SameServerURL(s.URL, serverURL)
			named = named && existing
		}
	}
	entry := "<name>"
	if named {
		entry = clierr.ShellQuote(loginEntry)
	}
	caFlag, inlineCA := "", false
	switch {
	case caCert == "" || (existing && named):
	case strings.Contains(caCert, "-----BEGIN"):
		inlineCA = true
	default:
		caFlag = " --cacert " + clierr.ShellQuote(caCert)
	}
	then := ""
	if config.IsExecLoginEntryName(loginEntry) {
		cluster, current := "<id>", ""
		if clusterID != "" {
			cluster = clierr.ShellQuote(clusterID)
		}
		if cfg != nil {
			current = cfg.CurrentServer
		}
		then = useServerRemedy("<name>", current) + ", then " + clierr.Rancher + " kubeconfig --cluster " + cluster
	}
	return LoginRemedy{
		Command:  fmt.Sprintf(clierr.Rancher+" auth login %s --url %s%s --token-stdin", entry, clierr.ShellQuote(serverURL), caFlag),
		Named:    named,
		InlineCA: inlineCA,
		CASource: caFrom,
		Then:     then,
	}
}
