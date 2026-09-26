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

// moveFixture is one file-to-keychain move with the credential in the
// source, which is the state a replay finds after a login recorded the
// change and died before running it.
type moveFixture struct {
	dir   string
	step  transitionStep
	token string
}

func newMoveFixture(t *testing.T, token string) *moveFixture {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	return &moveFixture{
		dir:   dir,
		token: token,
		step: transitionStep{
			Kind: transitionMove, Entry: "prod",
			Source: backendFile, Destination: backendKeychain,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, token),
			DestAccountID: acct,
		},
	}
}

func (f *moveFixture) seedSource(t *testing.T, token string) {
	t.Helper()
	require.NoError(t, NewFileStore(f.dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, token))
}

func (f *moveFixture) seedDestination(t *testing.T, token string) {
	t.Helper()
	require.NoError(t, keyringSet(keychainAccount(f.step.DestAccountID, "prod"), token))
}

func (f *moveFixture) sourceToken(t *testing.T) string {
	t.Helper()
	tok, err := NewFileStore(f.dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	if err != nil {
		return ""
	}
	return tok
}

func (f *moveFixture) destinationToken(t *testing.T) string {
	t.Helper()
	tok, err := keyringGet(keychainAccount(f.step.DestAccountID, "prod"))
	if err != nil {
		return ""
	}
	return tok
}

func (f *moveFixture) replay(t *testing.T) stepOutcome {
	t.Helper()
	step := f.step
	return replayMove(t.Context(), f.dir, &step, "https://r.ex", func(account string) error {
		f.step.DestAccountID = account
		return nil
	})
}

// TestReplayMoveFromTheSource asserts the ordinary case: the credential is
// where the change said it was, it lands in the destination, and only then
// is the source removed.
func TestReplayMoveFromTheSource(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "tok")

	out := f.replay(t)
	assert.Equal(t, verdictDone, out.verdict)
	assert.Equal(t, "tok", f.destinationToken(t))
	assert.Empty(t, f.sourceToken(t), "the source is removed only after the destination is verified")

	rec, err := readIdentityRecord(f.dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, rec, "a keychain destination gets the identity record that names it")
	assert.Equal(t, identityActive, rec.State)
	assert.Equal(t, f.step.DestAccountID, rec.ID)
}

// TestReplayMoveResumesAfterEachCrashPoint asserts the two boundaries a
// move has. A process can die after writing the destination and before
// deleting the source, and after deleting the source and before removing
// the step. Both are reached by a retry that asks the destination first,
// and neither loses the credential.
func TestReplayMoveResumesAfterEachCrashPoint(t *testing.T) {
	t.Run("destination written, source still there", func(t *testing.T) {
		f := newMoveFixture(t, "tok")
		f.seedSource(t, "tok")
		f.seedDestination(t, "tok")

		out := f.replay(t)
		assert.Equal(t, verdictDone, out.verdict)
		assert.Equal(t, "tok", f.destinationToken(t))
		assert.Empty(t, f.sourceToken(t), "the retry finishes the delete the crash interrupted")
	})

	t.Run("source deleted, step still there", func(t *testing.T) {
		f := newMoveFixture(t, "tok")
		f.seedDestination(t, "tok")

		out := f.replay(t)
		assert.Equal(t, verdictDone, out.verdict, "an absent source is success only once the destination is proven")
		assert.Equal(t, "tok", f.destinationToken(t))
	})
}

// TestReplayMoveWithTheCredentialNowhereIsTerminal asserts the case no
// retry can fix. The destination does not hold it and the source does not
// either, so the credential is in neither place the change knows about;
// saying so with the way out beats a step that fails forever.
func TestReplayMoveWithTheCredentialNowhereIsTerminal(t *testing.T) {
	f := newMoveFixture(t, "tok")

	out := f.replay(t)
	assert.Equal(t, verdictTerminal, out.verdict)
	err := reported(f.step, out)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.False(t, ce.Retryable, "no retry can change where the credential is")
	require.ErrorContains(t, err, "neither the store it was moving from nor the one it was moving to")
	require.ErrorContains(t, err, "log out this entry")
}

// TestReplayMoveLeavesADifferentSourceAlone asserts the digest is what
// limits the move. A file-store credential that is not the one the change
// recorded is somebody else's business: it is neither moved nor deleted,
// and the step stays so the state is visible.
func TestReplayMoveLeavesADifferentSourceAlone(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "a-different-token")

	out := f.replay(t)
	assert.Equal(t, verdictRetain, out.verdict)
	require.ErrorContains(t, reported(f.step, out), "not the one the change recorded")
	assert.Equal(t, "a-different-token", f.sourceToken(t), "nothing was deleted")
	assert.Empty(t, f.destinationToken(t), "nothing was written")
}

// TestReplayMoveRotatesRatherThanOverwriteADivergedDestination asserts the
// keychain case a copied config directory creates. The destination account
// holds a credential this change did not put there - another directory
// sharing the identity did - and overwriting it would destroy that
// directory's login. A fresh account is the answer, persisted before it is
// written so a crash in between is still addressable.
func TestReplayMoveRotatesRatherThanOverwriteADivergedDestination(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "tok")
	original := f.step.DestAccountID
	f.seedDestination(t, "another-directorys-token")

	out := f.replay(t)
	assert.Equal(t, verdictDone, out.verdict)
	assert.NotEqual(t, original, f.step.DestAccountID, "the change rotated to an account of its own")

	other, err := keyringGet(keychainAccount(original, "prod"))
	require.NoError(t, err)
	assert.Equal(t, "another-directorys-token", other, "the other directory's credential is untouched")

	moved, merr := keyringGet(keychainAccount(f.step.DestAccountID, "prod"))
	require.NoError(t, merr)
	assert.Equal(t, "tok", moved)
}

// TestReplayMoveFromADivergedKeychainSourceRetainsBeforeTheWrite asserts
// where supersession does NOT apply. The destination has not been written,
// so nothing has moved; the source slot holding another directory's
// credential is not a reason to drop the step, because dropping it would
// leave the entry with a credential in neither place and nothing in
// `config status` to say so. The step stays, where the operator can see
// it.
func TestReplayMoveFromADivergedKeychainSourceRetainsBeforeTheWrite(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	step := transitionStep{
		Kind: transitionMove, Entry: "prod",
		Source: backendKeychain, Destination: backendFile,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
		SourceAccountID: acct,
	}
	require.NoError(t, keyringSet(keychainAccount(acct, "prod"), "another-directorys-token"))

	out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
	assert.Equal(t, verdictRetain, out.verdict)
	ce, ok := errorsAsClierr(reported(step, out))
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code,
		"a step that may finish later carries the retryable code")
	assert.True(t, ce.Retryable)

	slot, gerr := keyringGet(keychainAccount(acct, "prod"))
	require.NoError(t, gerr)
	assert.Equal(t, "another-directorys-token", slot, "the other directory's credential stays")
}

// TestReplayMoveFromADivergedKeychainSourceIsSupersededAfterTheWrite
// asserts where supersession does apply: the destination already holds the
// credential, so the move is provably complete, and the source slot having
// become another copy's since is not this directory's business any more.
// Nothing is deleted and the claim on the slot is dropped.
func TestReplayMoveFromADivergedKeychainSourceIsSupersededAfterTheWrite(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	step := transitionStep{
		Kind: transitionMove, Entry: "prod",
		Source: backendKeychain, Destination: backendFile,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
		SourceAccountID: acct,
	}
	// The destination has it, so the write happened.
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "tok"))
	require.NoError(t, keyringSet(keychainAccount(acct, "prod"), "another-directorys-token"))
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityActive,
		ID: acct, Fingerprint: credentialFingerprint(acct, "tok"),
	}))

	out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
	assert.Equal(t, verdictDone, out.verdict)

	slot, gerr := keyringGet(keychainAccount(acct, "prod"))
	require.NoError(t, gerr)
	assert.Equal(t, "another-directorys-token", slot, "the other directory's credential is never deleted")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.Nil(t, rec, "this directory stops claiming the slot")
}

// TestReplayMoveOverwritesAFileDestination asserts a file destination is
// this directory's own file, not a shared slot: a stale plaintext token
// left there by an earlier era is overwritten by the move, and the
// read-back is what proves the result. Only the keychain is shared, which
// is why the rotation case is keychain-only.
func TestReplayMoveOverwritesAFileDestination(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	step := transitionStep{
		Kind: transitionMove, Entry: "prod",
		Source: backendKeychain, Destination: backendFile,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
		SourceAccountID: acct,
	}
	require.NoError(t, keyringSet(keychainAccount(acct, "prod"), "tok"))
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "a-stale-plaintext-token"))

	out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
	assert.Equal(t, verdictDone, out.verdict)

	got, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok", got, "the move landed rather than blocking on the stale copy")
	_, kerr := keyringGet(keychainAccount(acct, "prod"))
	require.Error(t, kerr, "and the source is gone")
}

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

// TestReplayMoveSettlesARecordItsOwnWriteLeftPending asserts the crash
// point between the keychain write and the record that settles it. The
// credential is in the destination, but a reader meeting a pending record
// gets LOGIN_INCOMPLETE, so a move that finished without settling it would
// leave the entry unusable with no step left to repair it.
func TestReplayMoveSettlesARecordItsOwnWriteLeftPending(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "tok")
	f.seedDestination(t, "tok")
	// The state a crash between keyringSet and the settling write leaves.
	require.NoError(t, writeIdentityRecord(f.dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityPending,
		ID: f.step.DestAccountID, Fingerprint: credentialFingerprint(f.step.DestAccountID, "tok"),
	}))

	out := f.replay(t)
	assert.Equal(t, verdictDone, out.verdict)

	rec, err := readIdentityRecord(f.dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, identityActive, rec.State, "the record the move opened is settled by the move")
	assert.Equal(t, f.step.DestAccountID, rec.ID)

	// The entry reads as itself again, rather than as a login in progress.
	tok, gerr := newPinnedKeyringStore(f.dir, true).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, "tok", tok)
}

// TestReplayMoveRevertsARecordWhoseWriteNeverLanded asserts the other half
// of that crash point: the record was written, the keychain set never
// happened, and the entry must go back to the identity it had rather than
// keep pointing at an account with nothing in it.
func TestReplayMoveRevertsARecordWhoseWriteNeverLanded(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "tok")
	priorAccount, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	require.NoError(t, writeIdentityRecord(f.dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityPending,
		ID: f.step.DestAccountID, Fingerprint: credentialFingerprint(f.step.DestAccountID, "tok"),
		Prior: &identityPrior{ID: priorAccount, Fingerprint: credentialFingerprint(priorAccount, "older")},
	}))

	out := f.replay(t)
	assert.Equal(t, verdictDone, out.verdict, "the move runs from the source, as it would have the first time")

	rec, err := readIdentityRecord(f.dir, "prod")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, identityActive, rec.State)
	assert.Equal(t, f.step.DestAccountID, rec.ID, "and settles on the account the move wrote")
	assert.Nil(t, rec.Prior)
	assert.Equal(t, "tok", f.destinationToken(t))
}

// TestReplayMoveLeavesAChangedFileSourceAlone asserts step 3's retain for
// a source that is not the keychain: the destination already holds the
// credential, so the move is complete as far as the credential goes, but
// the file now holds something a later writer put there. It is not this
// change's to delete, so the step stays and says so, and the file is
// untouched.
func TestReplayMoveLeavesAChangedFileSourceAlone(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedDestination(t, "tok")
	f.seedSource(t, "a-later-logins-token")

	out := f.replay(t)
	assert.Equal(t, verdictRetain, out.verdict)
	err := reported(f.step, out)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	require.ErrorContains(t, err, "different credential")
	assert.Equal(t, "a-later-logins-token", f.sourceToken(t), "the source was not deleted")
	assert.Equal(t, "tok", f.destinationToken(t))
}

// vanishingKeyring is a keychain whose credential disappears after it has
// been read once: the state a concurrent process leaves when it removes
// the destination between a move's read-back and its final check.
type vanishingKeyring struct {
	creds map[string]string
	reads map[string]int
}

func newVanishingKeyring() *vanishingKeyring {
	return &vanishingKeyring{creds: map[string]string{}, reads: map[string]int{}}
}

func (v *vanishingKeyring) Set(_, user, password string) error {
	v.creds[user] = password
	return nil
}

func (v *vanishingKeyring) Get(_, user string) (string, error) {
	tok, ok := v.creds[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	v.reads[user]++
	if v.reads[user] == 1 {
		delete(v.creds, user)
	}
	return tok, nil
}

func (v *vanishingKeyring) Delete(_, user string) error {
	if _, ok := v.creds[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(v.creds, user)
	return nil
}

// TestReplayMoveRetainsWhenTheDestinationVanishesAfterTheSourceDelete
// asserts step 4, the last destination read: the source is gone by then,
// so a destination that no longer holds the credential is the one state
// the step must not be dropped in. It is retained, retryable, and says
// where the credential was last seen.
func TestReplayMoveRetainsWhenTheDestinationVanishesAfterTheSourceDelete(t *testing.T) {
	f := newMoveFixture(t, "tok")
	f.seedSource(t, "tok")
	restore := SetKeyringBackendForTest(newVanishingKeyring())
	defer restore()

	out := f.replay(t)
	assert.Equal(t, verdictRetain, out.verdict)
	err := reported(f.step, out)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	require.ErrorContains(t, err, "no longer holds the credential")
	assert.Empty(t, f.sourceToken(t), "the source delete had already run")
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
// repeats. Both replay paths that retire a record earn the same verdict.
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

	t.Run("a move whose credential is nowhere", func(t *testing.T) {
		keyring.MockInit()
		dir := t.TempDir()
		acct := symlinkRecord(t, dir, "prod")
		vid, err := newTransitionID()
		require.NoError(t, err)
		step := transitionStep{
			Kind: transitionMove, Entry: "prod", Source: backendKeychain, Destination: backendFile,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
			SourceAccountID: acct,
		}

		out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
		assert.Equal(t, verdictTerminal, out.verdict)
		require.ErrorContains(t, reported(step, out), "neither the store it was moving from nor the one it was moving to",
			"the move's own verdict stands")
		require.Len(t, out.notices, 1)
		assert.Contains(t, out.notices[0], "retiring the credential identity for prod",
			"and the record it could not retire is said beside it")
	})

	t.Run("a move whose credential has landed", func(t *testing.T) {
		keyring.MockInit()
		dir := t.TempDir()
		acct := symlinkRecord(t, dir, "prod")
		vid, err := newTransitionID()
		require.NoError(t, err)
		step := transitionStep{
			Kind: transitionMove, Entry: "prod", Source: backendKeychain, Destination: backendFile,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
			SourceAccountID: acct,
		}
		require.NoError(t, keyringSet(keychainAccount(acct, "prod"), "tok"))

		out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
		assert.Equal(t, verdictTerminal, out.verdict)
		require.ErrorContains(t, reported(step, out), "retiring the credential identity for prod")
		tok, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
		require.NoError(t, gerr)
		assert.Equal(t, "tok", tok, "the credential work itself was done")
		_, kerr := keyringGet(keychainAccount(acct, "prod"))
		require.ErrorIs(t, kerr, keyring.ErrNotFound, "and the source slot is gone")
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

// hookedKeyring is a keychain that runs a callback after every delete: the
// seam for a concurrent process acting between a move's source delete and
// its final destination check.
type hookedKeyring struct {
	creds       map[string]string
	afterDelete func()
}

func (h *hookedKeyring) Set(_, user, password string) error {
	h.creds[user] = password
	return nil
}

func (h *hookedKeyring) Get(_, user string) (string, error) {
	tok, ok := h.creds[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return tok, nil
}

func (h *hookedKeyring) Delete(_, user string) error {
	if _, ok := h.creds[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(h.creds, user)
	h.afterDelete()
	return nil
}

// TestReplayMoveKeepsItsNoticesWhenTheLastCheckRetains asserts that what
// step 3 learned is not lost when step 4 retains: a record left in place
// because it names a later login is reported beside the retained step, or
// the operator would find the old record still there with nothing having
// said why.
func TestReplayMoveKeepsItsNoticesWhenTheLastCheckRetains(t *testing.T) {
	dir := t.TempDir()
	old, later := mustAccount(t), mustAccount(t)
	vid, err := newTransitionID()
	require.NoError(t, err)
	step := transitionStep{
		Kind: transitionMove, Entry: "prod", Source: backendKeychain, Destination: backendFile,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
		SourceAccountID: old,
	}
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityActive,
		ID: later, Fingerprint: credentialFingerprint(later, "new-tok"),
	}))
	kr := &hookedKeyring{creds: map[string]string{keychainAccount(old, "prod"): "tok"}}
	// Once the source is deleted, another process removes the destination
	// before the move's final check.
	kr.afterDelete = func() {
		require.NoError(t, NewFileStore(dir).DeleteToken(ServerRef{Name: "prod", URL: "https://r.ex"}))
	}
	restore := SetKeyringBackendForTest(kr)
	defer restore()

	out := replayMove(t.Context(), dir, &step, "https://r.ex", func(string) error { return nil })
	assert.Equal(t, verdictRetain, out.verdict)
	require.ErrorContains(t, reported(step, out), "no longer holds the credential")
	require.Len(t, out.notices, 1)
	assert.Contains(t, out.notices[0], "names a later login and was left in place")
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
