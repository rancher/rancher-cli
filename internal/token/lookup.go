package token

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// CredentialReader reads one entry's stored login credential: the one
// store call the exec plugin's lookup makes. config.CredStore is one.
type CredentialReader interface {
	GetToken(ref config.ServerRef) (string, error)
}

// LookupOutcome is what the exec plugin's lookup gives a call.
type LookupOutcome int

const (
	// LookupNone: no stored login answers for the call, so the plugin
	// signs in (or, with no terminal, names the ways to).
	LookupNone LookupOutcome = iota
	// LookupAnswered: the stored login of StoredLogin.Entry answers.
	LookupAnswered
	// LookupRefused: the plugin refuses the call - the entry its --name
	// identifies is registered at another URL, several entries tie, or
	// several plugin login entries answer. StoredLogin.Err is the refusal.
	LookupRefused
	// LookupReadFailed: a store read failed, so the plugin fails with it
	// until it can read. StoredLogin.Err is the failure.
	LookupReadFailed
)

// StoredLogin is the exec plugin's lookup for one call, as the plugin
// makes it: the entry the call's --name identifies, and the entry whose
// stored login credential answers for it, or why none does.
type StoredLogin struct {
	Outcome LookupOutcome
	// Identified is the entry --name identifies; "" for none, or when the
	// identification itself refused the call.
	Identified string
	// Entry is the entry whose stored login answers: the identified
	// entry's own, the exec plugin's own login entry, or the winner among
	// the entries for the URL. Set only for LookupAnswered.
	Entry string
	// PluginEntry reports that Entry is a login entry the exec plugin
	// created.
	PluginEntry bool
	// Err is the refusal or the read failure.
	Err error

	cred storedCredential
	// identifiedErr: the identification refused the call, before any
	// credential was read.
	identifiedErr bool
}

// errLookupRefused marks the lookup's own refusals, so the outcome can
// tell them from a store failure that passes through.
var errLookupRefused = errors.New("the exec plugin's lookup refuses this call")

// LookupStoredLogin is the exec plugin's lookup for a call from a
// kubeconfig exec entry with --server server, --cluster clusterID and
// --name name, over cfg and the store the plugin opens. It reads only
// what the plugin reads, and no network.
func LookupStoredLogin(cfg *config.Config, store CredentialReader, server, name, clusterID string) StoredLogin {
	return lookupStoredLogin(cfg, store, server, name, clusterID, false, entryChoice{})
}

// lookupStoredLogin is the lookup the plugin's credential read and
// LookupStoredLogin share. resolved is the identification the plugin's
// connection was set up under, when it read config.yaml for it: an
// identification that differs now means the file changed under the call,
// which is refused before any credential is read.
func lookupStoredLogin(cfg *config.Config, store CredentialReader, server, name, clusterID string, shim bool, resolved entryChoice) StoredLogin {
	identified, err := identifyEntry(cfg, server, name, clusterID, shim)
	if err != nil {
		return StoredLogin{Outcome: LookupRefused, Err: err, identifiedErr: true}
	}
	if resolved.known && resolved.name != identified {
		return StoredLogin{Outcome: LookupRefused, Err: configChangedError(resolved.name, identified), identifiedErr: true}
	}
	sc, ok, err := storedTokenForServer(cfg, store, server, identified, clusterID, shim)
	switch {
	case errors.Is(err, errLookupRefused):
		return StoredLogin{Outcome: LookupRefused, Identified: identified, Err: err}
	case err != nil:
		return StoredLogin{Outcome: LookupReadFailed, Identified: identified, Err: err}
	case !ok:
		return StoredLogin{Outcome: LookupNone, Identified: identified}
	}
	return StoredLogin{Outcome: LookupAnswered, Identified: identified, Entry: sc.name,
		PluginEntry: sc.entry.Provenance == clierr.ProvenanceExecLogin, cred: sc}
}
