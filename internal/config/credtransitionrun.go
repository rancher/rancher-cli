package config

import (
	"context"
	"errors"
	"slices"

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
// may be updated here; the caller writes it.
func PrepareMutation(ctx context.Context, dir string, cfg *Config, intent MutationIntent) (notices []string, err error) {
	return prepareMutation(ctx, dir, cfg, intent, func(fn func() error) error {
		return withTransitionLock(ctx, dir, fn)
	})
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
	case TransitionNone, TransitionFinished:
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
		if fresh == nil || fresh.ID != m.ID {
			// Finished, or replaced by a change this command never loaded.
			// The second is reachable only with the flock skipped. A
			// manifest this pass did not settle is deliberately left to
			// the command that owns it rather than refused on here, and
			// with the flock skipped that is the same lost-update residual
			// withTransitionLock already documents.
			return nil
		}
		m = fresh
		ns, left, rerr := replayPass(ctx, dir, cfg, m)
		notices = append(notices, ns...)
		retained = left
		if rerr == nil && len(m.Steps) > 0 {
			// What the pass left is decided here, under the lock that
			// settled it, rather than read back from disk afterwards.
			remaining = m
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
// keeps the verdict it earned - a non-retryable error naming the command
// the step points at, when it has one. A step whose last attempt could
// not finish carries the retryable code with the same command riding
// along as the hint, because some of those never clear on their own. A
// step no attempt has judged yet says only that work is left, and so does
// an attempted step that names no entry - the pin - since its subject
// would be empty; its cause still rides in the message. An agent that
// branches on retryable must never be told to retry a state no retry can
// change.
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

// RefusedByTransition answers the same question PrepareMutation answers,
// without the config lock and without replaying anything, so a login can
// decline before it asks anyone to authenticate rather than after. The
// locked prelude stays the authority: this only avoids a sign-in that was
// never going to be accepted.
func RefusedByTransition(dir string, cfg *Config, intent MutationIntent) error {
	if !HasPendingTransition(dir, cfg) {
		return nil
	}
	state, m, err := LoadTransition(dir, cfg)
	if err != nil {
		return err
	}
	if state != TransitionActive {
		return nil
	}
	return refusedBy(m, intent)
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

// ErrNoTokenHint is the hint an entry's "not logged in" carries while a
// store change is unfinished. Without it the operator is told to log in,
// when what is missing may be the pin the change has not written yet.
func ErrNoTokenHint(dir string, cfg *Config) string {
	if HasPendingTransition(dir, cfg) {
		return "rancher config status"
	}
	return ""
}
