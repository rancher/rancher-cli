package config

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// MutationIntent is what a command is about to do to the directory's
// credentials, which decides what it may do while a store change still has
// work left.
type MutationIntent struct {
	// ChangesStore is true for a login that would declare a different
	// store, change the pin, or schedule a change of its own. Those cannot
	// stack: a second change would forget the first, and an old copy the
	// first had still to delete would be left in a backend nothing names
	// any more.
	ChangesStore bool
}

// PrepareMutation is the prelude every credential-mutating command runs:
// retry the directory's recorded store change, then decide whether this
// command may proceed.
//
// It lives here rather than in each command because the plugin-created
// login reaches it too, through the same login primitive: a kubectl exec
// call may therefore replay a change or be refused with the retryable
// error, which is a typed error with a remedy and not a prompt, and a bare
// install never has a manifest to be refused on.
//
// The caller holds the config lock. cfg is the config loaded under it and
// may be updated here; the caller writes it. A change whose last step
// finished - before this call or during it - has its name cleared in cfg,
// so the caller's config writes forget it.
func PrepareMutation(ctx context.Context, dir string, cfg *Config, intent MutationIntent) (notices []string, err error) {
	return prepareMutation(ctx, dir, cfg, intent, func(fn func() error) error {
		return withTransitionLock(ctx, dir, fn)
	})
}

// PrepareCredentialDelete is the prelude of a command that deletes an
// entry's credential and never changes the store - auth logout, config
// delete-server, and config update-server when a URL change clears the
// credential. It runs after the command's own validation and before its
// first write. No remaining step refuses it: a delete step removes only
// the credential whose digest it carries, which the command's own
// credential is not, and a pin step names the store the change already
// committed to. A record that cannot be read does, with nothing deleted
// and nothing written. The notices of the work it did ride on its error,
// since a command that stops here returns nothing else.
func PrepareCredentialDelete(ctx context.Context, dir string, cfg *Config) ([]string, error) {
	notices, err := PrepareMutation(ctx, dir, cfg, MutationIntent{})
	return notices, WithNotices(err, notices)
}

// prepareMutationLocked is PrepareMutation for a caller that already holds
// the transition lock. The login primitive holds it around its whole body,
// and withDirLock is not reentrant: a nested take would wait on this very
// process until the lock deadline and then fail as a timeout.
func prepareMutationLocked(ctx context.Context, dir string, cfg *Config, intent MutationIntent) (notices []string, err error) {
	return prepareMutation(ctx, dir, cfg, intent, func(fn func() error) error { return fn() })
}

// prepareMutation is the prelude's body. locked runs its argument with the
// transition lock held, however the caller arranges that, so the order of
// the work is written once and cannot differ between a caller that takes
// the lock and one that already holds it.
func prepareMutation(
	ctx context.Context, dir string, cfg *Config, intent MutationIntent,
	locked func(func() error) error,
) (notices []string, err error) {
	var (
		retained  []string
		remaining *transitionManifest
	)
	state, m, lerr := LoadTransition(dir, cfg)
	if lerr != nil {
		return nil, lerr
	}
	switch state {
	case TransitionNone:
		return nil, nil
	case TransitionFinished:
		// Nothing is left, and a name left behind would make the next
		// change record - written by a login that dies before naming it -
		// read as a change this directory does not name, which refuses
		// every credential command, instead of as work that never started.
		// No lock is needed: a new change is recorded only under the
		// config lock the caller holds.
		cfg.CredentialTransitionID = ""
		return nil, nil
	case TransitionAbandoned:
		// A login died between preparing a change and activating it, so
		// none of its work was ever done. Removing it is what lets the next
		// store change start.
		return nil, locked(func() error { return removeTransition(dir) })
	}

	err = locked(func() error {
		// Re-read under the lock: another process may have finished the
		// work between the check above and here.
		fresh, rerr := readTransition(dir)
		if rerr != nil {
			return rerr
		}
		if fresh == nil {
			// Finished by another process since the check above.
			cfg.CredentialTransitionID = ""
			return nil
		}
		if fresh.ID != m.ID {
			// Replaced by a change this command never loaded, which is
			// reachable only with the flock skipped. A manifest this pass
			// did not settle is deliberately left to the command that owns
			// it rather than refused on here, and with the flock skipped
			// that is the same lost-update residual withTransitionLock
			// already documents.
			return nil
		}
		m = fresh
		ns, left, rerr := replayPass(ctx, dir, cfg, m)
		notices = append(notices, ns...)
		retained = left
		switch {
		case rerr != nil:
		case len(m.Steps) > 0:
			// What the pass left is decided here, under the lock that
			// settled it, rather than read back from disk afterwards.
			remaining = m
		default:
			// The pass finished the change and removed its record.
			cfg.CredentialTransitionID = ""
		}
		return rerr
	})
	if err != nil {
		return notices, err
	}
	notices = append(notices, retained...)
	return notices, refusedBy(remaining, intent)
}

// stepError is the error a command stopped by a remaining step reports,
// rebuilt from what the step recorded so the replay's warning and the
// refusal say the same thing. A step whose last attempt was terminal
// keeps the verdict it earned - a non-retryable error. A step whose last
// attempt could not finish carries the retryable code, because some of
// those never clear on their own. A step no attempt has judged yet says
// only that work is left, and so does an attempted step that names no
// entry - the pin - since its subject would be empty; its cause still
// rides in the message. Every one names the status report as its hint:
// no other command finishes a step. An agent that branches on retryable
// must never be told to retry a state no retry can change.
func stepError(step transitionStep) error {
	switch {
	case step.Terminal:
		return terminalStepError(step, step.LastError)
	case step.LastError != "" && step.Entry != "":
		return retainedStepError(step, step.LastError)
	}
	return transitionIncomplete(step)
}

// stepWarning is a step's error as the warning the replay leaves behind:
// the message, then the command its hint names. A refusal carries the
// hint as a typed field, but a command that goes on past the step - a
// logout - has only this line to point anywhere.
func stepWarning(step transitionStep) string {
	err := stepError(step)
	if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Hint != "" {
		return err.Error() + "; hint: " + ce.Hint
	}
	return err.Error()
}

// refusedBy is the merge rule: given the steps a directory still has, may
// a command with this intent go on. A store change never may - two changes
// cannot stack. Any other command may: a remaining delete removes only the
// credential whose digest it carries, which the command's own is not, and
// a remaining pin names the store the change already committed to. The
// step that refuses reports with the verdict it earned: terminal steps
// keep their own error, retained ones the retryable code.
func refusedBy(m *transitionManifest, intent MutationIntent) error {
	if m == nil || len(m.Steps) == 0 || !intent.ChangesStore {
		return nil
	}
	return stepError(m.Steps[0])
}

// transitionStepsLeft is the question a login asks before the sign-in
// about an earlier store change: does the directory have steps left that
// the locked prelude will replay. It takes no lock, replays nothing and
// writes nothing. Every step is the lock's to decide: the replay can finish
// it, and only once it has run can the transition refusal or the store
// rules be judged on the directory as the replay left it - a check made
// before the replay sees the directory before that, and would refuse what
// the lock then accepts. A change record that cannot be read is the one
// answer given here, as an error: the lock refuses on it for certain, and
// there is nothing for the replay to do.
func transitionStepsLeft(dir string, cfg *Config) (bool, error) {
	if !HasPendingTransition(dir, cfg) {
		return false, nil
	}
	state, m, err := LoadTransition(dir, cfg)
	if err != nil {
		return false, err
	}
	return state == TransitionActive && m != nil && len(m.Steps) > 0, nil
}

// replayPass runs the remaining steps in order, removing each one under
// the lock only after its postcondition holds.
//
// A failing step stops the pass before every later step, so the change
// finishes in the order it was recorded and the next command resumes where
// this one stopped.
//
// A step that could not finish - now, or ever - is reported as a
// warning, not as this function's error: whether the calling command may
// go on is the caller's question, and for a logout the answer is yes. The
// error return is for failures writing the manifest itself.
func replayPass(ctx context.Context, dir string, cfg *Config, m *transitionManifest) (notices, retained []string, err error) {
	for len(m.Steps) > 0 {
		done, ns, left, rerr := replayStepAt(ctx, dir, cfg, m, 0)
		notices = append(notices, ns...)
		if rerr != nil {
			return notices, nil, rerr
		}
		if !done {
			return notices, left, nil
		}
	}
	// Unreachable: a manifest with no steps does not validate, so the
	// loop always exits through replayStepAt's last-step branch, which
	// removes the manifest and leaves no steps behind.
	return notices, nil, nil
}

// replayStepAt performs the step at index i and settles the manifest: a
// completed step is removed and the manifest rewritten, or deleted when
// it was the last one; a step that could not finish keeps the cause and
// the verdict this attempt earned, so the next command reports what to
// fix. The caller holds the transition lock.
//
// retained is the warning for a step that stayed. Terminal and retained
// are both work the directory still has, and neither is this function's
// decision to act on: a step that can never finish for one entry is no
// more reason to refuse a command for another than one that merely could
// not finish now. Both come out as warnings, each in its own words and
// with the command it points at, and the caller's own rules decide who is
// stopped. The error return is for failures writing the manifest itself.
func replayStepAt(ctx context.Context, dir string, cfg *Config, m *transitionManifest, i int) (done bool, notices, retained []string, err error) {
	s := m.Steps[i]
	var out stepOutcome
	switch s.Kind {
	case transitionDelete:
		out = replayDelete(ctx, dir, s, entryURLFor(cfg, s.Entry))
	case transitionPin:
		out = replayPin(dir, s)
	}
	notices = append(notices, out.notices...)
	if out.verdict != verdictDone {
		out.record(&m.Steps[i])
		if werr := writeTransition(dir, *m); werr != nil {
			return false, notices, nil, werr
		}
		return false, notices, []string{stepWarning(m.Steps[i])}, nil
	}
	m.Steps = slices.Delete(slices.Clone(m.Steps), i, i+1)
	if len(m.Steps) == 0 {
		return true, notices, nil, removeTransition(dir)
	}
	if werr := writeTransition(dir, *m); werr != nil {
		return true, notices, nil, werr
	}
	return true, notices, nil, nil
}

// entryURLFor is the entry's current URL, or empty when the entry is gone
// - a `config delete-server` removed it while its cleanup steps remain.
// A helper step carries its own recorded URL and does not rely on this.
func entryURLFor(cfg *Config, entry string) string {
	if cfg == nil {
		return ""
	}
	return cfg.Servers[entry].URL
}

// errText is a step's recorded failure, for the status report.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ClearFinishedTransition drops a transition id the directory no longer
// has a file for. The last step of a replay deletes the file, and the
// config write that follows is what forgets the name; a crash in between
// leaves a name with nothing behind it, which the next config write
// clears the same way.
func ClearFinishedTransition(dir string, cfg *Config) {
	if cfg == nil || cfg.CredentialTransitionID == "" {
		return
	}
	if m, err := readTransition(dir); err == nil && m == nil {
		cfg.CredentialTransitionID = ""
	}
}

// The notes an error adds about the directory's store change. The first
// sits beside a remedy that still works; the second replaces it as the
// hint, since that remedy is a credential command the record refuses.
const (
	unfinishedChangeNote = "a credential store change is unfinished; run " + StatusCommand + " to see it"
	unreadableChangeNote = "the credential store change record cannot be read; every auth login, auth logout, config delete-server and config update-server --url refuses until it is restored or moved aside; run " + StatusCommand + " to see it"
)

// UnfinishedChange is what an error with a remedy of its own - "not
// logged in", credentials in both stores, the exec plugin's sign-in with
// no terminal, auth status's failures - says about the directory's store
// change: nothing, a note beside that remedy, or, while the change record
// cannot be read, a note and the status report as the hint.
type UnfinishedChange struct {
	Note string
	// Refuses: the record cannot be read, so the error's own remedy is
	// refused until it is restored or moved aside.
	Refuses bool
}

// UnfinishedChangeOf reads the directory's store change for such an
// error, the way the status report reads it. It opens nothing when
// config.yaml names no change, so the paths that call it pay nothing for
// it then: a record nothing names is seen by the status commands and by
// the prelude of the command a hint names, which refuses on it.
func UnfinishedChangeOf(dir string, cfg *Config) UnfinishedChange {
	if dir == "" || cfg == nil || cfg.CredentialTransitionID == "" {
		return UnfinishedChange{}
	}
	return ChangeRecordStatusOf(dir, cfg).Pending()
}

// Apply adds the note to err's message, keeping its code and cause. When
// the record refuses, the error's hint moves into its message, as the
// command to run once the record is restored, and the status report
// becomes the hint. An error whose message already carries the note - it
// was given it where it was raised - or a cancellation is returned as it
// is.
func (u UnfinishedChange) Apply(err error) error {
	if err == nil || u.Note == "" || strings.Contains(err.Error(), u.Note) {
		return err
	}
	return clierr.WrapUnlessCanceled(err, func(err error) error {
		out := clierr.Wrap(err, err.Error()+"; "+u.Note)
		if !u.Refuses {
			return out
		}
		if out.Hint != "" && !strings.Contains(out.Message, out.Hint) {
			out = out.WithMessage(out.Message + "; after that: " + out.Hint)
		}
		return out.WithHint(StatusCommand)
	})
}
