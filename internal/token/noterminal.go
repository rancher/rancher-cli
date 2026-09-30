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
// its own terms.
type needsTerminal struct{ provider string }

func (e *needsTerminal) Error() string { return "interactive login required for " + e.provider }

// interactiveLoginRequired is the refusal a sign-in prompt gives, with the
// remedy of a command run by someone who could answer it.
func interactiveLoginRequired(provider, remedy string) *clierr.Error {
	return clierr.New(clierr.CodeAuth, "interactive login required for "+provider+"; "+remedy).
		WithCause(&needsTerminal{provider: provider})
}

// noTerminalCall is what the plugin's no-terminal remedies are filled in
// with: the call's --name and --cluster, the server URL, the CA the call
// resolved - its --cacert, or else the CA of the entry the call means or
// the matching entry in config.yaml; a path or an inline PEM - whether the
// call is the legacy shim's, and whether a sign-in would store a login
// credential - token reuse on and a cluster named, the plugin-created
// login's lane - which later calls reuse with a --name or without.
type noTerminalCall struct {
	name, clusterID, serverURL, caCert string
	shim                               bool
	keepsLogin                         bool
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
//     --name, the cluster token until it expires. With neither the message
//     says the exec entry needs a --name;
//   - a kubeconfig that carries its own token, create kubeconfig with the
//     cluster and both of its gates.
//
// While a store change is unfinished the message names it too, and while
// its record cannot be read the status report is the hint. Every other
// error passes through.
func (t tokenCmd) withNoTerminalRemedies(err error, call noTerminalCall) error {
	nt, ok := errors.AsType[*needsTerminal](err)
	if !ok {
		return err
	}
	var (
		pending config.UnfinishedChange
		cfg     *config.Config
	)
	if loaded, lerr := config.Load(config.FilePath(t.dir)); lerr == nil {
		cfg = loaded
		pending = config.UnfinishedChangeOf(t.dir, cfg)
	}
	login := NoTerminalLogin(cfg, call.name, call.clusterID, call.serverURL, call.caCert, call.shim)
	msg := fmt.Sprintf("interactive login required for %s, and this call has no terminal to answer it; to log in without one, pipe an API token to %s", nt.provider, login.Text())
	cluster := "<id>"
	if call.clusterID != "" {
		cluster = clierr.ShellQuote(call.clusterID)
	}
	switch {
	case call.keepsLogin:
		msg += "; or run kubectl once in a terminal and sign in"
	case call.name != "":
		// Only the cluster token is kept, under this call's --name.
		msg += "; or run kubectl once in a terminal and sign in, which lasts until the cached token expires"
	default:
		msg += "; this exec entry sets no --name, so a sign-in in a terminal would not be kept: give it one, for example by regenerating the kubeconfig with " + clierr.Rancher + " kubeconfig --cluster " + cluster
	}
	msg += "; or use " + CreateKubeconfigRemedy(call.clusterID) + " for a kubeconfig that carries its own token"
	out := clierr.New(clierr.CodeAuth, msg).WithCause(err)
	if h := login.Hint(); h != "" {
		out = out.WithHint(h)
	}
	return pending.Apply(out)
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
}

// Text is the login as a sentence names it, with what the operator has to
// fill in.
func (l LoginRemedy) Text() string {
	s := l.Command
	if !l.Named {
		s += ", with a name of your choice for <name>"
	}
	if l.InlineCA {
		s += ", adding --cacert with the path of a file that holds the server's CA certificate, which this exec entry carries inline"
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
// and the CA the call resolved (its --cacert, a path or an inline PEM),
// over cfg (nil when config.yaml could not be read). The entry is the one
// the call's --name identifies, which may carry another name than the
// --name; else, for a --name of the form <entry>-<cluster>, <entry>,
// which the plugin then identifies for every cluster's kubeconfig; else
// the --name. A name auth login refuses, or one registered at another URL,
// becomes <name>. An entry registered here uses its own CA, so no --cacert
// is named for it.
func NoTerminalLogin(cfg *config.Config, name, clusterID, serverURL, caCert string, shim bool) LoginRemedy {
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
	return LoginRemedy{
		Command:  fmt.Sprintf(clierr.Rancher+" auth login %s --url %s%s --token-stdin", entry, clierr.ShellQuote(serverURL), caFlag),
		Named:    named,
		InlineCA: inlineCA,
	}
}
