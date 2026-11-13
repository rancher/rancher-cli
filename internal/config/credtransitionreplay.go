package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/zalando/go-keyring"
)

// Replaying a credential store change runs its steps in the order they
// were recorded: the delete of the logging-in entry's old copy, then the
// pin. A delete is conditional. It removes exactly the credential whose
// digest the step carries and nothing else, and an absent credential means
// an earlier attempt, or a concurrent command, already removed it, so a
// replay after a crash at any point either finishes the step or finds it
// finished.

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

// terminalStepError is a step that no retry can complete. It is not
// retryable. Its message carries the step and the cause, and its hint is
// the report that shows the step.
func terminalStepError(s transitionStep, what string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"the credential store change cannot finish for %s: %s", s.Entry, what)).WithHintCommand(clierr.NewCommand(StatusWords))
}

// retainedStepError is a step that could not finish now. It carries the
// retryable code; the step records its cause, and stepError rebuilds this
// error from it for every later command the step stops.
func retainedStepError(s transitionStep, what string) error {
	return clierr.Retryable(clierr.CodeCredentialTransitionIncomplete, fmt.Sprintf(
		"the credential store change could not finish for %s: %s", s.Entry, what)).WithHintCommand(clierr.NewCommand(StatusWords))
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

// replayPin writes the pin the change ends with. It is the moment this
// machine's answer changes, so it is the step that says so: the pin write
// is the only place that knows both what the machine used to resolve to
// and what it resolves to now.
func replayPin(dir string, s transitionStep) stepOutcome {
	was := ReadPin(dir)
	if err := WritePin(dir, s.Target); err != nil {
		return retain(err)
	}
	if was == s.Target {
		return stepOutcome{verdict: verdictDone}
	}
	return stepOutcome{verdict: verdictDone, notices: []string{noticePinned(was, s.Target)}}
}
