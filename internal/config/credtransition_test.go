package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validManifest is a two-step change: delete one entry's old copy from the
// file store, then write the pin that makes the keychain the directory's
// answer.
func validManifest(t *testing.T) transitionManifest {
	t.Helper()
	id, err := newTransitionID()
	require.NoError(t, err)
	vid, verr := newTransitionID()
	require.NoError(t, verr)
	return transitionManifest{
		Version: transitionVersion,
		ID:      id,
		Steps: []transitionStep{
			{
				Kind: transitionDelete, Entry: "prod", Source: backendFile,
				VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
			},
			{Kind: transitionPin, Target: backendKeychain},
		},
	}
}

// TestTransitionRoundTrip asserts a manifest written by this package reads
// back as the same work.
func TestTransitionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := validManifest(t)
	require.NoError(t, writeTransition(dir, want))

	got, err := readTransition(dir)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want, *got)

	fi, serr := os.Stat(TransitionPath(dir))
	require.NoError(t, serr)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "owner-only, like every other credential state")
	}
}

// TestTransitionAbsentIsNotAChange asserts a directory with no manifest has
// nothing recorded. Absent is the ordinary state and never an error.
func TestTransitionAbsentIsNotAChange(t *testing.T) {
	m, err := readTransition(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, m)
}

// validManifestBytes is a manifest that passes validation, as the file
// holds it, so a case that puts it somewhere unsafe fails on the safety
// check alone and not on its content.
func validManifestBytes(t *testing.T) []byte {
	t.Helper()
	scratch := t.TempDir()
	require.NoError(t, writeTransition(scratch, validManifest(t)))
	b, err := os.ReadFile(TransitionPath(scratch))
	require.NoError(t, err)
	return b
}

// TestTransitionUnsafeOrUnreadableIsTerminal asserts every state that is
// not a manifest this release can act on. A manifest drives deletes across
// backends: one it cannot read is a set of credential deletes nobody can
// account for, so it stops the command, says no backend operation ran, and
// is left on disk for the operator to inspect rather than rewritten.
func TestTransitionUnsafeOrUnreadableIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		skip  string
		// want is the words the error must use for this state, so each
		// case proves the check it names and not an earlier one.
		want string
	}{
		{name: "empty", want: "is empty", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), nil, 0o600))
		}},
		{name: "unparseable", want: "unreadable", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("\t: not yaml ["), 0o600))
		}},
		{name: "a version this release does not understand", want: "version 99", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("version: 99\nid: 0123456789abcdef0123456789abcdef\n"), 0o600))
		}},
		{name: "no identifier", want: "identifier", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("version: 1\n"), 0o600))
		}},
		{name: "no steps", want: "is empty", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir),
				[]byte("version: 1\nid: 0123456789abcdef0123456789abcdef\n"), 0o600))
		}},
		{name: "a move an earlier build recorded", want: "step 1 is of no known kind", setup: func(t *testing.T, dir string) {
			t.Helper()
			// No login records a move any more. One a pre-release build
			// left behind - staging's credential on its way from the file
			// store into the keychain - is refused like any kind this
			// release does not know, with the file named and the way to
			// set it aside.
			vid, err := newTransitionID()
			require.NoError(t, err)
			acct, aerr := newKeychainAccountID()
			require.NoError(t, aerr)
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte(
				"version: 1\nid: 0123456789abcdef0123456789abcdef\nsteps:\n- kind: move\n  entry: staging\n  source: file\n  destination: keychain\n"+
					"  verificationId: "+vid+"\n  credentialDigest: "+credentialFingerprint(vid, "tok")+"\n  destAccountId: "+acct+"\n"), 0o600))
		}},
		{name: "a pin recorded as impossible", want: "recorded as impossible", setup: func(t *testing.T, dir string) {
			t.Helper()
			// No attempt writes this: a pin either lands or is retried, and
			// writeTransition refuses it too. Written raw, the way a hand
			// edit would leave it; read back, it would render a terminal
			// step with no entry to name.
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte(
				"version: 1\nid: 0123456789abcdef0123456789abcdef\nsteps:\n- kind: pin\n  target: file\n  terminal: true\n  lastError: cannot recover\n"), 0o600))
		}},
		{name: "a step recorded as impossible with no cause", want: "without a cause", setup: func(t *testing.T, dir string) {
			t.Helper()
			// terminalBecause always carries a cause, so no attempt
			// produces this. Written raw, the way a hand edit would leave
			// it; read back, it would refuse the next command with an
			// empty reason.
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte(
				"version: 1\nid: 0123456789abcdef0123456789abcdef\nsteps:\n- kind: delete\n  entry: prod\n  source: keychain\n"+
					"  verificationId: 0123456789abcdef0123456789abcdef\n"+
					"  credentialDigest: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"+
					"  sourceAccountId: 0123456789abcdef0123456789abcdef\n  terminal: true\n"), 0o600))
		}},
		{name: "a symlink", skip: "windows", want: "a symlink", setup: func(t *testing.T, dir string) {
			t.Helper()
			backing := filepath.Join(dir, "elsewhere.yaml")
			require.NoError(t, os.WriteFile(backing, validManifestBytes(t), 0o600))
			require.NoError(t, os.Symlink(backing, TransitionPath(dir)))
		}},
		{name: "a directory", want: "not a regular file", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.Mkdir(TransitionPath(dir), 0o700))
		}},
		{name: "readable by other users", skip: "windows", want: "other than its owner", setup: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), validManifestBytes(t), 0o644))
		}},
		{name: "unreadable by its owner", skip: "windows", want: "unreadable: ", setup: func(t *testing.T, dir string) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-0000 file")
			}
			require.NoError(t, os.WriteFile(TransitionPath(dir), validManifestBytes(t), 0o600))
			require.NoError(t, os.Chmod(TransitionPath(dir), 0o000))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skip == runtime.GOOS {
				t.Skip("the check this exercises is not the one that applies on " + runtime.GOOS)
			}
			dir := t.TempDir()
			tc.setup(t, dir)

			before, rerr := os.Lstat(TransitionPath(dir))
			require.NoError(t, rerr)

			_, err := readTransition(dir)
			require.Error(t, err)
			ce, ok := errorsAsClierr(err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, transitionFileName)
			require.ErrorContains(t, err, "no credential was read or written")
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, clierr.Rancher+" config status", ce.Hint, "the report that shows the file and why it cannot be used")

			after, aerr := os.Lstat(TransitionPath(dir))
			require.NoError(t, aerr, "the file is left for the operator to inspect")
			assert.Equal(t, before.Mode(), after.Mode())
		})
	}
}

// TestTransitionStepValidation asserts the fields a replay would otherwise
// have to trust. Every one of them decides which credential a step may
// read, write or delete.
func TestTransitionStepValidation(t *testing.T) {
	dir := t.TempDir()
	id, err := newTransitionID()
	require.NoError(t, err)
	vid, verr := newTransitionID()
	require.NoError(t, verr)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	digest := credentialFingerprint(vid, "tok")
	ok := transitionStep{
		Kind: transitionDelete, Entry: "prod", Source: backendFile,
		VerificationID: vid, CredentialDigest: digest,
	}
	with := func(f func(*transitionStep)) transitionStep {
		s := ok
		f(&s)
		return s
	}

	for _, tc := range []struct {
		name string
		step transitionStep
		good bool
	}{
		{name: "a delete from the file store", step: ok, good: true},
		{name: "a delete from the keychain naming its account", good: true, step: with(func(s *transitionStep) {
			s.Source, s.SourceAccountID = backendKeychain, acct
		})},
		{name: "a pin", good: true, step: transitionStep{Kind: transitionPin, Target: backendFile}},
		// No login records a move any more; one left by an earlier build
		// is refused like any other kind this release does not know.
		{name: "a move", step: with(func(s *transitionStep) { s.Kind = "move" })},
		{name: "a pin to nowhere", step: transitionStep{Kind: transitionPin, Target: "auto"}},
		{name: "a kind this release does not know", step: with(func(s *transitionStep) { s.Kind = "rename" })},
		{name: "no entry", step: with(func(s *transitionStep) { s.Entry = "" })},
		{name: "no verifier", step: with(func(s *transitionStep) { s.VerificationID = "" })},
		{name: "no digest", step: with(func(s *transitionStep) { s.CredentialDigest = "" })},
		{name: "a policy rather than a backend", step: with(func(s *transitionStep) { s.Source = "auto" })},
		{name: "a helper source with no URL to address it by", step: with(func(s *transitionStep) {
			s.Source = "exec:vault"
		})},
		{name: "a helper source with its URL", good: true, step: with(func(s *transitionStep) {
			s.Source, s.CanonicalURL = "exec:vault", "https://r.ex"
		})},
		{name: "a keychain source naming no account", step: with(func(s *transitionStep) {
			s.Source = backendKeychain
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateTransition(dir, &transitionManifest{
				Version: transitionVersion, ID: id, Steps: []transitionStep{tc.step},
			})
			if tc.good {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}
}

// TestTransitionStates asserts what the pair of "does the config name a
// change" and "is there a file" amounts to. Both halves are needed: the
// name alone would leave a finished change looking pending forever, and
// the file alone would let work a login abandoned before it ever touched a
// credential be replayed.
func TestTransitionStates(t *testing.T) {
	m := validManifest(t)

	t.Run("neither: nothing recorded", func(t *testing.T) {
		state, got, err := LoadTransition(t.TempDir(), &Config{})
		require.NoError(t, err)
		assert.Equal(t, TransitionNone, state)
		assert.Nil(t, got)
	})

	t.Run("both, agreeing: active", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, writeTransition(dir, m))
		state, got, err := LoadTransition(dir, &Config{CredentialTransitionID: m.ID})
		require.NoError(t, err)
		assert.Equal(t, TransitionActive, state)
		require.NotNil(t, got)
		assert.Equal(t, m.ID, got.ID)
		assert.True(t, HasPendingTransition(dir, &Config{CredentialTransitionID: m.ID}))
	})

	t.Run("a file the config does not name: abandoned", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, writeTransition(dir, m))
		state, got, err := LoadTransition(dir, &Config{})
		require.NoError(t, err)
		assert.Equal(t, TransitionAbandoned, state, "a login died between preparing the change and activating it")
		require.NotNil(t, got)
		assert.False(t, HasPendingTransition(dir, &Config{}), "nothing was ever moved")
	})

	t.Run("a name with no file: finished", func(t *testing.T) {
		dir := t.TempDir()
		state, got, err := LoadTransition(dir, &Config{CredentialTransitionID: m.ID})
		require.NoError(t, err)
		assert.Equal(t, TransitionFinished, state, "the last step removed the file before the config write")
		assert.Nil(t, got)
		assert.False(t, HasPendingTransition(dir, &Config{CredentialTransitionID: m.ID}))
	})

	t.Run("a file with another identifier: terminal", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, writeTransition(dir, m))
		other, err := newTransitionID()
		require.NoError(t, err)
		_, _, lerr := LoadTransition(dir, &Config{CredentialTransitionID: other})
		require.Error(t, lerr)
		require.ErrorContains(t, lerr, other)
	})
}

// TestPendingTransitionTouchesNoFileWithoutAName asserts the hot-path rule:
// a directory whose config names no change never opens the manifest, so
// the exec plugin's cache hit and the credential read pay nothing for a
// feature they are not using.
func TestPendingTransitionTouchesNoFileWithoutAName(t *testing.T) {
	var ops []string
	restore := SetIdentityObserverForTest(func(op string) { ops = append(ops, op) })
	defer restore()

	dir := t.TempDir()
	require.NoError(t, writeTransition(dir, validManifest(t)))
	ops = nil

	assert.False(t, HasPendingTransition(dir, &Config{}))
	assert.Empty(t, ops, "no stat, no read: the config named nothing")
}

// errorsAsClierr is the typed-error extraction these tests repeat.
func errorsAsClierr(err error) (*clierr.Error, bool) {
	return errors.AsType[*clierr.Error](err)
}

// TestTransitionLockSerializesAndHonorsTheVariable asserts the cold-path
// lock: two commands cannot each decide the same step still needs doing,
// and RANCHER_CLI_CONFIG_NO_LOCK skips the flock while keeping the
// in-process semaphore, exactly as it does for every other lock here.
func TestTransitionLockSerializesAndHonorsTheVariable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigNoLock, "")

	var order []string
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withTransitionLock(t.Context(), dir, func() error {
			order = append(order, "first in")
			close(entered)
			<-release
			order = append(order, "first out")
			return nil
		})
	}()
	<-entered

	second := make(chan error, 1)
	go func() {
		second <- withTransitionLock(t.Context(), dir, func() error {
			order = append(order, "second in")
			return nil
		})
	}()
	// Nothing the second goroutine does can land before the first leaves.
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-second)
	assert.Equal(t, []string{"first in", "first out", "second in"}, order)

	assert.FileExists(t, filepath.Join(dir, transitionLockName))
}

// TestTransitionLockSkipsTheFlockUnderTheVariable asserts the escape hatch
// for filesystems whose advisory locking does not work: the flock file is
// never created, and the work still runs.
func TestTransitionLockSkipsTheFlockUnderTheVariable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigNoLock, "1")

	var ran bool
	require.NoError(t, withTransitionLock(t.Context(), dir, func() error {
		ran = true
		return nil
	}))
	assert.True(t, ran)
	assert.NoFileExists(t, filepath.Join(dir, transitionLockName))
}

// TestTransitionIncompleteNamesTheStepAndItsError asserts the error a
// command stops with while work is left: the code a caller branches on,
// retryable because the step completes on its own once the backend it
// needs answers, the step it is waiting on, and the backend error that
// last stopped it - which is the only thing that tells the operator what
// to fix.
func TestTransitionIncompleteNamesTheStepAndItsError(t *testing.T) {
	t.Parallel()
	err := transitionIncomplete(transitionStep{
		Kind: transitionDelete, Entry: "staging", LastError: "keyring get: the keychain is locked",
	})
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.True(t, ce.Retryable, "the work completes once the backend answers")
	require.ErrorContains(t, err, "delete staging")
	require.ErrorContains(t, err, "the keychain is locked")
	require.ErrorContains(t, err, "run this command again")
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint, "the report that shows every step left")

	// A step with no entry and no recorded error still says what is left.
	bare := transitionIncomplete(transitionStep{Kind: transitionPin})
	require.ErrorContains(t, bare, "pin")
	require.ErrorContains(t, bare, "unfinished")
}

// TestWriteTransitionNeverReplacesADifferentChange asserts the invariant
// the single fixed path exists for: a second manifest cannot be written
// over one that still has work, only the same change may be rewritten as
// its steps complete, and a change that has been removed leaves the path
// free.
func TestWriteTransitionNeverReplacesADifferentChange(t *testing.T) {
	dir := t.TempDir()
	first := validManifest(t)
	require.NoError(t, writeTransition(dir, first))

	second := validManifest(t)
	err := writeTransition(dir, second)
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "already recorded")
	require.ErrorContains(t, err, transitionFileName)
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint, "the report that shows the change that is there")

	got, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, got)
	assert.Equal(t, first.ID, got.ID, "the first change is untouched")

	rewritten := first
	rewritten.Steps[0].LastError = "the keychain is locked"
	require.NoError(t, writeTransition(dir, rewritten), "the same change may be rewritten")

	require.NoError(t, removeTransition(dir))
	require.NoError(t, writeTransition(dir, second), "a removed change leaves the path free")
}

// TestWriteTransitionRefusesAnInvalidManifest asserts the write side
// validates too. The read side refusing is what keeps an unusable
// manifest from being replayed; the write side refusing is what keeps one
// from reaching the disk in the first place, so a step this release
// cannot account for is never the file another process finds.
func TestWriteTransitionRefusesAnInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	m := validManifest(t)
	m.Steps = []transitionStep{{Kind: transitionPin, Target: backendFile, Terminal: true, LastError: "cannot recover"}}

	err := writeTransition(dir, m)
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "unusable: step 1 is")
	require.ErrorContains(t, err, "recorded as impossible")
	assert.NoFileExists(t, TransitionPath(dir), "nothing reached the disk")
}

// TestRemoveTransitionIsIdempotent asserts the manifest can be removed and
// that removing one that is already gone is not a failure: the last step
// of a replay removes it, and a crash after that leaves the next command
// removing nothing.
func TestRemoveTransitionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeTransition(dir, validManifest(t)))
	require.FileExists(t, TransitionPath(dir))

	require.NoError(t, removeTransition(dir))
	assert.NoFileExists(t, TransitionPath(dir))
	require.NoError(t, removeTransition(dir), "removing what is already gone is the same outcome")
}

// TestAnUnsafeManifestIsReportedEvenWhenUnnamed asserts the one state
// that most needs saying out loud is not swallowed by the fast path. A
// directory whose config names no change still has to classify a
// manifest path that is there but is not a manifest: a symlink or a
// directory where the file belongs is the state a botched restore or a
// dotfile manager leaves, and reporting nothing would let it sit there
// until a store-changing login tripped over it.
func TestAnUnsafeManifestIsReportedEvenWhenUnnamed(t *testing.T) {
	t.Run("a directory in its place", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(TransitionPath(dir), 0o700))

		_, _, err := LoadTransition(dir, &Config{})
		require.Error(t, err, "unnamed is not a reason to stop looking")
		require.ErrorContains(t, err, transitionFileName)
		require.ErrorContains(t, err, "no credential was read or written")
	})

	t.Run("a symlink in its place", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs a privilege the runner may not have")
		}
		dir := t.TempDir()
		backing := filepath.Join(dir, "elsewhere.yaml")
		require.NoError(t, os.WriteFile(backing, []byte("version: 1\n"), 0o600))
		require.NoError(t, os.Symlink(backing, TransitionPath(dir)))

		_, _, err := LoadTransition(dir, &Config{})
		require.ErrorContains(t, err, "a symlink")
	})

	t.Run("and nothing there is still nothing", func(t *testing.T) {
		state, m, err := LoadTransition(t.TempDir(), &Config{})
		require.NoError(t, err)
		assert.Equal(t, TransitionNone, state)
		assert.Nil(t, m)
	})
}
