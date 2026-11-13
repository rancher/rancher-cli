package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// A login that changes where the directory keeps its credentials changes
// it for its own credential only. `auth login prod --credential-store
// keychain` names one entry and a store; it says nothing about `staging`.
// Moving the other entries along would also change how they can be read:
// a keychain that an agent sandbox, an SSH session, a CI job or a
// container sharing the directory cannot open would break every entry at
// once. So the change is refused, before the sign-in, while another entry
// still holds a credential in the store being left, and each of those
// entries goes to the new store when it is logged in there itself.

// TransitionPlan is the work a store-changing login will record: the
// delete of this entry's old copy and the pin the change ends with.
type TransitionPlan struct {
	manifest transitionManifest
}

// ID is the manifest's identifier, which the config write that activates
// it records.
func (p *TransitionPlan) ID() string { return p.manifest.ID }

// transitionRefusal is a store change that cannot be performed, reported
// before authentication. A single entry gets a hint that is one runnable
// command; several get the names in the message, because no single
// command covers them.
func transitionRefusal(msg string, entries []string) error {
	err := clierr.New(clierr.CodeValidation, msg)
	if len(entries) == 1 {
		return err.WithHintCommand(LogoutHintFor(entries[0]))
	}
	return err
}

// transitionRefusalReason is one entry that holds a credential in the
// store being left, and why that stops the change. Keeping the two apart
// means the hint is built from the name rather than parsed back out of the
// sentence, which an entry name containing a bracket would break.
type transitionRefusalReason struct {
	entry string
	why   string
}

// transitionSourceError refuses a store change because one entry's
// credential in the store being left could not be read at all - an
// unreachable keychain, an unreadable credentials file. Logging that entry
// out cannot clear it, because the logout has to reach the same store, so
// the remedy is the store itself or another directory, and there is no
// hint. A store that is unavailable keeps its own code and remedy.
func transitionSourceError(entry string, err error) error {
	if ce := asStoreUnavailable(err); ce != nil {
		return ce.WithMessage(fmt.Sprintf(
			"the credential store cannot be changed while the stored credential for %s cannot be read: %s",
			entry, ce.Message))
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential store cannot be changed while the stored credential for %s cannot be read: %v; make that store reachable, or use a different --config, then run this command again",
		entry, err)).
		WithCause(err)
}

// newTransitionPlan is the empty plan a store-changing login fills in. One
// place allocates a manifest id.
func newTransitionPlan() (*TransitionPlan, error) {
	id, err := newTransitionID()
	if err != nil {
		return nil, err
	}
	return &TransitionPlan{manifest: transitionManifest{Version: transitionVersion, ID: id}}, nil
}

// CheckTransitionSources refuses a login that changes the store of its
// own credential from `from` to `to` while any other entry holds a
// credential in `from`, or has one left in `to`. "Holds" is every answer
// but absent: a login that never settled, a keychain slot another
// directory owns and a store that cannot be read refuse too. What is left
// in `to` counts because every reader moves there with the change: an
// entry logged out while its keychain copy could not be reached keeps
// that copy and its identity record, and after the change a reader of
// that entry would be served the credential the operator logged out.
//
// A login runs it before the sign-in, so the answer arrives before the
// operator authenticates, and again under the lock, where it is the
// authority; while an earlier change has steps left, only the lock runs
// it, after the replay. It takes no lock and writes nothing.
func CheckTransitionSources(dir string, cfg *Config, loginEntry, from, to string) error {
	if !isPhysicalBackend(from) {
		return fmt.Errorf("a credential store change needs a physical backend to leave, not %q", from)
	}
	others := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		if name != loginEntry {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	if len(others) == 0 {
		return nil
	}

	if helper, ok := execHelperName(from); ok {
		// A helper's `get` may answer for any entry - provisioned outside
		// this CLI, or created on demand - and a helper that creates
		// credentials keeps no record of them, so no read here can say
		// which entries it serves. Every other entry counts. Logging one
		// out does not stop the helper answering for it, so the remedies
		// are another directory for the new store or removing the entry.
		return clierr.New(clierr.CodeValidation, removeEntriesText(fmt.Sprintf(
			"a login changes the store of its own credential only, and the credential helper %s may answer for %s, which would then read a store that holds nothing for %s; keep this directory on the helper and use a different --config for the new store, or remove %s",
			helper, strings.Join(others, ", "), pronounFor(others), pronounFor(others)), others))
	}
	if err := helperDestinationRefusal(to, others); err != nil {
		return err
	}

	refusals, err := credentialsHeldIn(dir, from, others)
	if err != nil {
		return err
	}
	left, err := credentialsLeftIn(dir, to, others)
	if err != nil {
		return err
	}
	refusals = append(refusals, left...)
	if len(refusals) == 0 {
		return nil
	}
	why := make([]string, 0, len(refusals))
	names := make([]string, 0, len(refusals))
	for _, r := range refusals {
		why = append(why, r.why)
		names = append(names, r.entry)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	remedy := logOutFirst(names)
	if to == backendKeychain && len(left) > 0 {
		remedy += " with the keychain reachable"
	}
	return transitionRefusal(fmt.Sprintf(
		"a login changes the store of its own credential only, and %s; %s, then run this command again",
		strings.Join(why, "; "), remedy), names)
}

// credentialsLeftIn names every entry that has a credential in the store a
// change moves the directory's readers to, when that store is not a
// helper. For the keychain it is the identity records: a record exists
// only for a keychain credential, and its name is enough, without opening
// it or the slot. For the file store it is a key in credentials.yaml.
func credentialsLeftIn(dir, to string, entries []string) ([]transitionRefusalReason, error) {
	var left []transitionRefusalReason
	switch to {
	case backendKeychain:
		records, err := identityRecordNames(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if records[identityFileName(entry)] {
				left = append(left, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the keychain still holds a credential of %s from an earlier login, which a reader of %s would be served after the change", entry, entry)})
			}
		}
	case backendFile:
		tokens, err := newFileStore(dir, &noticeBuffer{}).read()
		if err != nil {
			return nil, unreadableCredentialsFile(dir, err)
		}
		for _, entry := range entries {
			if _, ok := tokens[entry]; ok {
				left = append(left, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the plaintext file %s still holds a credential of %s from an earlier login, which a reader of %s would be served after the change", CredentialsFilePath(dir), entry, entry)})
			}
		}
	default:
		return nil, fmt.Errorf("unknown credential backend %q", to)
	}
	return left, nil
}

// CheckHelperDestination is the destination half of CheckTransitionSources
// for a login that sends the directory's readers to a credential helper
// from no store at all - an unpinned directory that has stored nothing -
// where there is no source to read but every other entry still counts.
func CheckHelperDestination(cfg *Config, loginEntry, to string) error {
	others := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		if name != loginEntry {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	return helperDestinationRefusal(to, others)
}

// helperDestinationRefusal refuses a change into a credential helper while
// any other entry exists. It is the helper-source rule from the other
// side: a helper that creates credentials on demand keeps no record and
// answers `get` for any entry, so after the change every other entry - one
// that was logged out included - would be served a credential the helper
// created and it never logged in with. Every other entry counts. nil when
// to is not a helper or there is no other entry.
func helperDestinationRefusal(to string, others []string) error {
	helper, ok := execHelperName(to)
	if !ok || len(others) == 0 {
		return nil
	}
	each := others[0]
	if len(others) > 1 {
		each = "each of them"
	}
	return clierr.New(clierr.CodeValidation, removeEntriesText(fmt.Sprintf(
		"a login changes the store of its own credential only, and the credential helper %s may answer for %s: after the change %s would be served a credential it never logged in with; keep this directory where it is and use a different --config for the helper, or remove %s",
		helper, strings.Join(others, ", "), each, pronounFor(others)), others))
}

// pronounFor refers back to the entries a refusal names, in the number the
// list has.
func pronounFor(names []string) string {
	if len(names) == 1 {
		return "it"
	}
	return "them"
}

// removeEntriesText is the prose that ends with the request to remove the
// named entries, then the command that removes each one, one command per
// entry in the order given and separated by "; ", then the request to run the
// command again. A command that cannot be printed is left out alone; when
// none can be printed the text carries the note and no command.
func removeEntriesText(prose string, names []string) string {
	deletes := make([]clierr.Command, 0, len(names))
	for _, name := range names {
		deletes = append(deletes, clierr.NewCommand(DeleteServerWords).Arg(name))
	}
	parts := slices.Concat([]any{prose}, commandListParts(" with: ", deletes), []any{"; then run this command again"})
	return clierr.Text(parts...)
}

// commandListParts is the parts of a clierr.Text for independent commands:
// the lead-in before the first command that can be printed, each of those
// separated by "; ", then each command that cannot be printed, which Text
// leaves out and notes.
func commandListParts(lead clierr.Lead, commands []clierr.Command) []any {
	var parts, left []any
	for _, c := range commands {
		switch {
		case !c.OK():
			left = append(left, c)
		case len(parts) == 0:
			parts = append(parts, lead, c)
		default:
			parts = append(parts, "; ", c)
		}
	}
	return slices.Concat(parts, left)
}

// credentialsHeldIn reads each entry's credential in one physical store
// that is not a helper, and names every entry that holds one there.
func credentialsHeldIn(dir, from string, entries []string) ([]transitionRefusalReason, error) {
	var refusals []transitionRefusalReason
	switch from {
	case backendFile:
		tokens, err := newFileStore(dir, &noticeBuffer{}).read()
		if err != nil {
			return nil, unreadableCredentialsFile(dir, err)
		}
		for _, entry := range entries {
			if _, ok := tokens[entry]; ok {
				refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the credential of %s is still in the plaintext file %s", entry, CredentialsFilePath(dir))})
			}
		}
	case backendKeychain:
		for _, entry := range entries {
			rec, err := readIdentityRecord(dir, entry)
			switch {
			case err != nil:
				return nil, err
			case rec == nil:
				// Nothing was ever stored for this entry under this
				// directory's identity.
				continue
			case rec.State == identityPending:
				refusals = append(refusals, transitionRefusalReason{entry: entry,
					why: fmt.Sprintf("a login for %s did not finish", entry)})
				continue
			}
			tok, gerr := keyringGet(rec.Account())
			switch {
			case errors.Is(gerr, keyring.ErrNotFound):
				continue
			case gerr != nil:
				return nil, transitionSourceError(entry, keychainError("get", entry, gerr, orAnotherConfig))
			case credentialFingerprint(rec.ID, tok) != rec.Fingerprint:
				// Another config directory sharing this identity owns the
				// slot now; the logout removes this directory's claim on
				// it without touching the slot.
				refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the stored credential for %s belongs to another config directory sharing this one's identity", entry)})
				continue
			}
			refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
				"the credential of %s is still in the keychain", entry)})
		}
	default:
		return nil, fmt.Errorf("unknown credential backend %q", from)
	}
	return refusals, nil
}

// logOutFirst is a refusal's remedy for the entries it names, in the
// number the list has.
func logOutFirst(names []string) string {
	if len(names) == 1 {
		return "log that entry out first"
	}
	return "log those entries out first"
}

// AddOldCopyDelete records the delete of the logging-in entry's credential
// in the backend it is leaving. It is added after the credential is known,
// because the digest is what limits the delete to that exact credential:
// a later login's token in the same place is not this change's to remove.
func (p *TransitionPlan) AddOldCopyDelete(entry, from, token, url, accountID string) error {
	if p == nil || token == "" {
		return nil
	}
	vid, err := newTransitionID()
	if err != nil {
		return err
	}
	step := transitionStep{
		Kind: transitionDelete, Entry: entry, Source: from,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, token),
		SourceAccountID: accountID,
	}
	if isExecBackend(from) {
		step.CanonicalURL = url
	}
	p.manifest.Steps = append(p.manifest.Steps, step)
	return nil
}

// AddPin records the pin the change ends with, after the old copy's
// delete.
func (p *TransitionPlan) AddPin(target string) {
	if p == nil {
		return
	}
	p.manifest.Steps = append(p.manifest.Steps, transitionStep{Kind: transitionPin, Target: target})
}

// Write persists the manifest. It is inert until a config write names its
// id, so a login that fails after this and before that leaves nothing to
// finish.
func (p *TransitionPlan) Write(dir string) error {
	if p == nil || len(p.manifest.Steps) == 0 {
		return nil
	}
	return writeTransition(dir, p.manifest)
}

// Empty reports whether the plan has no work, in which case the login
// records no change at all.
func (p *TransitionPlan) Empty() bool { return p == nil || len(p.manifest.Steps) == 0 }
