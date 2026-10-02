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
	// LookupSkipped: the call names --user-id, and the plugin login entry
	// that would answer for it records another owner, so the call neither
	// reads nor writes that entry and signs in. StoredLogin.Skipped is the
	// entry and SkippedOwner the owner it records. A lookup without a user
	// id never gives it.
	LookupSkipped
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
	// Skipped and SkippedOwner are set only for LookupSkipped.
	Skipped, SkippedOwner string

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
	return lookupStoredLogin(cfg, store, server, name, clusterID, false, entryChoice{}, userCheck{})
}

// lookupStoredLogin is the lookup the plugin's credential read and
// LookupStoredLogin share. resolved is the identification the plugin's
// connection was set up under, when it read config.yaml for it: an
// identification that differs now means the file changed under the call,
// which is refused before any credential is read. user is the call's
// --user-id: an entry whose login records another owner refuses the call
// before any request, and a plugin login entry that records another owner
// is skipped rather than read.
func lookupStoredLogin(cfg *config.Config, store CredentialReader, server, name, clusterID string, shim bool, resolved entryChoice, user userCheck) StoredLogin {
	identified, err := identifyEntry(cfg, server, name, clusterID, shim)
	if err != nil {
		return StoredLogin{Outcome: LookupRefused, Err: err, identifiedErr: true}
	}
	if resolved.known && resolved.name != identified {
		return StoredLogin{Outcome: LookupRefused, Err: configChangedError(resolved.name, identified), identifiedErr: true}
	}
	sc, ok, err := storedTokenForServer(cfg, store, server, identified, clusterID, shim, user)
	if skipped, isSkip := errors.AsType[*pluginEntrySkipped](err); isSkip {
		return StoredLogin{Outcome: LookupSkipped, Identified: identified, Skipped: skipped.entry, SkippedOwner: skipped.owner}
	}
	switch {
	case errors.Is(err, errLookupRefused):
		return StoredLogin{Outcome: LookupRefused, Identified: identified, Err: err}
	case err != nil:
		return StoredLogin{Outcome: LookupReadFailed, Identified: identified, Err: err}
	case !ok:
		// A plugin login entry the --name identifies has no fallback, and
		// the plugin-created login would write it: one that records
		// another owner is refused rather than written over.
		if s := cfg.Servers[identified]; identified != "" && cfg.TokenReuse && s.Provenance == clierr.ProvenanceExecLogin && user.otherOwner(s.TokenUserID) {
			return StoredLogin{Outcome: LookupRefused, Identified: identified, Err: user.entryOwnerRefused(cfg, identified, s.TokenUserID, true, false)}
		}
		return StoredLogin{Outcome: LookupNone, Identified: identified}
	}
	plugin := sc.entry.Provenance == clierr.ProvenanceExecLogin
	if owner := sc.entry.TokenUserID; user.otherOwner(owner) {
		switch {
		case plugin && sc.name != identified:
			// The URL's plugin login entry answering for an entry with no
			// login, or as the ranked answer: refusing would leave only
			// the removal of another user's login as the remedy.
			return StoredLogin{Outcome: LookupSkipped, Identified: identified, Skipped: sc.name, SkippedOwner: owner}
		default:
			return StoredLogin{Outcome: LookupRefused, Identified: identified, Err: user.entryOwnerRefused(cfg, sc.name, owner, plugin, true)}
		}
	}
	return StoredLogin{Outcome: LookupAnswered, Identified: identified, Entry: sc.name, PluginEntry: plugin, cred: sc}
}
