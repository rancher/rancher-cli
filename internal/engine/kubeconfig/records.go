package kubeconfig

import (
	"errors"
	"fmt"
	"os"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
)

// installURL is where the rancher CLI is installed from, the default
// command's installHint.
const installURL = "https://github.com/rancher/rancher-cli"

// installHintDefault is the installHint of an exec entry that runs the
// default command.
const installHintDefault = "install the rancher CLI: " + installURL

// recordedStore is the RANCHER_CLI_CREDENTIAL_STORE value the exec entry
// sets, so kubectl's own never reaches the plugin: empty for a directory
// that declares its store, which then decides every read, and otherwise the
// value set when this command ran (empty when none was), so the plugin's
// first login stores where this command's would.
func recordedStore(cfg *config.Config) string {
	if cfg.CredentialStore != "" {
		return ""
	}
	return os.Getenv(config.EnvCredentialStore)
}

// execEnv is the exec entry's env: the config directory, always, and the
// store variable. Nothing else - the token is never read by the plugin,
// and the URL and CA are in its flags.
func execEnv(dir, store string) []clientcmdapi.ExecEnvVar {
	return []clientcmdapi.ExecEnvVar{
		{Name: config.EnvConfigDir, Value: dir},
		{Name: config.EnvCredentialStore, Value: store},
	}
}

// installHint is what kubectl prints when the exec entry's command cannot
// be run. kubectl prints it itself, so every value in it is printable. For
// a command other than the default it names the steps that write the
// kubeconfig again from the entry it was written from; with no entry (an
// environment-only session) the variables it was written with, the CA's
// included when the file carries one.
func installHint(command, entry, clusterID, dir, serverURL string, carriesCA bool) string {
	if command == defaultExecCommand {
		return installHintDefault
	}
	shown := output.Printable(command) + " was not found; restore it, or "
	prefix, printable := runtime.RemedyPrefix(dir)
	switch {
	case !printable:
		shown += "regenerate this kubeconfig; " + runtime.UnprintableConfigDirNote
	case entry == "":
		shown += "regenerate this kubeconfig with " + config.EnvURL + " set to " + clierr.ShellQuote(serverURL)
		if carriesCA {
			shown += ", " + config.EnvCACert + " set to the CA this kubeconfig carries"
		}
		shown += " and " + config.EnvToken + " set: rancher" + prefix + " kubeconfig --cluster " + clierr.ShellQuote(clusterID)
	default:
		shown += "regenerate this kubeconfig from entry " + clierr.ShellQuote(entry) + ": " +
			token.RegenerateSteps("rancher"+prefix, entry, clusterID)
	}
	return output.Printable(shown)
}

// machineNotice says what of this machine the file records: its config
// directory always, the store variable when it is not empty, and the
// command when it is a path.
func machineNotice(dir, store, command string, commandIsPath bool) string {
	msg := "this kubeconfig records this machine's config directory " + output.Printable(dir)
	if output.Printable(dir) != dir {
		msg += " (its path has characters a terminal cannot show)"
	}
	if store != "" {
		msg += ", credential store " + output.Printable(store)
	}
	if commandIsPath {
		msg += " and command " + output.Printable(command)
	}
	return msg + "; it works for this user on this machine only"
}

// effectiveRead answers the stored login of the entry Effective already
// read, from that read, and asks next for every other entry: the same
// entry, the same store, so the answer is the one a second read would
// give, and the common case costs no read.
type effectiveRead struct {
	entry, token string
	next         token.CredentialReader
}

func (e effectiveRead) GetToken(ref config.ServerRef) (string, error) {
	if ref.Name == e.entry {
		return e.token, nil
	}
	return e.next.GetToken(ref)
}

// lookupCall is what the lookup notice is decided from: the exec entry's
// call as the plugin will make it, and what the session used.
type lookupCall struct {
	cfg                   *config.Config
	dir, server, serverID string
	name, clusterID       string
	// caSource is the session's CA as config.yaml or the environment names
	// it, a path or an inline PEM, for a remedy that names a new entry;
	// eff.CASource says which of the two it is.
	caSource string
	eff      config.Effective
	envToken bool
	store    config.CredStore
}

// lookupStore is the store the plugin will open: the declared one when
// the directory declares a store, since the exec entry sets the variable
// empty there, else the one this command opened, which the variable this
// command ran with chose, as it will the plugin's.
func (l lookupCall) lookupStore() (token.CredentialReader, error) {
	if _, _, err := config.EffectivePolicyWithSource(l.cfg.CredentialStore); errors.Is(err, config.ErrCredentialStoreConflict) {
		return config.OpenDeclaredStore(l.cfg, l.dir)
	}
	if !l.envToken && l.eff.ServerName != "" && l.eff.Token != "" {
		return effectiveRead{entry: l.eff.ServerName, token: l.eff.Token, next: l.store}, nil
	}
	return l.store, nil
}

// lookup is the exec plugin's own lookup for the call, over the store it
// will open; a store that cannot be opened is a read failure.
func (l lookupCall) lookup() token.StoredLogin {
	reader, err := l.lookupStore()
	if err != nil {
		return token.StoredLogin{Outcome: token.LookupReadFailed, Err: err}
	}
	return token.LookupStoredLogin(l.cfg, reader, l.server, l.name, l.clusterID, "")
}

// recordedAuthConfig is the --auth-provider the exec entry carries beside
// --user-id: the auth config the answering login recorded, when it is an
// auth config name. An entry that recorded none, or one that is not a name,
// gets none, and no notice: a call without --auth-provider checks no auth
// config.
func (l lookupCall) recordedAuthConfig(lk token.StoredLogin) string {
	if lk.Outcome != token.LookupAnswered {
		return ""
	}
	recorded := l.cfg.Servers[lk.Entry].TokenAuthConfig
	if token.ValidAuthConfigName(recorded) {
		return recorded
	}
	return ""
}

// recordedOwner is the --user-id the exec entry carries - the owner the
// login that answers records, when it is a user id - or why it carries
// none: the answering login records no owner. Any other outcome of the
// lookup gives neither; its notice says what it is.
func (l lookupCall) recordedOwner(lk token.StoredLogin) (owner, notice string) {
	if lk.Outcome != token.LookupAnswered {
		return "", ""
	}
	recorded := l.cfg.Servers[lk.Entry].TokenUserID
	if token.ValidUserID(recorded) {
		return recorded, ""
	}
	whose := "the stored login of " + clierr.ShellQuote(lk.Entry)
	if lk.PluginEntry {
		whose = "the stored login of the exec plugin's own login entry " + clierr.ShellQuote(lk.Entry)
	}
	if recorded != "" {
		return "", runtime.PrintableRemedies("this kubeconfig names no Rancher user: " + whose + " records the owner " +
			clierr.ShellQuote(clierr.IDText(recorded)) + ", which --user-id does not accept; the exec plugin does not check the user")
	}
	var remedy string
	// auth login refuses a name in the exec plugin's own login entry
	// namespace whatever the entry's provenance, so such a name is never
	// the login target.
	switch {
	case !lk.PluginEntry && !config.IsExecLoginEntryName(lk.Entry):
		remedy = "pipe an API token to: " + clierr.Rancher + " auth login" + clierr.ShellQuoteArgs(" --token-stdin", lk.Entry) + kubeconfigAgain(l.clusterID)
	case lk.Identified != "" && lk.Identified != lk.Entry && !config.IsExecLoginEntryName(lk.Identified):
		// The identified entry fell back to the plugin login entry: its
		// own login, which records its owner, then answers.
		remedy = "pipe an API token to: " + clierr.Rancher + " auth login" + clierr.ShellQuoteArgs(" --token-stdin", lk.Identified) + kubeconfigAgain(l.clusterID)
	default:
		// The exec plugin's own login entry: auth login refuses its name,
		// and the next sign-in makes it again with its owner.
		remedy = "remove " + clierr.ShellQuote(lk.Entry) + " with: " + clierr.Rancher + " auth logout" + clierr.ShellQuoteArgs("", lk.Entry) +
			"; then " + clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", lk.Entry) + kubeconfigAgain(l.clusterID)
	}
	return "", runtime.PrintableRemedies("this kubeconfig names no Rancher user: " + whose +
		" records no owner; the exec plugin does not check the user; " + remedy)
}

// lookupNotice is what the kubeconfig says about the login kubectl's exec
// plugin will use, decided by the plugin's own lookup lk over the store it
// will open: nothing when it is the session's own; which login otherwise;
// how to get one when none answers; the plugin's refusal with its
// remedies; or that the store could not be read. A store failure is a
// notice, so RANCHER_CLI_TOKEN still writes the file.
func lookupNotice(l lookupCall, lk token.StoredLogin) string {
	switch lk.Outcome {
	case token.LookupAnswered:
		if !l.envToken && lk.Entry == l.eff.ServerName {
			return ""
		}
		through := "the stored login of " + clierr.ShellQuote(lk.Entry)
		if lk.PluginEntry {
			through = "the exec plugin's own login entry " + clierr.ShellQuote(lk.Entry)
		}
		not := config.EnvToken
		if !l.envToken {
			not = "the login of " + clierr.ShellQuote(l.eff.ServerName)
		}
		return runtime.PrintableRemedies("this kubeconfig authenticates through " + through + ", not through " + not)
	case token.LookupNone:
		login := token.NoTerminalLogin(l.cfg, l.name, l.clusterID, l.serverID, l.caSource, l.eff.CASource, false)
		createKubeconfig := token.CreateKubeconfigRemedy(l.clusterID)
		return runtime.PrintableRemedies(fmt.Sprintf("no stored login in %s answers for this kubeconfig: kubectl will sign in on first use when it has a terminal; "+
			"%s; or, for a kubeconfig that carries its own token, run: %s",
			l.dir, login.Instruction("for unattended use, pipe an API token to: "), createKubeconfig))
	case token.LookupRefused:
		// The lookup's refusals carry their remedies in the message; none
		// has a hint.
		msg := lk.Err.Error()
		if ce, ok := errors.AsType[*clierr.Error](lk.Err); ok {
			msg = ce.Message
		}
		return runtime.PrintableRemedies("kubectl's exec plugin will refuse this kubeconfig: " + msg)
	}
	entry := lk.Identified
	if entry == "" {
		entry = l.name
	}
	return runtime.PrintableRemedies(fmt.Sprintf("could not check the stored login for %s (%v); kubectl's exec plugin fails the same way until it can",
		clierr.ShellQuote(entry), lk.Err))
}

// reservedName refuses a --name auth get-token refuses: one in the
// namespace of the legacy token shim's cache slots or of the exec plugin's
// --user-id cache slots, which an environment-only session's host can
// begin with. The remedy is a login under an entry of another name, with
// the session's CA, made current.
func reservedName(name, clusterID, serverURL, ca string, from config.CASource) error {
	prefix := ""
	switch {
	case config.IsLegacyCacheSlot(name):
		prefix = config.LegacyCachePrefix
	case config.IsUserIDCacheSlot(name):
		prefix = config.UserIDCachePrefix
	default:
		return nil
	}
	login := token.NoTerminalLogin(nil, "", clusterID, serverURL, ca, from, false)
	return clierr.New(clierr.CodeValidation, "the kubeconfig's --name "+clierr.ShellQuote(name)+" would start with "+clierr.ShellQuote(prefix)+
		", which auth get-token refuses; "+login.Instruction("to log in under a server entry of another name, pipe an API token to: ")+
		"; then "+clierr.Rancher+" config use-server <name>"+kubeconfigAgain(clusterID))
}

// kubeconfigAgain is the step that writes the kubeconfig for a cluster again,
// after the steps before it in the same sentence.
func kubeconfigAgain(clusterID string) string {
	return "; then write this kubeconfig again with: " + clierr.Rancher + " kubeconfig" + clierr.ShellQuoteArgs(" --cluster "+clierr.ShellQuote(clusterID))
}
