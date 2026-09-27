package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestReplayDeleteIsConditional asserts a delete removes exactly the
// credential the change recorded. Anything else in that place belongs to a
// later login or another directory, and the step is superseded rather than
// performed.
func TestReplayDeleteIsConditional(t *testing.T) {
	seed := func(t *testing.T, stored string) (string, transitionStep) {
		t.Helper()
		dir := t.TempDir()
		vid, err := newTransitionID()
		require.NoError(t, err)
		if stored != "" {
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, stored))
		}
		return dir, transitionStep{
			Kind: transitionDelete, Entry: "prod", Source: backendFile,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
		}
	}

	t.Run("the recorded credential is removed", func(t *testing.T) {
		dir, step := seed(t, "tok")
		out := replayDelete(t.Context(), dir, step, "https://r.ex")
		assert.Equal(t, verdictDone, out.verdict)
		_, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
		require.ErrorIs(t, gerr, ErrNoToken)
	})

	t.Run("a different credential is left alone", func(t *testing.T) {
		dir, step := seed(t, "a-later-login")
		out := replayDelete(t.Context(), dir, step, "https://r.ex")
		assert.Equal(t, verdictDone, out.verdict, "superseded, not stuck")
		require.Len(t, out.notices, 1)
		assert.Contains(t, out.notices[0], "replaced since the change started")
		tok, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
		require.NoError(t, gerr)
		assert.Equal(t, "a-later-login", tok)
	})

	t.Run("an absent credential is already done", func(t *testing.T) {
		dir, step := seed(t, "")
		out := replayDelete(t.Context(), dir, step, "https://r.ex")
		assert.Equal(t, verdictDone, out.verdict)
		require.Len(t, out.notices, 1)
		assert.Contains(t, out.notices[0], "already gone")
	})
}

// TestReplayPinWritesTheTarget asserts the last step of a change.
func TestReplayPinWritesTheTarget(t *testing.T) {
	dir := t.TempDir()
	out := replayPin(dir, transitionStep{Kind: transitionPin, Target: backendKeychain})
	assert.Equal(t, verdictDone, out.verdict)
	assert.Equal(t, "keychain", ReadPin(dir))
}

// TestReplayLeavesARecordNamingALaterLoginInPlace asserts that retiring a
// keychain source's identity record checks whose record it is. A delete
// step for an old keychain copy can outlive a login for the same entry;
// the login wrote a fresh active record naming its own account, and
// removing that record when the old slot turns out empty would leave the
// newer credential with nothing addressing it.
func TestReplayLeavesARecordNamingALaterLoginInPlace(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	old, oerr := newKeychainAccountID()
	require.NoError(t, oerr)
	later, lerr := newKeychainAccountID()
	require.NoError(t, lerr)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityActive,
		ID: later, Fingerprint: credentialFingerprint(later, "new-tok"),
	}))
	step := transitionStep{
		Kind: transitionDelete, Entry: "prod", Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "old-tok"),
		SourceAccountID: old,
	}

	out := replayDelete(t.Context(), dir, step, "https://r.ex")
	assert.Equal(t, verdictDone, out.verdict, "an absent old copy finishes the delete")
	require.NotEmpty(t, out.notices)
	assert.Contains(t, out.notices[len(out.notices)-1], "later login")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	require.NotNil(t, rec, "the later login's record is still there")
	assert.Equal(t, later, rec.ID)

	// The same delete against a record that does name the old slot
	// retires it.
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityActive,
		ID: old, Fingerprint: credentialFingerprint(old, "old-tok"),
	}))
	out = replayDelete(t.Context(), dir, step, "https://r.ex")
	assert.Equal(t, verdictDone, out.verdict)
	rec, rerr = readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.Nil(t, rec, "a record naming the retired slot goes with it")
}

// reported is what a later command is told about a step this outcome left
// behind: the verdict recorded into the step and read back the way
// refusedBy and the replay's warning read it.
func reported(s transitionStep, out stepOutcome) error {
	out.record(&s)
	return stepError(s)
}

// keychainDeleteStep is a conditional delete of one entry's keychain copy.
func keychainDeleteStep(t *testing.T, entry, acct, token string) transitionStep {
	t.Helper()
	vid, err := newTransitionID()
	require.NoError(t, err)
	return transitionStep{
		Kind: transitionDelete, Entry: entry, Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, token),
		SourceAccountID: acct,
	}
}

// symlinkRecord puts a symlink where entry's identity record belongs,
// which the record read refuses. A record for another entry creates the
// identity directory with its proper mode first; its account id is
// returned for the step to name.
func symlinkRecord(t *testing.T, dir, entry string) string {
	t.Helper()
	acct := mustAccount(t)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "staging", State: identityActive,
		ID: acct, Fingerprint: credentialFingerprint(acct, "x"),
	}))
	require.NoError(t, os.Symlink(
		filepath.Join(identityDirPath(dir), identityFileName("staging")),
		filepath.Join(identityDirPath(dir), identityFileName(entry))))
	return acct
}

// TestReplayIsTerminalWhenTheSourceRecordCannotBeTrusted asserts the
// verdict for a step whose credential work is done while the identity
// record it must retire cannot be read as one - here a symlink where the
// record should be. No retry reads it differently, so the step is
// terminal and not retryable: an agent told to retry would retry forever,
// and the way out is the identity error's own, which the step's cause
// repeats.
func TestReplayIsTerminalWhenTheSourceRecordCannotBeTrusted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symlinked record needs POSIX symlink semantics")
	}
	t.Run("a delete whose slot is already empty", func(t *testing.T) {
		keyring.MockInit()
		dir := t.TempDir()
		acct := symlinkRecord(t, dir, "prod")
		step := keychainDeleteStep(t, "prod", acct, "old-tok")

		out := replayDelete(t.Context(), dir, step, "https://r.ex")
		assert.Equal(t, verdictTerminal, out.verdict)
		err := reported(step, out)
		ce, ok := errorsAsClierr(err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.False(t, ce.Retryable, "a record no retry can read is not retryable work")
		require.ErrorContains(t, err, "retiring the credential identity for prod")
		require.ErrorContains(t, err, "delete the file and log in again")
		assert.Empty(t, ce.Hint,
			"a delete already is the cleanup; a logout would change nothing, so no command is named")
		require.Len(t, out.notices, 1)
		assert.Contains(t, out.notices[0], "already gone",
			"the credential itself is gone, and that fact survives the failure")
	})
}

// TestReplayRetainsWhenTheRecordLockIsHeld asserts the other verdict a
// retirement can earn: a lock another writer holds is a reason to retry,
// so the step is retained with the retryable code, and it says which
// record it could not retire. The delete itself has run by then - the
// retirement follows the credential work, on this path as on every other.
func TestReplayRetainsWhenTheRecordLockIsHeld(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	restore := SetLockWaitTimeoutForTest(200 * time.Millisecond)
	defer restore()
	// This process's own identity-lock semaphore, held for the whole test.
	identityLockSem <- struct{}{}
	defer func() { <-identityLockSem }()
	acct := mustAccount(t)
	step := keychainDeleteStep(t, "prod", acct, "old-tok")
	require.NoError(t, keyringSet(keychainAccount(acct, "prod"), "old-tok"))

	out := replayDelete(t.Context(), dir, step, "https://r.ex")
	assert.Equal(t, verdictRetain, out.verdict)
	_, kerr := keyringGet(keychainAccount(acct, "prod"))
	require.ErrorIs(t, kerr, keyring.ErrNotFound, "the credential work was done before the record was tried")
	err := reported(step, out)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.True(t, ce.Retryable, "a held lock is a reason to retry")
	require.ErrorContains(t, err, "retiring the credential identity for prod")
	require.ErrorContains(t, err, "lock")
}

// TestRetirementOutcomeFollowsTheErrorsOwnVerdict asserts the one
// classification a retirement failure gets. Terminal is reserved for an
// error the identity layer marks AND refuses to retry - a record or
// directory that cannot be trusted, and a credential another directory
// replaced. A marked but retryable state, an
// unmarked validation error, a lock wait and a bare removal error are all
// retained, because a retry may finish them.
func TestRetirementOutcomeFollowsTheErrorsOwnVerdict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	step := transitionStep{Kind: transitionDelete, Entry: "prod", Source: backendKeychain}
	for _, tc := range []struct {
		name string
		err  error
		want transitionVerdict
	}{
		{"a record that cannot be trusted", identityError(dir, "prod", "a symlink"), verdictTerminal},
		{"an identity directory that cannot be trusted", identityDirError(dir, "a symlink"), verdictTerminal},
		{"a login in progress: marked, but retryable", loginIncompleteError("prod"), verdictRetain},
		{"a credential replaced by another directory: marked and not retryable", credentialReplacedError("prod"), verdictTerminal},
		{"a validation error without the identity marker", clierr.New(clierr.CodeValidation, "unrelated"), verdictRetain},
		{"a lock wait", lockWaitError(context.DeadlineExceeded, lockWaitInProcess), verdictRetain},
		{"a bare removal error", errors.New("remove: permission denied"), verdictRetain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := retirementOutcome(step, tc.err)
			assert.Equal(t, tc.want, out.verdict)
			assert.Contains(t, out.reason, "retiring the credential identity for prod: "+tc.err.Error(),
				"the cause is the error's own words behind the step's sentence")
		})
	}
}

// TestAHelperStepAddressesTheURLItRecorded asserts why a step whose backend
// is a credential helper carries a canonical URL of its own. The helper
// protocol keys a credential on that URL plus the name, and `config
// update-server --url` or `config delete-server` can change or remove the
// entry's URL before the step is replayed; addressing the entry's current
// URL would then erase nothing, or something else.
func TestAHelperStepAddressesTheURLItRecorded(t *testing.T) {
	step := transitionStep{
		Kind: transitionDelete, Entry: "prod", Source: "exec:vault",
		CanonicalURL: "https://old.ex",
	}
	acc := accessFor(t.TempDir(), step, step.Source, "", "https://new.ex")
	assert.Equal(t, "https://old.ex", acc.url,
		"the URL the step was scheduled with, not the one the entry has now")

	// A step against a backend the entry's own URL addresses carries none,
	// and follows the entry.
	plain := transitionStep{Kind: transitionDelete, Entry: "prod", Source: backendFile}
	assert.Equal(t, "https://new.ex", accessFor(t.TempDir(), plain, plain.Source, "", "https://new.ex").url)
}
