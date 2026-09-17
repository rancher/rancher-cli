package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// Replaying a credential store change is destination-first, and the reason
// is the two crash boundaries a move has. A process can die after writing
// the destination and before deleting the source, and it can die after
// deleting the source and before removing the step. Reading the source
// first cannot tell those apart: in the second the source is simply gone,
// and treating absence as success would let a step whose destination was
// never written be dropped, stranding the credential nowhere.
//
// So every replay asks the destination first. A destination that already
// holds the expected credential means the write happened, whatever else
// did; only then is an absent source "the delete already ran". A source
// that is absent while the destination is not verified is terminal: no
// retry can succeed, because the credential is in neither place the
// manifest knows about.

// transitionVerdict is what one replayed step amounts to.
type transitionVerdict int

const (
	// verdictDone: the step's work is finished and the step is removed.
	verdictDone transitionVerdict = iota
	// verdictRetain: the step could not finish now and stays for the next
	// credential-mutating command. Loud, retryable.
	verdictRetain
	// verdictTerminal: the step cannot finish at all. It stays so the
	// state remains visible, and the error is not the retryable one.
	verdictTerminal
)

// stepOutcome is a replayed step's verdict and the cause behind it.
type stepOutcome struct {
	verdict transitionVerdict
	// reason is the cause in the backend's or the step's own words,
	// without the sentence a command wraps it in. record writes it into
	// the step and stepError reads it back for every later command the
	// step stops, so the replay's warning, the refusal and `config
	// status` are one text.
	reason string
	// notices are what the operator should know about work that was
	// superseded rather than performed.
	notices []string
}

// record writes the verdict and its cause into the step. It is the one
// place a step learns what its last attempt found.
func (out stepOutcome) record(s *transitionStep) {
	s.LastError = out.reason
	s.Terminal = out.verdict == verdictTerminal
}

// retain is a step that could not finish now because of a backend error;
// the cause is the backend's own.
func retain(err error) stepOutcome {
	return stepOutcome{verdict: verdictRetain, reason: errText(err)}
}

// retainBecause is a step that could not finish now for a reason the
// replay itself established.
func retainBecause(what string) stepOutcome {
	return stepOutcome{verdict: verdictRetain, reason: what}
}

// terminalBecause is a step no retry can complete.
func terminalBecause(what string) stepOutcome {
	return stepOutcome{verdict: verdictTerminal, reason: what}
}

// backendAccess reads, writes and deletes one entry's credential in one
// physical backend. The keychain is addressed by the account the step
// recorded, never by the entry's current identity record: a replay must
// reach the slot the step was scheduled against even after a later login
// rotated the entry to a different one.
type backendAccess struct {
	dir     string
	backend string
	entry   string
	account string
	url     string
}

var errBackendAbsent = errors.New("no credential in this backend")

// get returns the credential, or errBackendAbsent when the backend holds
// none. Any other error is the backend being unreachable, which is a
// reason to retry later and never a reason to delete anything.
func (b backendAccess) get() (string, error) {
	switch b.backend {
	case backendFile:
		tok, err := newFileStore(b.dir, &noticeBuffer{}).GetToken(ServerRef{Name: b.entry, URL: b.url})
		if errors.Is(err, ErrNoToken) {
			return "", errBackendAbsent
		}
		return tok, err
	case backendKeychain:
		tok, err := keyringGet(b.account)
		if errors.Is(err, keyring.ErrNotFound) {
			return "", errBackendAbsent
		}
		return tok, err
	default:
		name, ok := execHelperName(b.backend)
		if !ok {
			return "", fmt.Errorf("unknown credential backend %q", b.backend)
		}
		tok, err := newExecStore(name, b.dir).GetToken(ServerRef{Name: b.entry, URL: b.url})
		if errors.Is(err, ErrNoToken) {
			return "", errBackendAbsent
		}
		return tok, err
	}
}

// delete removes the credential. An absent one is not a failure: the
// caller has already proved it may be removed, and a concurrent delete
// reaching it first is the same outcome.
func (b backendAccess) delete(ctx context.Context) error {
	switch b.backend {
	case backendFile:
		err := newFileStore(b.dir, &noticeBuffer{}).DeleteToken(ServerRef{Name: b.entry, URL: b.url})
		if errors.Is(err, ErrNoToken) {
			return nil
		}
		return err
	case backendKeychain:
		err := keyringDelete(b.account)
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	default:
		name, ok := execHelperName(b.backend)
		if !ok {
			return fmt.Errorf("unknown credential backend %q", b.backend)
		}
		err := newExecStore(name, b.dir).DeleteToken(ServerRef{Name: b.entry, URL: b.url})
		if errors.Is(err, ErrNoToken) {
			return nil
		}
		return err
	}
}

// accessFor builds the accessor for one side of a step.
func accessFor(dir string, s transitionStep, backend, account, entryURL string) backendAccess {
	url := entryURL
	if s.CanonicalURL != "" {
		// A helper keys its credential on the canonical URL the step was
		// scheduled with; the entry's URL may have changed since, or the
		// entry may be gone.
		url = s.CanonicalURL
	}
	return backendAccess{dir: dir, backend: backend, entry: s.Entry, account: account, url: url}
}

// matches reports whether a credential is the one this step is allowed to
// act on.
//
// A keychain side is judged by this digest alone, not also by the entry's
// identity-record fingerprint. The digest is keyed per step and was taken
// from the credential the planner read through that fingerprint check, so
// it is the stronger of the two; and the replay addresses the account the
// step recorded, which the entry's record may no longer name after a
// later login, so the record is not a fact about this slot any more.
func (s transitionStep) matches(token string) bool {
	return credentialFingerprint(s.VerificationID, token) == s.CredentialDigest
}

// stepHint is the command a stopped step points at. A move has a remedy
// of its own: the logout that turns it into cleanups, which clear on
// absence. A delete has no such conversion - it already is the cleanup -
// so it points at the status report: the next thing to look at, not
// something that clears the step.
func stepHint(s transitionStep) string {
	if s.Kind == transitionMove {
		return "rancher auth logout " + clierr.ShellQuote(s.Entry)
	}
	return "rancher config status"
}

// terminalStepError is a step that no retry can complete. It is not
// retryable; its hint is the command the step points at.
func terminalStepError(s transitionStep, what string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential store change cannot finish for %s: %s", s.Entry, what)).
		WithHint(stepHint(s))
}

// retainedStepError is a step that could not finish now. It carries the
// retryable code; the step records its cause, and stepError rebuilds this
// error from it for every later command the step stops.
//
// Some of these never clear on their own - a source another config
// directory has taken over will not come back - so the command the step
// points at rides along as the hint: for a move, the logout that turns it
// into cleanups. Retrying is free and sometimes right; when it is not,
// the way out is in the same message rather than one the operator has to
// find.
func retainedStepError(s transitionStep, what string) error {
	return clierr.Retryable(clierr.CodeCredentialTransitionIncomplete, fmt.Sprintf(
		"the credential store change could not finish for %s: %s", s.Entry, what)).
		WithHint(stepHint(s))
}

// replayMove runs one move, destination first.
//
// It never overwrites a destination that holds something else: on the
// keychain that is another config directory's credential, reached through
// an account this directory shares with a copy of itself, and the answer
// is a fresh account rather than a write over it.
func replayMove(ctx context.Context, dir string, s *transitionStep, entryURL string, setDestAccount func(string) error) stepOutcome {
	dest := accessFor(dir, *s, s.Destination, keychainAccount(s.DestAccountID, s.Entry), entryURL)
	src := accessFor(dir, *s, s.Source, keychainAccount(s.SourceAccountID, s.Entry), entryURL)

	// 0. A keychain destination is written through the same write-ahead
	// identity transaction a login uses, so a crash between the keychain
	// write and the record that settles it leaves the entry `pending`.
	// Reconcile that first: a reader meeting a pending record gets
	// LOGIN_INCOMPLETE, so finishing the move without settling it would
	// leave the credential correctly in place and the entry unusable,
	// with no step left to repair it.
	if s.Destination == backendKeychain {
		if err := reconcileMoveRecord(ctx, dir, *s); err != nil {
			return retain(err)
		}
	}

	// 1. The destination decides everything that follows.
	destTok, derr := dest.get()
	switch {
	case derr == nil && s.matches(destTok):
		// The write landed, in this pass or an earlier one.
	case derr != nil && !errors.Is(derr, errBackendAbsent):
		return retain(derr)
	default:
		destHoldsOther := derr == nil
		srcTok, serr := src.get()
		switch {
		case errors.Is(serr, errBackendAbsent):
			// Nowhere the manifest knows about holds the credential. No
			// retry can change that, so say so rather than leaving a step
			// that will fail forever. The source record is this
			// directory's own bookkeeping for a slot that is gone;
			// leaving it would read as unfinished work on every later
			// logout.
			out := terminalBecause(
				"the credential is in neither the store it was moving from nor the one it was moving to; log out this entry and log in again")
			notice, failed := retireSourceRecord(ctx, dir, *s)
			if failed != nil {
				// The verdict stands; the record it could not retire is
				// said out loud instead of being lost behind it.
				notice = failed.reason
			}
			out.notices = appendNotice(out.notices, notice)
			return out
		case serr != nil:
			return retain(serr)
		case !s.matches(srcTok):
			// The destination was never written, so nothing has moved.
			// Whatever is in the source now - another config directory's
			// credential in a shared keychain slot, or a later login's -
			// is not this change's to take, and dropping the step here
			// would leave the entry with a credential in neither place
			// and nothing to say so. It stays, where `config status`
			// shows it.
			return retainBecause(
				"the credential in the store it is moving from is not the one the change recorded")
		}

		if destHoldsOther && s.Destination == backendKeychain {
			// The destination account holds a credential this change did
			// not put there. Rotating to a fresh account is the only
			// answer that neither overwrites it nor strands this one, and
			// the new account is persisted before it is written so a
			// crash in between is addressable.
			fresh, aerr := newKeychainAccountID()
			if aerr != nil {
				return retain(aerr)
			}
			s.DestAccountID = fresh
			if perr := setDestAccount(fresh); perr != nil {
				return retain(perr)
			}
			dest = accessFor(dir, *s, s.Destination, keychainAccount(fresh, s.Entry), entryURL)
		}
		// A file destination holding something else is this directory's
		// own file, not another directory's: the move overwrites it, and
		// the read-back below is what proves the result. Only a keychain
		// slot is shared, which is why the rotation above is keychain-only.

		// 2. Write the destination and read it back. A destination that
		// does not read back as what was written is not a destination
		// this change may delete a source for.
		if werr := writeBackend(ctx, dir, *s, dest, srcTok); werr != nil {
			return retain(werr)
		}
		back, berr := dest.get()
		if berr != nil || !s.matches(back) {
			return retainBecause(
				"the store it is moving to did not keep the credential that was written to it")
		}
	}

	// 3. Re-read the source and delete it only while it still holds what
	// this change is allowed to remove.
	var notices []string
	retire := func() (stepOutcome, bool) {
		notice, failed := retireSourceRecord(ctx, dir, *s)
		if failed != nil {
			// The credential has moved, but the record that would now
			// name an empty slot is still there: the step stays, with
			// the verdict the retirement earned.
			return *failed, true
		}
		notices = appendNotice(notices, notice)
		return stepOutcome{}, false
	}
	srcTok, serr := src.get()
	switch {
	case errors.Is(serr, errBackendAbsent):
		if out, stop := retire(); stop {
			return out
		}
	case serr != nil:
		return retain(serr)
	case !s.matches(srcTok):
		if s.Source == backendKeychain {
			// The destination holds the credential, and the source slot
			// has become another copy's since: this directory's claim on
			// it is over, the move is complete, and the slot is left
			// alone. This is the one place supersession applies, because
			// the credential is provably already where it was going.
			if out, stop := retire(); stop {
				return out
			}
			break
		}
		return retainBecause(
			"the store it is moving from now holds a different credential; it was not deleted")
	default:
		if derr := src.delete(ctx); derr != nil {
			return retain(derr)
		}
		if out, stop := retire(); stop {
			return out
		}
	}

	// 4. The destination is asked once more, because the source is gone
	// now and this is the last moment the step can still be retried.
	final, ferr := dest.get()
	if ferr != nil || !s.matches(final) {
		out := retainBecause(
			"the store it moved to no longer holds the credential")
		out.notices = append(out.notices, notices...)
		return out
	}
	return stepOutcome{verdict: verdictDone, notices: notices}
}

// retireSourceRecord removes the identity record a keychain-source step
// leaves behind once its slot is gone or given up, and only when the
// record still names that slot. A record naming a different account is a
// later login's, and taking it away would leave that login's credential
// with nothing addressing it. Runs under the identity lock, like every
// other record write. Returns a notice when the record was left in place
// and nothing for a step whose source is not the keychain.
//
// A failure comes back as the outcome the step must take, decided by
// retirementOutcome and nowhere else, so every caller takes the same
// verdict for the same failure. The step is never dropped on a failure:
// its credential work is done, but a record naming an empty slot would
// read as unfinished work on every later logout, with nothing left to
// repair it.
func retireSourceRecord(ctx context.Context, dir string, s transitionStep) (notice string, failed *stepOutcome) {
	if s.Source != backendKeychain {
		return "", nil
	}
	err := withIdentityLock(ctx, dir, func() error {
		rec, err := readIdentityRecord(dir, s.Entry)
		if err != nil || rec == nil {
			return err
		}
		if rec.ID != s.SourceAccountID {
			notice = fmt.Sprintf("the credential identity for %s names a later login and was left in place", s.Entry)
			return nil
		}
		return removeIdentityRecord(dir, s.Entry)
	})
	if err == nil {
		return notice, nil
	}
	out := retirementOutcome(s, err)
	return "", &out
}

// retirementOutcome is the verdict a step takes when its source record
// could not be retired. It is the one place that verdict is decided.
//
// Terminal needs two things of the error: that it carries
// ErrCredentialIdentity, and that it is a typed error which is not
// retryable. The marker alone is not enough - its contract is "do not
// fall through to another credential source", and a login in progress
// carries it on a state a retry can resolve. A credential replaced by a
// directory sharing this one's identity carries the marker too and is
// terminal by its own verdict, which is what the second condition takes
// it from rather than from a list here. What the identity layer
// marks and also refuses to retry is a record or directory that cannot be
// trusted, which no later read sees differently; its message carries its
// own remedy, and the step's cause repeats it. Everything else - the lock
// could not be taken, the removal failed, a retryable identity state - is
// retained, since a retry may finish it.
func retirementOutcome(s transitionStep, err error) stepOutcome {
	what := fmt.Sprintf("retiring the credential identity for %s: %s", s.Entry, err.Error())
	if ce, ok := errors.AsType[*clierr.Error](err); ok && !ce.Retryable && errors.Is(err, ErrCredentialIdentity) {
		return terminalBecause(what)
	}
	return retainBecause(what)
}

// appendNotice adds a notice unless it is empty.
func appendNotice(notices []string, n string) []string {
	if n == "" {
		return notices
	}
	return append(notices, n)
}

// reconcileMoveRecord settles an identity record this move's own
// destination write left pending.
//
// It is bookkeeping, never a decision about the credential: the slot
// under the pending account either carries the token the record's
// fingerprint names, in which case the write landed and the record
// becomes active, or it does not, in which case the write did not land
// and the record goes back to what it was. The move's own verdict is
// taken afterwards, from the backends.
func reconcileMoveRecord(ctx context.Context, dir string, s transitionStep) error {
	rec, err := readIdentityRecord(dir, s.Entry)
	if err != nil || rec == nil || rec.State != identityPending || rec.ID != s.DestAccountID {
		return err
	}
	tok, gerr := keyringGet(keychainAccount(rec.ID, s.Entry))
	switch {
	case errors.Is(gerr, keyring.ErrNotFound):
		// The set never landed: back to the identity the entry had.
		return withIdentityLock(ctx, dir, func() error {
			if rec.Prior == nil {
				return removeIdentityRecord(dir, s.Entry)
			}
			return writeIdentityRecord(dir, identityRecord{
				Version: identityVersion, Entry: s.Entry, State: identityActive,
				ID: rec.Prior.ID, Fingerprint: rec.Prior.Fingerprint,
			})
		})
	case gerr != nil:
		return gerr
	case credentialFingerprint(rec.ID, tok) != rec.Fingerprint:
		// The slot holds something the record does not name, so this is
		// not the write this record was opened for. Leave it: the move's
		// own destination check below is what decides, and it never
		// overwrites a slot it cannot account for.
		return nil
	}
	return withIdentityLock(ctx, dir, func() error {
		return writeIdentityRecord(dir, identityRecord{
			Version: identityVersion, Entry: s.Entry, State: identityActive,
			ID: rec.ID, Fingerprint: rec.Fingerprint,
		})
	})
}

// writeBackend puts the credential in the destination. A keychain
// destination goes through the same write-ahead identity transaction a
// login uses, so a crash between the keychain write and the record that
// names it leaves an account the next pass can still address.
//
// Validation admits only the file store and the keychain as a move's
// destination, so those two are the whole switch.
func writeBackend(ctx context.Context, dir string, s transitionStep, dest backendAccess, token string) error {
	if dest.backend == backendFile {
		_, err := newFileStore(dir, &noticeBuffer{}).SetTokenWithOutcome(
			ServerRef{Name: dest.entry, URL: dest.url}, token, PriorCredential{})
		return err
	}
	return withIdentityLock(ctx, dir, func() error {
		prior, perr := readIdentityRecord(dir, s.Entry)
		if perr != nil {
			return perr
		}
		rec := identityRecord{
			Version: identityVersion, Entry: s.Entry, State: identityPending,
			ID: s.DestAccountID, Fingerprint: credentialFingerprint(s.DestAccountID, token),
		}
		switch {
		case prior != nil && prior.State == identityActive:
			rec.Prior = &identityPrior{ID: prior.ID, Fingerprint: prior.Fingerprint}
		case prior != nil && prior.Prior != nil:
			// A second pass over a record this move already left
			// pending: the way back is the identity the entry had
			// before the move, not the half-written one.
			rec.Prior = prior.Prior
		}
		if werr := writeIdentityRecord(dir, rec); werr != nil {
			return werr
		}
		if serr := keyringSet(dest.account, token); serr != nil {
			return serr
		}
		rec.State, rec.Prior = identityActive, nil
		return writeIdentityRecord(dir, rec)
	})
}

// replayDelete runs one conditional delete. It removes exactly the
// recorded credential and nothing else: a slot that holds something
// different is another directory's, or a later login's, and the step is
// superseded rather than performed.
func replayDelete(ctx context.Context, dir string, s transitionStep, entryURL string) stepOutcome {
	target := accessFor(dir, s, s.Source, keychainAccount(s.SourceAccountID, s.Entry), entryURL)
	tok, err := target.get()
	switch {
	case errors.Is(err, errBackendAbsent):
		notices := []string{fmt.Sprintf("the old copy of %s's credential was already gone", s.Entry)}
		notice, failed := retireSourceRecord(ctx, dir, s)
		if failed != nil {
			// The one fact worth having beside the failure is that the
			// credential itself is gone.
			failed.notices = notices
			return *failed
		}
		return stepOutcome{verdict: verdictDone, notices: appendNotice(notices, notice)}
	case err != nil:
		return retain(err)
	case !s.matches(tok):
		// Superseded: something else is there, and this change has no
		// claim on it.
		return stepOutcome{verdict: verdictDone, notices: []string{fmt.Sprintf(
			"the old copy of %s's credential was replaced since the change started; it was left alone", s.Entry)}}
	}
	if derr := target.delete(ctx); derr != nil {
		return retain(derr)
	}
	notice, failed := retireSourceRecord(ctx, dir, s)
	if failed != nil {
		return *failed
	}
	return stepOutcome{verdict: verdictDone, notices: appendNotice(nil, notice)}
}

// replayPin writes the pin the change ends with. It is last among the
// steps for a reason: under `auto` every entry that has not moved yet is
// still read through the old pin, so flipping it early would make those
// entries unreadable until their own move ran.
func replayPin(dir string, s transitionStep) stepOutcome {
	if err := WritePin(dir, s.Target); err != nil {
		return retain(err)
	}
	return stepOutcome{verdict: verdictDone}
}
