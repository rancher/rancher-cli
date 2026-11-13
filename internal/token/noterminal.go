package token

import (
	"errors"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// needsTerminal is why a sign-in was refused: it has to ask someone, and
// nobody can answer here. The sign-in prompts give it, and both auth
// login and the exec plugin reach them; each says what to do about it in
// its own terms. authConfig is the name of the auth config the sign-in
// was for; noInput is set when --no-input is the reason, in a terminal or
// not.
type needsTerminal struct {
	authConfig string
	noInput    bool
}

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

// interactiveLoginRequired is the refusal a sign-in prompt gives. It names
// no remedy: each command that shows it says what to do in its own terms
// (the exec plugin in withNoTerminalRemedies, auth login in
// AuthLoginRemedy). authConfig names the auth config the sign-in was for;
// noInput says that --no-input is why nobody can answer.
func interactiveLoginRequired(authConfig string, noInput bool) *clierr.Error {
	return clierr.New(clierr.CodeAuth, "interactive login required for auth config "+authConfig).
		WithCause(&needsTerminal{authConfig: authConfig, noInput: noInput})
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
	// authConfig is the call's --auth-provider with --user-id: the login
	// and the sign-in the remedies name are made through it.
	authConfig string
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
// The refusal of an auth config this CLI cannot sign in with keeps its code,
// CAPABILITY_UNSUPPORTED: a terminal would not make the auth config usable,
// so it gets only the piped login (unsupportedConfigRemedy).
//
// While a store change is unfinished the message names it too, and while
// its record cannot be read the status report is the hint. Every other
// error passes through.
func (t tokenCmd) withNoTerminalRemedies(err error, call noTerminalCall) error {
	if _, ok := errors.AsType[*unsupportedConfigRefused](err); ok {
		return t.unsupportedConfigRemedy(err, call)
	}
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
	note := pc.note
	var choice string
	if len(names) > 1 {
		choice = "or add --auth-provider with one of " + strings.Join(names, ", ") + " to the command (in a kubeconfig, to the exec entry's args) and sign in through it in a browser"
	} else if fragment, ok := clierr.FlagFragmentWord("--auth-provider", names[0]); ok {
		choice = "or add " + fragment + " to the command (in a kubeconfig, to the exec entry's args) to sign in through " + names[0] + " in a browser"
	} else {
		note = clierr.WithLeftOutNote(note)
	}
	head := "this server has several auth configs and this call has no terminal to choose one"
	return t.noTerminalError(err, call, clierr.CodeAmbiguousSelection, head, choice, pc.noTerminal, note)
}

// unsupportedConfigRemedy is the exec plugin's form of the refusal of an
// auth config this CLI cannot sign in with. A terminal would not make that
// config usable, so the only remedy is the login that takes a piped API
// token (NoTerminalLogin). It is the hint when it runs as written; for a
// call with no --name, or an inline CA, it is in the message, since a hint
// is a command that runs as written. While a store change is unfinished the
// message names it too.
func (t tokenCmd) unsupportedConfigRemedy(err error, call noTerminalCall) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		return err
	}
	cfg, pending := t.remedyConfig()
	login := NoTerminalLogin(cfg, call.name, call.clusterID, call.serverURL, call.caCert, call.caFrom, call.shim)
	msg := ce.Message
	if !login.hasHint() {
		msg += "; " + login.Instruction("to log in with an API token, pipe it")
	}
	out := clierr.New(ce.Code, clierr.NoteOnce(msg)).WithCause(err).WithCandidates(ce.Candidates)
	if login.hasHint() {
		out = out.WithHintCommand(login.Hint())
	}
	return pending.Apply(out)
}

// remedyConfig is the config in the directory, nil when it cannot be read,
// and the store change it leaves unfinished.
func (t tokenCmd) remedyConfig() (*config.Config, config.UnfinishedChange) {
	var (
		pending config.UnfinishedChange
		cfg     *config.Config
	)
	if loaded, lerr := config.Load(config.FilePath(t.dir)); lerr == nil {
		cfg = loaded
		pending = config.UnfinishedChangeOf(t.dir, cfg)
	}
	return cfg, pending
}

// noTerminalError builds the remedies both refusals share: head says what
// was refused, then the login that takes a piped API token, choice (when
// set) the auth config to name, the sentence about a terminal sign-in, the
// kubeconfig the call could use, and note (when set). cause is kept as the
// new error's cause.
func (t tokenCmd) noTerminalError(cause error, call noTerminalCall, code, head, choice string, candidates []clierr.Candidate, note string) error {
	cfg, pending := t.remedyConfig()
	login := NoTerminalLogin(cfg, call.name, call.clusterID, call.serverURL, call.caCert, call.caFrom, call.shim)
	of, signIn := "", "sign in"
	if call.userID != "" {
		of, signIn = " of Rancher user "+clierr.ShellQuote(call.userID), "sign in as "+clierr.ShellQuote(call.userID)
		if call.authConfig != "" {
			through := " through auth config " + clierr.ShellQuote(call.authConfig)
			of, signIn = of+" made"+through, signIn+through
		}
	}
	msg := head + "; " + login.Instruction("to log in without a terminal, pipe an API token"+of)
	if choice != "" {
		msg += "; " + choice
	}
	switch {
	case call.keepsLogin:
		msg += "; or run kubectl once in a terminal and " + signIn
	case call.name != "" || call.userID != "":
		// Only the cluster token is kept, under this call's --name or, for
		// a call with --user-id, under its user.
		msg += "; or run kubectl once in a terminal and " + signIn + ", which lasts until the cached token expires"
	default:
		msg = clierr.Text(msg+"; this exec entry sets no --name, and a sign-in in a terminal would not be kept; give it one, for example by regenerating the kubeconfig",
			clierr.Lead(" with: "), kubeconfigRemedy(call.clusterID))
	}
	if (call.userID == "" || call.clusterID != "") && t.currentLoginServes(cfg, call) {
		msg = clierr.Text(msg, clierr.Lead("; for a kubeconfig that carries its own token, run: "), CreateKubeconfigRemedy(call.clusterID))
	}
	if note != "" {
		msg += "; " + note
	}
	out := clierr.New(code, clierr.NoteOnce(msg)).WithCause(cause).WithCandidates(candidates)
	if login.hasHint() {
		out = out.WithHintCommand(login.Hint())
	}
	return pending.Apply(out)
}

// The words of auth login's remedy for a refused sign-in, one per form. Each
// ends before the piped login (LoginRemedy.Instruction adds it); auth login
// always has an entry name, so the command is complete.
const (
	loginLeadTerminal    = loginFallbackTerminal + ", or to log in with an API token, pipe it"
	loginLeadNoInput     = loginFallbackNoInput + ", or to log in with an API token, pipe it"
	loginLeadUnsupported = "to log in with an API token, pipe it"

	loginFallbackTerminal = "run in a terminal"
	loginFallbackNoInput  = "run without --no-input"
)

// LoginRefusalRemedy is what auth login adds to a refusal of a sign-in: the
// piped login to run, and what to say when the entry the login would name
// changed after it was registered.
type LoginRefusalRemedy struct {
	// Instruction is the way to a credential that needs no sign-in, ending
	// with the piped login for the entry: for a refusal that a terminal, or
	// no --no-input, would answer, it opens with that.
	Instruction string
	// Hint is the piped login, valid as written.
	Hint string
	// Fallback is the part of Instruction that holds no command ("run in a
	// terminal"), for a text that cannot end with one; "" for the refusal
	// of an auth config this CLI cannot sign in with, which a terminal does
	// not answer.
	Fallback string
}

// AuthLoginRemedyOf is auth login's remedy for the refusals that say nobody
// can answer a sign-in prompt, and for the refusal of an auth config this CLI
// cannot sign in with; false for any other error, nil included. The login is
// the piped login for the entry name at serverURL (NoTerminalLogin over cfg).
// The CA is caSelector, the --cacert value or the entry's stored selector,
// never the PEM, which would leave the hint empty; an entry registered at
// serverURL uses its own CA, so the command names none.
func AuthLoginRemedyOf(err error, cfg *config.Config, name, serverURL, caSelector string) (LoginRefusalRemedy, bool) {
	var lead, fallback string
	if nt, ok := errors.AsType[*needsTerminal](err); ok {
		lead, fallback = loginLeadTerminal, loginFallbackTerminal
		if nt.noInput {
			lead, fallback = loginLeadNoInput, loginFallbackNoInput
		}
	} else if _, ok := errors.AsType[*unsupportedConfigRefused](err); ok {
		lead = loginLeadUnsupported
	} else {
		return LoginRefusalRemedy{}, false
	}
	if _, ok := errors.AsType[*clierr.Error](err); !ok {
		return LoginRefusalRemedy{}, false
	}
	login := NoTerminalLogin(cfg, name, "", serverURL, caSelector, config.CASource{}, false)
	return LoginRefusalRemedy{Instruction: login.Instruction(lead), Hint: login.Hint().Field(), Fallback: fallback}, true
}

// AuthLoginRemedy is err with auth login's remedy (AuthLoginRemedyOf) added
// at the end of its message and its hint set to the piped login. Any other
// error, and nil, is returned as it is.
func AuthLoginRemedy(err error, cfg *config.Config, name, serverURL, caSelector string) error {
	remedy, ok := AuthLoginRemedyOf(err, cfg, name, serverURL, caSelector)
	if !ok {
		return err
	}
	ce, _ := errors.AsType[*clierr.Error](err)
	return ce.WithMessage(ce.Message + "; " + remedy.Instruction).WithHint(remedy.Hint)
}

// currentLoginServes reports whether create kubeconfig, which uses the
// directory's current server and its login, would give the call a
// kubeconfig for the right server and user: the current server answers for
// the call's URL and holds a login, and, for a call with --user-id, that
// login records the call's user and, for a call with an auth config, that
// auth config. A store that cannot be read says no.
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
	// A login that recorded no auth config does not count: create kubeconfig
	// uses it without the plugin's check of the token's auth config.
	if call.authConfig != "" && s.TokenAuthConfig != call.authConfig {
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
func CreateKubeconfigRemedy(clusterID string) clierr.Command {
	create := clierr.NewCommand("create kubeconfig")
	if clusterID == "" {
		create = create.FlagText(" --clusters <id>")
	} else {
		create = create.FlagValue("--clusters", clusterID)
	}
	return create.Flag("--apply").Flag("--yes")
}

// LoginRemedy is the auth login that stores a login for an exec entry's
// call without a terminal: an API token piped to it.
type LoginRemedy struct {
	// command is the auth login, a remedy command. With an inline CA it holds
	// --cacert <file>, which needs the clause Facts gives, so it is read only
	// through Text, Instruction and Hint.
	command clierr.Command
	// steps is the command, then, for an exec entry whose --name stands for an
	// entry auth login refuses, the steps that make the exec entry use it: the
	// new entry made current, then the kubeconfig written again from it.
	steps clierr.Sequence
	// selects reports that steps select an entry, so the text ends with what
	// that does to the directory's current server, current being the one now.
	selects bool
	current string
	// Named reports that the entry name is filled in; otherwise <name>
	// stands for one the operator chooses.
	Named bool
	// InlineCA reports that the call carries its CA inline, so auth login
	// needs it from a file the command cannot name; Facts says what <file>
	// stands for.
	InlineCA bool
	// CASource is where an inline CA came from: RANCHER_CLI_CACERT, or the
	// server entry whose config.yaml record carries it, for a login of
	// another server than the exec entry's; otherwise the exec entry
	// carries it.
	CASource config.CASource
	// name, serverURL and caPath are what the command was built from; the
	// sign-in command is built the same way: the entry name (empty for the
	// placeholder <name>), the server URL and the path of the CA file the
	// command names (empty for none, or when the CA is inline).
	name, serverURL, caPath string
}

// Text is the login and the steps that follow it, as commands: the login
// first, then each step after "; then ".
func (l LoginRemedy) Text() string {
	return clierr.NoteOnce(clierr.Text(l.steps) + l.tail())
}

// Login is the auth login alone.
func (l LoginRemedy) Login() clierr.Command { return l.command }

// tail is what ends the text of a login that selects an entry: what the
// selection does to the directory's current server.
func (l LoginRemedy) tail() string {
	if !l.selects {
		return ""
	}
	return useServerNote(l.current)
}

// Facts says what the operator has to know before the login command: that
// <name> is a name of their choice, and, where an inline CA is carried, that
// --cacert needs a file and what <file> stands for. It is empty when the
// command needs neither.
func (l LoginRemedy) Facts() string {
	var facts []string
	if !l.Named {
		facts = append(facts, "<name> stands for a name of your choice")
	}
	if l.InlineCA {
		facts = append(facts, config.InlineCAHolder(l.CASource)+" and --cacert needs a file", config.CAFileMeaning)
	}
	return strings.Join(facts, "; ")
}

// WithFacts is prose with Facts before it, when there are any.
func (l LoginRemedy) WithFacts(prose string) string {
	if facts := l.Facts(); facts != "" {
		return facts + "; " + prose
	}
	return prose
}

// String is Facts, then the text of the login, so a print of the value (%v,
// %+v, %s) carries Facts before the command, as every message does.
func (l LoginRemedy) String() string {
	return l.WithFacts(l.Text())
}

// Instruction is the login as a sentence names it: Facts, then prose, which
// asks for the login, then the login and the steps after it. The command
// comes last in its clause.
func (l LoginRemedy) Instruction(prose string) string {
	return clierr.NoteOnce(clierr.Text(l.WithFacts(prose), clierr.Lead(" to: "), l.steps) + l.tail())
}

// InstructionSignIn is Instruction for a login that can be made by signing in
// through authConfig as well: Facts, then signInProse, the sign-in, then
// pipedProse, which asks for the piped login, and the login.
func (l LoginRemedy) InstructionSignIn(signInProse, authConfig, pipedProse string) string {
	return clierr.NoteOnce(clierr.Text(l.WithFacts(signInProse), clierr.Lead(" with: "), l.signIn(authConfig),
		"; or "+pipedProse, clierr.Lead(" to: "), l.steps) + l.tail())
}

// signIn is the sign-in through authConfig that logs in the same entry as
// the piped login does: the command without --token-stdin and with
// --auth-provider.
func (l LoginRemedy) signIn(authConfig string) clierr.Command {
	return l.loginCommand().FlagValue("--auth-provider", authConfig)
}

// pipedLogin is the auth login that reads a token from stdin, for the entry
// l.name (the placeholder <name> when empty) at l.serverURL, with the CA
// the login names.
func (l LoginRemedy) pipedLogin() clierr.Command {
	return l.loginCommand().Flag("--token-stdin")
}

// loginCommand is the auth login of the entry, with the URL and the CA.
func (l LoginRemedy) loginCommand() clierr.Command {
	c := clierr.NewCommand(config.LoginWords)
	if l.name == "" {
		c = c.Placeholder("<name>")
	} else {
		c = c.Arg(l.name)
	}
	c = c.FlagValue("--url", l.serverURL)
	switch {
	case l.InlineCA:
		c = c.FlagText(config.CAFileFlag)
	case l.caPath != "":
		c = c.FlagValue("--cacert", l.caPath)
	}
	return c
}

// hasHint reports whether the login runs as written: a hint is a command and
// nothing else.
func (l LoginRemedy) hasHint() bool { return l.Named && !l.InlineCA }

// Hint is the login when it runs as written, the zero Command otherwise.
func (l LoginRemedy) Hint() clierr.Command {
	if l.hasHint() {
		return l.command
	}
	return clierr.Command{}
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
	caPath, inlineCA := "", false
	switch {
	case caCert == "" || (existing && named):
	case strings.Contains(caCert, "-----BEGIN"):
		inlineCA = true
	default:
		caPath = caCert
	}
	login := LoginRemedy{
		Named:     named,
		InlineCA:  inlineCA,
		CASource:  caFrom,
		serverURL: serverURL,
		caPath:    caPath,
	}
	if named {
		login.name = loginEntry
	}
	login.command = login.pipedLogin()
	login.steps = clierr.NewSequence().Then("", login.command)
	if config.IsExecLoginEntryName(loginEntry) {
		if cfg != nil {
			login.current = cfg.CurrentServer
		}
		login.selects = true
		login.steps = login.steps.
			Then("then ", clierr.NewCommand("config use-server").Placeholder("<name>")).
			Then("then ", kubeconfigRemedy(clusterID))
	}
	return login
}
