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
//   - a token piped to auth login, with the entry the call means (else the
//     plugin's --name), the server and the CA the call resolved - none for
//     an entry already registered here, whose own CA auth login uses. With
//     no --name, or one auth login refuses, the message asks for a name;
//     with an inline CA, for the CA's file. Either way no hint is given,
//     since a hint is a command that runs as written;
//   - kubectl run once in a terminal, only where what it makes is kept: a
//     login credential in the plugin-created login's lane, or else, with a
//     --name, the cluster token until it expires. With neither the message
//     says the exec entry needs a --name;
//   - a kubeconfig that carries its own token.
//
// While a store change is unfinished the message names it too, and while
// its record cannot be read the status report is the hint. Every other
// error passes through.
func (t tokenCmd) withNoTerminalRemedies(err error, call noTerminalCall) error {
	nt, ok := errors.AsType[*needsTerminal](err)
	if !ok {
		return err
	}
	name := call.name
	// loginEntry is the entry the suggested auth login signs in to: the entry
	// the call means, which may carry a different name than the --name
	// that identified it, else the --name itself.
	loginEntry := name
	existing := false
	var (
		pending config.UnfinishedChange
		cfg     *config.Config
	)
	if loaded, lerr := config.Load(config.FilePath(t.dir)); lerr == nil {
		cfg = loaded
		if id, ierr := identifyEntry(cfg, call.serverURL, call.name, call.clusterID, call.shim); ierr == nil && id != "" {
			loginEntry = id
		}
		pending = config.UnfinishedChangeOf(t.dir, cfg)
	}
	named := loginEntry != "" && config.ValidateEntryName(loginEntry) == nil
	if cfg != nil {
		if s, ok := cfg.Servers[loginEntry]; ok {
			// auth login refuses a name registered at another server, and
			// for one registered here it uses the entry's own CA and
			// refuses a --cacert that differs from it.
			existing = config.SameServerURL(s.URL, call.serverURL)
			named = named && existing
		}
	}
	entry := "<name>"
	if named {
		entry = clierr.ShellQuote(loginEntry)
	}
	caFlag, inlineCA := "", false
	switch {
	case call.caCert == "" || (existing && named):
	case strings.Contains(call.caCert, "-----BEGIN"):
		inlineCA = true
	default:
		caFlag = " --cacert " + clierr.ShellQuote(call.caCert)
	}
	login := fmt.Sprintf("rancher auth login %s --url %s%s --token-stdin", entry, clierr.ShellQuote(call.serverURL), caFlag)
	msg := fmt.Sprintf("interactive login required for %s, and this call has no terminal to answer it; to log in without one, pipe an API token to %s", nt.provider, login)
	if !named {
		msg += ", with a name of your choice for <name>"
	}
	if inlineCA {
		msg += ", adding --cacert with the path of a file that holds the server's CA certificate, which this exec entry carries inline"
	}
	switch {
	case call.keepsLogin:
		msg += "; or run kubectl once in a terminal and sign in"
	case name != "":
		// Only the cluster token is kept, under this call's --name.
		msg += "; or run kubectl once in a terminal and sign in, which lasts until the cached token expires"
	default:
		cluster := "<id>"
		if call.clusterID != "" {
			cluster = clierr.ShellQuote(call.clusterID)
		}
		msg += "; this exec entry sets no --name, so a sign-in in a terminal would not be kept: give it one, for example by regenerating the kubeconfig with rancher kubeconfig --cluster " + cluster
	}
	msg += "; or use rancher create kubeconfig for a kubeconfig that carries its own token"
	out := clierr.New(clierr.CodeAuth, msg).WithCause(err)
	if named && !inlineCA {
		out = out.WithHint(login)
	}
	return pending.Apply(out)
}
