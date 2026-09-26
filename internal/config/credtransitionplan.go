package config

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// A login that changes the directory's credential store does not log the
// other entries out. It moves them.
//
// Logging them out would destroy logins the operator never named: someone
// who runs `auth login prod --credential-store file` on a directory that
// also holds `staging` has said nothing about staging, and finding it
// logged out afterwards is a surprise with a real cost. Moving the
// credential leaves it usable, leaves its exec-credential cache valid -
// the login it belongs to has not changed, so its cache binding id does
// not advance - and needs no gate, because nothing is destroyed.
//
// What can move is decided per source, before the operator is asked to
// authenticate, so the answer to "this cannot be done" arrives before the
// sign-in rather than after it.

// TransitionPlan is the work a store-changing login will record.
type TransitionPlan struct {
	manifest transitionManifest
	// Moved names the entries whose credentials the change will move, for
	// the notice the login gives.
	Moved []string
	// PrePin is the pin to write before the commit, or empty. It is only
	// ever written while the declaration is one no reader resolves
	// through the pin. A directory declared `file` never reads the pin,
	// so one leaving the file store may have none; writing `file` first
	// states what is already true, stays truthful if the login fails, and
	// makes a change into `auto` the same case as a change away from a
	// `file` pin. The login also sets it to the store it commits to when
	// the pin as it stands names a third store.
	PrePin string
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
		return err.WithHint("rancher auth logout " + clierr.ShellQuote(entries[0]))
	}
	return err
}

// transitionRefusalReason is one entry that cannot move and why. Keeping
// the two apart means the hint is built from the name rather than parsed
// back out of the sentence, which an entry name containing a bracket
// would break.
type transitionRefusalReason struct {
	entry string
	why   string
}

// transitionSourceError refuses a store change because one entry's
// credential in the backend being left could not be read at all - an
// unreachable keychain, an unreadable credentials file, a helper that did
// not answer. It is the same refusal shape as an entry that cannot move:
// typed, naming the entry, with the logout that removes it from the
// change as the hint, and the backend's error as the cause.
func transitionSourceError(entry string, err error) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential store cannot be changed while the stored credential for %s cannot be read: %v; make that store reachable, or log the entry out first, then run this command again",
		entry, err)).
		WithHint("rancher auth logout " + clierr.ShellQuote(entry)).
		WithCause(err)
}

// PlanTransition builds the manifest for a login that moves this directory
// from one physical backend to another.
//
// loginEntry is the entry being logged in: its new credential goes to the
// destination through the login's own commit, so the plan carries only the
// delete of its old copy. Every other entry gets a move.
//
// from and to are physical backends, resolved by the caller before this is
// called: a step must never carry the policy `auto`, or a pin written
// later could retarget work already scheduled.
func PlanTransition(ctx context.Context, dir string, cfg *Config, loginEntry, from, to, pin string) (*TransitionPlan, error) {
	if from == to {
		return nil, nil
	}
	movable, err := transitionSources(ctx, dir, cfg, loginEntry, from, to)
	if err != nil {
		return nil, err
	}

	plan, err := newTransitionPlan()
	if err != nil {
		return nil, err
	}
	for _, c := range movable {
		step, serr := moveStep(c, from, to)
		if serr != nil {
			return nil, serr
		}
		plan.manifest.Steps = append(plan.manifest.Steps, step)
		plan.Moved = append(plan.Moved, c.entry)
	}
	if pin == "" && from == backendFile {
		plan.PrePin = backendFile
	}
	return plan, nil
}

// newTransitionPlan is the empty plan: the starting point PlanTransition
// fills in, and the whole plan of a change that moves nothing and has
// only the pin it ends with. One place allocates a manifest id.
func newTransitionPlan() (*TransitionPlan, error) {
	id, err := newTransitionID()
	if err != nil {
		return nil, err
	}
	return &TransitionPlan{manifest: transitionManifest{Version: transitionVersion, ID: id}}, nil
}

// CheckTransitionSources reads every entry a store change would move and
// reports the refusal it would raise, without allocating a manifest id,
// building a step or writing anything.
//
// A login runs it before the sign-in so the answer to "this cannot be
// done" arrives before the operator authenticates rather than after. It
// takes no lock, so its answer can be stale; the plan built under the
// lock runs the same reads and is the authority.
func CheckTransitionSources(ctx context.Context, dir string, cfg *Config, loginEntry, from, to string) error {
	if from == to {
		return nil
	}
	_, err := transitionSources(ctx, dir, cfg, loginEntry, from, to)
	return err
}

// transitionSources is the read half of planning: every other entry's
// credential in the backend being left, or the refusal that says why the
// change cannot be made. Both the pre-authentication check and the plan
// call it, so neither can answer a question the other would answer
// differently.
func transitionSources(ctx context.Context, dir string, cfg *Config, loginEntry, from, to string) ([]movableCredential, error) {
	if !isPhysicalBackend(from) || !isPhysicalBackend(to) {
		return nil, fmt.Errorf("a credential store change needs two physical backends, not %q and %q", from, to)
	}

	others := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		if name != loginEntry {
			others = append(others, name)
		}
	}
	slices.Sort(others)

	movable, refusals, err := movableCredentials(ctx, dir, cfg, from, to, others)
	if err != nil {
		return nil, err
	}
	if len(refusals) > 0 {
		why := make([]string, 0, len(refusals))
		names := make([]string, 0, len(refusals))
		for _, r := range refusals {
			why = append(why, r.why)
			names = append(names, r.entry)
		}
		slices.Sort(names)
		names = slices.Compact(names)
		return nil, transitionRefusal(fmt.Sprintf(
			"the credential store cannot be changed while %s; %s, then run this command again",
			strings.Join(why, "; "), logOutFirst(names)), names)
	}
	// A credential helper cannot receive a change: `store` cannot be
	// probed without storing, and `get` may create a credential rather
	// than return the one it was given, so no replay could ever prove the
	// move landed. This is the one place moving yields to a refusal.
	if isExecBackend(to) && len(movable) > 0 {
		names := make([]string, 0, len(movable))
		for _, c := range movable {
			names = append(names, c.entry)
		}
		return nil, transitionRefusal(fmt.Sprintf(
			"a credential helper cannot receive %s: the helper protocol cannot confirm what it stored; %s, then run this command again",
			credentialsOf(names), logOutFirst(names)), names)
	}
	// The plaintext file store receives no other entry's credential. A
	// login names one entry, and moving every other entry out of the
	// keychain or a helper would lower their protection on an answer
	// about that one; each goes to the file store only when it is logged
	// in there itself. Nothing ever moves into the file store as a
	// result, so no reader can meet a leftover token there while a move
	// is still pending.
	if to == backendFile && len(movable) > 0 {
		names := make([]string, 0, len(movable))
		for _, c := range movable {
			names = append(names, c.entry)
		}
		verb := "are"
		if len(names) == 1 {
			verb = "is"
		}
		return nil, transitionRefusal(fmt.Sprintf(
			"the plaintext file store takes only the credential of the entry logging in, and %s %s still in %s; %s, then run this command again",
			credentialsOf(names), verb, from, logOutFirst(names)), names)
	}
	return movable, nil
}

// credentialsOf names the credentials of the entries a refusal is about,
// in the number the list has.
func credentialsOf(names []string) string {
	if len(names) == 1 {
		return "the credential of " + names[0]
	}
	return "the credentials of " + strings.Join(names, ", ")
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

// AddPin records the pin the change ends with. It is last among the steps:
// under `auto` every entry that has not moved yet is still read through
// the old pin, so flipping it earlier would make those entries unreadable
// until their own move ran.
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

// movableCredential is one entry's credential in the backend the directory
// is leaving.
type movableCredential struct {
	entry     string
	token     string
	accountID string
	url       string
}

// movableCredentials reads every other entry's credential from the backend
// being left, and reports the ones that cannot move.
//
// Reading each source once, before authentication, is what lets the
// operator decide before anything changes: a keychain slot another
// directory now owns, a login that never settled, a helper credential the
// protocol cannot move - all of them are answered here rather than
// halfway through.
func movableCredentials(ctx context.Context, dir string, cfg *Config, from, to string, entries []string) ([]movableCredential, []transitionRefusalReason, error) {
	var (
		movable  []movableCredential
		refusals []transitionRefusalReason
	)
	// A credential helper's namespace is the helper's own, and `get` may
	// answer for credentials this CLI never stored - provisioned outside
	// it, or created on demand. Only the entries the helper records name
	// for this helper are the directory's to move; every other answer is
	// left where it is.
	var recorded map[string]bool
	if name, ok := execHelperName(from); ok {
		recorded = map[string]bool{}
		if err := withHelperRecordsLock(ctx, dir, func(st *helperRecords) error {
			for _, e := range st.Entries {
				if e.Helper == name {
					recorded[e.Name] = true
				}
			}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	for _, entry := range entries {
		url := cfg.Servers[entry].URL
		switch from {
		case backendFile:
			tok, err := newFileStore(dir, &noticeBuffer{}).GetToken(ServerRef{Name: entry, URL: url})
			switch {
			case errors.Is(err, ErrNoToken):
			case err != nil:
				return nil, nil, transitionSourceError(entry, err)
			default:
				held := false
				if to == backendKeychain {
					var herr error
					if held, herr = keychainCredentialHeld(dir, entry); herr != nil {
						return nil, nil, transitionSourceError(entry, herr)
					}
				}
				if held {
					// The entry has a credential in both stores, and only
					// one of them can be the one this directory keeps. A
					// keychain account is addressable state: writing a
					// second one for the same entry replaces the record
					// that names the first and leaves what it holds
					// reachable by nothing. So the entry is named and the
					// logout that settles it is the remedy, rather than
					// this code deciding which of the two is real.
					//
					// The mirror case does not arise: a change into the
					// file store moves no other entry at all.
					refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
						"the credential for %s is stored in both the store being left and the one it would move to", entry)})
					continue
				}
				movable = append(movable, movableCredential{entry: entry, token: tok, url: url})
			}
		case backendKeychain:
			rec, err := readIdentityRecord(dir, entry)
			switch {
			case err != nil:
				return nil, nil, err
			case rec == nil:
				// Nothing was ever stored for this entry under this
				// directory's identity, so there is nothing to move.
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
				return nil, nil, transitionSourceError(entry, gerr)
			case credentialFingerprint(rec.ID, tok) != rec.Fingerprint:
				// Another config directory sharing this identity owns the
				// slot now. It stays where it is, and this directory stops
				// claiming it.
				refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the stored credential for %s belongs to another config directory sharing this one's identity", entry)})
				continue
			}
			movable = append(movable, movableCredential{entry: entry, token: tok, accountID: rec.ID, url: url})
		default:
			name, ok := execHelperName(from)
			if !ok {
				return nil, nil, fmt.Errorf("unknown credential backend %q", from)
			}
			if !recorded[entry] {
				continue
			}
			store := newExecStore(name, dir)
			tok, gerr := store.GetToken(ServerRef{Name: entry, URL: url})
			switch {
			case errors.Is(gerr, ErrNoToken):
				continue
			case gerr != nil:
				return nil, nil, transitionSourceError(entry, gerr)
			}
			if _, short := store.ShortLivedTokenExpiry(ServerRef{Name: entry, URL: url}); short {
				// The helper created this credential on the way out rather
				// than returning one it stored, so there is nothing to
				// move and nothing that would still be there afterwards.
				refusals = append(refusals, transitionRefusalReason{entry: entry, why: fmt.Sprintf(
					"the credential helper creates %s's credential on demand rather than storing it", entry)})
				continue
			}
			movable = append(movable, movableCredential{entry: entry, token: tok, url: url})
		}
	}
	return movable, refusals, nil
}

// keychainCredentialHeld reports whether this directory still addresses a
// keychain credential for the entry: a record naming an account, and a
// slot holding the credential that record's fingerprint names. A record
// whose slot is gone, or that now holds another directory's credential,
// is not something this directory can lose.
func keychainCredentialHeld(dir, entry string) (bool, error) {
	rec, err := readIdentityRecord(dir, entry)
	if err != nil || rec == nil {
		return false, err
	}
	tok, gerr := keyringGet(rec.Account())
	switch {
	case errors.Is(gerr, keyring.ErrNotFound):
		return false, nil
	case gerr != nil:
		return false, gerr
	}
	return credentialFingerprint(rec.ID, tok) == rec.Fingerprint, nil
}

// moveStep records one entry's move with the verifier that limits it to
// exactly the credential read above.
func moveStep(c movableCredential, from, to string) (transitionStep, error) {
	vid, err := newTransitionID()
	if err != nil {
		return transitionStep{}, err
	}
	step := transitionStep{
		Kind: transitionMove, Entry: c.entry, Source: from, Destination: to,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, c.token),
		SourceAccountID: c.accountID,
	}
	if to == backendKeychain {
		account, aerr := newKeychainAccountID()
		if aerr != nil {
			return transitionStep{}, aerr
		}
		step.DestAccountID = account
	}
	if isExecBackend(from) || isExecBackend(to) {
		step.CanonicalURL = c.url
	}
	return step, nil
}
