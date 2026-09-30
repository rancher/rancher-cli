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
		msg += " (its path holds characters that cannot be printed)"
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
	name, clusterID, ca   string
	eff                   config.Effective
	envToken              bool
	store                 config.CredStore
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

// lookupNotice is what the kubeconfig says about the login kubectl's exec
// plugin will use, decided by the plugin's own lookup over the store it
// will open: nothing when it is the session's own; which login otherwise;
// how to get one when none answers; the plugin's refusal with its
// remedies; or that the store could not be read. A store failure is a
// notice, so RANCHER_CLI_TOKEN still writes the file.
func lookupNotice(l lookupCall) string {
	reader, err := l.lookupStore()
	var lk token.StoredLogin
	if err == nil {
		lk = token.LookupStoredLogin(l.cfg, reader, l.server, l.name, l.clusterID)
	} else {
		lk = token.StoredLogin{Outcome: token.LookupReadFailed, Err: err}
	}
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
		login := token.NoTerminalLogin(l.cfg, l.name, l.clusterID, l.serverID, l.ca, false)
		return runtime.PrintableRemedies(fmt.Sprintf("no stored login in %s answers for this kubeconfig: kubectl will sign in on first use when it has a terminal; "+
			"for unattended use pipe an API token to %s first, or use %s for a kubeconfig that carries its own token",
			l.dir, login.Text(), token.CreateKubeconfigRemedy(l.clusterID)))
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
