package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// keychainDir returns a fresh config directory and a keychain store over
// it. Every directory a test makes this way shares one in-memory keychain,
// which is the model the design works from: one operating-system user, one
// keychain, many config directories.
func keychainDir(t *testing.T) (string, CredStore) {
	t.Helper()
	dir := t.TempDir()
	return dir, newPinnedKeyringStore(dir, false)
}

// copyIdentityDir copies every identity record from one directory to
// another, which is what copying a config directory does: the same ids and
// the same fingerprints, naming the same keychain accounts.
func copyIdentityDir(t *testing.T, from, to string) {
	t.Helper()
	records, err := listIdentityRecords(from)
	require.NoError(t, err)
	for _, rec := range records {
		require.NoError(t, writeIdentityRecord(to, rec))
	}
}

func recordFor(t *testing.T, dir, entry string) identityRecord {
	t.Helper()
	rec, err := readIdentityRecord(dir, entry)
	require.NoError(t, err)
	require.NotNil(t, rec, "no identity record for %q", entry)
	return *rec
}

// TestKeychainAccountsSeparateUnrelatedDirectories asserts P2's close: two
// directories that never shared a file store the same server name under
// different keychain accounts, each reads only its own, and neither is
// reachable under the bare entry name.
func TestKeychainAccountsSeparateUnrelatedDirectories(t *testing.T) {
	keyring.MockInit()
	ref := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	dirB, b := keychainDir(t)

	require.NoError(t, a.SetToken(ref, "token-a"))
	require.NoError(t, b.SetToken(ref, "token-b"))

	recA, recB := recordFor(t, dirA, "prod"), recordFor(t, dirB, "prod")
	assert.NotEqual(t, recA.ID, recB.ID, "unrelated directories never share an identity")
	assert.Equal(t, recA.ID+":prod", recA.Account())

	tok, err := a.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "token-a", tok)
	tok, err = b.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "token-b", tok)

	_, err = keyring.Get(keyringService, "prod")
	assert.Error(t, err, "nothing is stored under the bare entry name")
}

// TestKeychainLogoutInAnotherDirectoryLeavesThisOne asserts the same
// separation across a delete: logging out of one directory cannot remove
// another directory's credential for the same server name.
func TestKeychainLogoutInAnotherDirectoryLeavesThisOne(t *testing.T) {
	keyring.MockInit()
	ref := ServerRef{Name: "prod", URL: "https://prod.ex"}
	_, a := keychainDir(t)
	dirB, b := keychainDir(t)

	require.NoError(t, a.SetToken(ref, "token-a"))
	require.NoError(t, b.SetToken(ref, "token-b"))

	_, _, err := DeleteLoginToken(t.Context(), dirB, ref, "keychain")
	require.NoError(t, err)

	tok, gerr := a.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "token-a", tok)
	_, gerr = b.GetToken(ref)
	assert.ErrorIs(t, gerr, ErrNoToken)
}

// TestKeychainRecordCreatedOnlyByAWrite asserts that only a credential
// write creates an identity: a read and a delete leave a directory without
// one, and a file store never grows one at all.
func TestKeychainRecordCreatedOnlyByAWrite(t *testing.T) {
	keyring.MockInit()
	ref := ServerRef{Name: "prod", URL: "https://prod.ex"}

	dir, store := keychainDir(t)
	_, err := store.GetToken(ref)
	require.ErrorIs(t, err, ErrNoToken)
	require.NoError(t, store.DeleteToken(ref))
	assert.NoDirExists(t, identityDirPath(dir), "neither a read nor a delete creates an identity")

	require.NoError(t, store.SetToken(ref, "token"))
	assert.FileExists(t, filepath.Join(identityDirPath(dir), identityFileName("prod")))

	fileDir := t.TempDir()
	require.NoError(t, NewFileStore(fileDir).SetToken(ref, "token"))
	assert.NoDirExists(t, identityDirPath(fileDir), "a file store keeps its credential in the directory already")
}

// TestKeychainCopyThenReloginInTheOriginal asserts the case the design
// names first: a copied directory shares its original's identity until the
// original stores something new, and then learns it loud rather than using
// a credential it never stored. The copy's other entry keeps working, and
// the copy's own login separates that one entry alone.
func TestKeychainCopyThenReloginInTheOriginal(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	staging := ServerRef{Name: "staging", URL: "https://staging.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-1"))
	require.NoError(t, a.SetToken(staging, "staging-1"))

	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	b := newPinnedKeyringStore(dirB, false)

	tok, err := b.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-1", tok, "a copy inherits what it was copied with")

	require.NoError(t, a.SetToken(prod, "token-2"))

	_, err = b.GetToken(prod)
	require.ErrorContains(t, err, "was replaced by another config directory")

	tok, err = b.GetToken(staging)
	require.NoError(t, err)
	assert.Equal(t, "staging-1", tok, "identity is per entry: staging is untouched")

	beforeA := recordFor(t, dirA, "prod")
	require.NoError(t, b.SetToken(prod, "token-b"))
	afterB := recordFor(t, dirB, "prod")
	assert.NotEqual(t, beforeA.ID, afterB.ID, "the copy rotates its own identity")

	tok, err = a.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-2", tok, "the original is never touched by the copy's rotation")

	assert.Equal(t, beforeA, recordFor(t, dirA, "prod"))
	assert.Equal(t, recordFor(t, dirA, "staging").ID, recordFor(t, dirB, "staging").ID,
		"the entry that did not diverge still shares its identity")

	// Both entries are still reachable by name for a logout of everything
	// the copy holds, the rotated one and the inherited one alike.
	entries, err := IdentityEntries(dirB)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"prod", "staging"}, entries)
	for _, entry := range entries {
		_, _, derr := DeleteLoginToken(t.Context(), dirB, ServerRef{Name: entry}, "keychain")
		require.NoError(t, derr)
	}
	left, err := IdentityEntries(dirB)
	require.NoError(t, err)
	assert.Empty(t, left, "nothing of the copy's is left stranded")

	tok, err = a.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-2", tok, "the original's rotated entry is untouched by the copy's logout")
}

// TestKeychainCopyThenLogoutInTheCopy asserts the shared-slot case: both
// directories hold the same credential, so either one removing it is what
// logout means, and the other reads "not logged in", loud.
func TestKeychainCopyThenLogoutInTheCopy(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-1"))

	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)

	_, _, err := DeleteLoginToken(t.Context(), dirB, prod, "keychain")
	require.NoError(t, err)

	_, gerr := a.GetToken(prod)
	require.ErrorIs(t, gerr, ErrNoToken)

	// The original's login finds an absent slot and writes under the
	// identity it still has.
	before := recordFor(t, dirA, "prod")
	require.NoError(t, a.SetToken(prod, "token-3"))
	after := recordFor(t, dirA, "prod")
	assert.Equal(t, before.ID, after.ID, "an absent slot is a logged-out entry, not a divergence")
}

// TestKeychainCopyThenReloginThenTheOriginalIsGone asserts the case an
// ownership registry gets wrong: the original logs in as another user and
// is then removed entirely, and the copy still refuses the credential it
// never stored. Nothing about the verdict depends on the original being
// there to ask.
func TestKeychainCopyThenReloginThenTheOriginalIsGone(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA := t.TempDir()
	a := newPinnedKeyringStore(dirA, false)
	require.NoError(t, a.SetToken(prod, "token-user-one"))

	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	b := newPinnedKeyringStore(dirB, false)

	// The original logs in as somebody else, then goes away.
	require.NoError(t, a.SetToken(prod, "token-user-two"))
	require.NoError(t, os.RemoveAll(dirA))

	_, err := b.GetToken(prod)
	require.ErrorContains(t, err, "was replaced by another config directory")

	// And keeps refusing it on every later command, not just the first.
	_, err = b.GetToken(prod)
	require.ErrorContains(t, err, "was replaced by another config directory")
}

// TestKeychainMoveOrRenameChangesNothing asserts that no path is compared:
// a directory moved with its records reads exactly as before.
func TestKeychainMoveOrRenameChangesNothing(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-1"))

	moved := t.TempDir()
	copyIdentityDir(t, dirA, moved)
	tok, err := newPinnedKeyringStore(moved, false).GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-1", tok)
}

// TestKeychainCrashBeforeTheSetRevertsAndStoresItsOwn asserts the crash
// point between the pending record and the credential write: the next login
// finds the pending record, sees the intended credential did not land,
// goes back to the identity the entry had, and then stores its own.
func TestKeychainCrashBeforeTheSetRevertsAndStoresItsOwn(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir, store := keychainDir(t)
	ks := store.(*keyringStore)
	require.NoError(t, ks.SetToken(prod, "token-1"))
	first := recordFor(t, dir, "prod")

	// A login that recorded its intent and ended before the write.
	_, err := beginKeychainWrite(t.Context(), ks, prod, "token-2")
	require.NoError(t, err)
	assert.Equal(t, identityPending, recordFor(t, dir, "prod").State)
	_, err = ks.GetToken(prod)
	require.ErrorContains(t, err, "did not complete")

	require.NoError(t, ks.SetToken(prod, "token-3"))
	final := recordFor(t, dir, "prod")
	assert.Equal(t, identityActive, final.State)
	assert.Equal(t, first.ID, final.ID, "the reverted login left the entry on its own identity")

	tok, err := ks.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-3", tok)
}

// TestKeychainCrashAfterTheSetStoresTheSecondUsersToken asserts the crash
// point after the credential write: the next login settles the record for
// the credential that did land, then stores its own over it, so a second
// user's login never leaves the first user's token in place.
func TestKeychainCrashAfterTheSetStoresTheSecondUsersToken(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir, store := keychainDir(t)
	ks := store.(*keyringStore)

	write, err := beginKeychainWrite(t.Context(), ks, prod, "first-user")
	require.NoError(t, err)
	_, err = ks.SetTokenWithOutcome(prod, "first-user", write.Prior)
	require.NoError(t, err)
	// The process ends before the record is settled.
	assert.Equal(t, identityPending, recordFor(t, dir, "prod").State)

	require.NoError(t, ks.SetToken(prod, "second-user"))
	tok, err := ks.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "second-user", tok)
	assert.Equal(t, identityActive, recordFor(t, dir, "prod").State)
}

// TestKeychainRotationCrashRetriesWithoutTouchingTheOtherAccount asserts
// the rotation crash point: a rotation that ended before its write is
// reverted, judged again, and rotates again, and the account the other
// copy owns is never written at any step.
func TestKeychainRotationCrashRetriesWithoutTouchingTheOtherAccount(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-a"))
	shared := recordFor(t, dirA, "prod")

	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	b := newPinnedKeyringStore(dirB, false).(*keyringStore)

	// The original stores something new, so the copy has diverged.
	require.NoError(t, a.SetToken(prod, "token-a2"))

	// The copy's login rotates and then ends before its write.
	write, err := beginKeychainWrite(t.Context(), b, prod, "token-b")
	require.NoError(t, err)
	firstRotation := write.pending.ID
	assert.NotEqual(t, shared.ID, firstRotation)

	require.NoError(t, b.SetToken(prod, "token-b2"))
	final := recordFor(t, dirB, "prod")
	assert.NotEqual(t, shared.ID, final.ID, "the retry rotated again")
	assert.NotEqual(t, firstRotation, final.ID, "and not to the identity the abandoned attempt chose")

	tok, err := a.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-a2", tok, "the other directory's account was never written")

	tok, err = b.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-b2", tok)
}

// TestKeychainRotationCompensationRestoresTheOldIdentity asserts the
// compensation of a rotated login: the new account is deleted, the record
// goes back to the identity and fingerprint the entry had, and the account
// the other copy owns is untouched.
func TestKeychainRotationCompensationRestoresTheOldIdentity(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-a"))

	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	b := newPinnedKeyringStore(dirB, false).(*keyringStore)
	before := recordFor(t, dirB, "prod")

	require.NoError(t, a.SetToken(prod, "token-a2"))

	write, err := beginKeychainWrite(t.Context(), b, prod, "token-b")
	require.NoError(t, err)
	out, err := b.SetTokenWithOutcome(prod, "token-b", write.Prior)
	require.NoError(t, err)
	require.NoError(t, write.finish(t.Context(), out.State))
	rotated := recordFor(t, dirB, "prod")

	require.NotNil(t, out.Compensate)
	require.NoError(t, out.Compensate())
	require.NoError(t, write.revert(t.Context()))

	assert.Equal(t, before, recordFor(t, dirB, "prod"), "the entry is back on its old identity")
	_, gerr := keyring.Get(keyringService, rotated.Account())
	require.Error(t, gerr, "the account the rotation created is gone")

	tok, gerr := a.GetToken(prod)
	require.NoError(t, gerr)
	assert.Equal(t, "token-a2", tok, "the other directory keeps its credential")
}

// TestKeychainLogoutAfterACrashBeforeTheSetDeletesThePriorCredential
// asserts that a logout meeting an unsettled record still removes the
// credential the directory actually has, which sits under the identity the
// unsettled write would have gone back to.
func TestKeychainLogoutAfterACrashBeforeTheSetDeletesThePriorCredential(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir, store := keychainDir(t)
	ks := store.(*keyringStore)
	require.NoError(t, ks.SetToken(prod, "token-1"))
	first := recordFor(t, dir, "prod")

	dirB := t.TempDir()
	copyIdentityDir(t, dir, dirB)
	// A copy stores something new, so this directory's next login rotates
	// and then ends before its write.
	require.NoError(t, newPinnedKeyringStore(dirB, false).SetToken(prod, "token-b"))
	_, err := beginKeychainWrite(t.Context(), ks, prod, "token-2")
	require.NoError(t, err)

	from, _, derr := DeleteLoginToken(t.Context(), dir, prod, "keychain")
	require.NoError(t, derr)
	assert.NotContains(t, from, "keychain", "the shared account is the copy's now")

	// The copy's credential is the one under the shared account and is left
	// alone; this directory's record is gone.
	tok, gerr := keyring.Get(keyringService, first.Account())
	require.NoError(t, gerr)
	assert.Equal(t, "token-b", tok)
	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.Nil(t, rec)
}

// TestKeychainCredentialReadOperationCount asserts what a credential read
// costs: one stat of the identity directory, one no-follow open of it, one
// stat and one read of the record, exactly one keychain read, and no lock
// file of any kind.
func TestKeychainCredentialReadOperationCount(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	_, store := keychainDir(t)
	require.NoError(t, store.SetToken(prod, "token-1"))

	var fileOps, keyOps []string
	locks := 0
	restoreFile := SetIdentityObserverForTest(func(op string) { fileOps = append(fileOps, op) })
	t.Cleanup(restoreFile)
	SetKeyringObserverForTest(func(op string) { keyOps = append(keyOps, op) })
	t.Cleanup(func() { SetKeyringObserverForTest(nil) })
	restoreLock := SetWaitForFlockHookForTest(func() { locks++ })
	t.Cleanup(restoreLock)

	tok, err := store.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-1", tok)

	assert.Equal(t, []string{"dirstat", "diropen", "recordstat", "recordread"}, fileOps)
	assert.Equal(t, []string{"get"}, keyOps)
	assert.Zero(t, locks, "a read takes no lock")
}

// TestKeychainReadWithoutAnIdentityMakesNoKeychainCall asserts the cheapest
// path: a directory with no identity for the entry stops at one stat and
// never reaches the keychain.
func TestKeychainReadWithoutAnIdentityMakesNoKeychainCall(t *testing.T) {
	keyring.MockInit()
	_, store := keychainDir(t)

	var fileOps, keyOps []string
	restoreFile := SetIdentityObserverForTest(func(op string) { fileOps = append(fileOps, op) })
	t.Cleanup(restoreFile)
	SetKeyringObserverForTest(func(op string) { keyOps = append(keyOps, op) })
	t.Cleanup(func() { SetKeyringObserverForTest(nil) })

	_, err := store.GetToken(ServerRef{Name: "prod"})
	require.ErrorIs(t, err, ErrNoToken)
	assert.Equal(t, []string{"dirstat"}, fileOps)
	assert.Empty(t, keyOps)
}

// TestKeychainLoginReadsTheSlotOnce asserts the login's one keychain
// pre-read feeds both the divergence verdict and the prior state: an
// ordinary re-login reads the account once, writes once, and does no
// read-back.
func TestKeychainLoginReadsTheSlotOnce(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	_, store := keychainDir(t)
	require.NoError(t, store.SetToken(prod, "token-1"))

	var keyOps []string
	SetKeyringObserverForTest(func(op string) { keyOps = append(keyOps, op) })
	t.Cleanup(func() { SetKeyringObserverForTest(nil) })

	require.NoError(t, store.SetToken(prod, "token-2"))
	assert.Equal(t, []string{"get", "set"}, keyOps)
}

// TestKeychainStoreClassifiesTheOrdinaryReloginPrior asserts what the
// login hands the store on a re-login: the same account, the previous
// credential, and a keychain prior source, so a compensation restores the
// exact value rather than deleting the account.
func TestKeychainStoreClassifiesTheOrdinaryReloginPrior(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir, store := keychainDir(t)
	ks := store.(*keyringStore)
	require.NoError(t, ks.SetToken(prod, "token-1"))
	rec := recordFor(t, dir, "prod")

	write, err := beginKeychainWrite(t.Context(), ks, prod, "token-2")
	require.NoError(t, err)
	assert.Equal(t, rec.Account(), write.Prior.Account)
	assert.Equal(t, clierr.PriorKeyring, write.Prior.Source)
	assert.Equal(t, "token-1", write.Prior.Raw)
}

// TestKeychainDivergedLoginNeverCarriesTheOtherTokenAsPrior asserts the
// rotation's prior: a new account has no prior by construction, so the
// value read from the slot the other directory owns is never restored
// anywhere.
func TestKeychainDivergedLoginNeverCarriesTheOtherTokenAsPrior(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-a"))
	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	b := newPinnedKeyringStore(dirB, false).(*keyringStore)
	require.NoError(t, a.SetToken(prod, "token-a2"))

	write, err := beginKeychainWrite(t.Context(), b, prod, "token-b")
	require.NoError(t, err)
	assert.Equal(t, clierr.PriorNone, write.Prior.Source)
	assert.Empty(t, write.Prior.Raw)
	assert.NotEqual(t, recordFor(t, dirA, "prod").Account(), write.Prior.Account)
}

// TestKeychainWriteSettlesTheRecordByOutcome asserts the fifth step of
// the login sequence against each outcome the keychain can report: the
// record becomes active for both committed states, stays pending for the
// one outcome the keychain could not account for, so the next login
// settles it, and goes away when nothing was written.
func TestKeychainWriteSettlesTheRecordByOutcome(t *testing.T) {
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	cases := []struct {
		name  string
		state CommitState
		want  identityState
		gone  bool
	}{
		{"committed", CommitCommitted, identityActive, false},
		{"committed, no way back", CommitCommittedUncompensatable, identityActive, false},
		{"possibly committed", CommitPossiblyCommitted, identityPending, false},
		{"not written", CommitNotWritten, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			dir, store := keychainDir(t)
			ks := store.(*keyringStore)

			write, err := beginKeychainWrite(t.Context(), ks, prod, "token-1")
			require.NoError(t, err)
			require.NoError(t, write.finish(t.Context(), tc.state))

			rec, err := readIdentityRecord(dir, "prod")
			require.NoError(t, err)
			if tc.gone {
				assert.Nil(t, rec, "a write that stored nothing leaves no identity behind")
				return
			}
			require.NotNil(t, rec)
			assert.Equal(t, tc.want, rec.State)
			assert.Equal(t, write.pending.ID, rec.ID)
		})
	}
}

// TestKeychainCrashAfterTheReadBackLeavesThePendingRecord asserts the
// crash point between the read-back that proved the write landed and the
// record's settlement: the record stays pending, a reader says so, and the
// next login settles it for the credential that is actually there.
func TestKeychainCrashAfterTheReadBackLeavesThePendingRecord(t *testing.T) {
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir := t.TempDir()
	// A keychain whose write lands and then reports an error, so the store
	// reads back to classify it.
	withFakeKeyring(t, &fakeKeyring{items: map[string]string{}, setErr: errors.New("dbus: reply lost"), commitThenErr: true})
	ks := newPinnedKeyringStore(dir, false).(*keyringStore)

	write, err := beginKeychainWrite(t.Context(), ks, prod, "token-1")
	require.NoError(t, err)
	out, err := ks.SetTokenWithOutcome(prod, "token-1", write.Prior)
	require.NoError(t, err)
	require.Equal(t, CommitCommitted, out.State)
	require.NotEmpty(t, out.Notice, "the keychain reported an error after storing")
	// The process ends here, before finish.

	assert.Equal(t, identityPending, recordFor(t, dir, "prod").State)
	_, gerr := ks.GetToken(prod)
	require.ErrorContains(t, gerr, "did not complete")

	require.NoError(t, ks.SetToken(prod, "token-2"))
	tok, err := ks.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-2", tok)
}

// TestKeychainLogoutRacingALoginIsLoud asserts the interleaving the design
// accepts and the property it keeps: a delete in one copy can land between
// another copy's compare and its own write, and the result is a loud
// "not logged in" in the other process, never a credential it did not
// store.
func TestKeychainLogoutRacingALoginIsLoud(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-1"))
	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)

	// B compares and matches.
	recB := recordFor(t, dirB, "prod")
	stored, err := keyring.Get(keyringService, recB.Account())
	require.NoError(t, err)
	require.Equal(t, recB.Fingerprint, credentialFingerprint(recB.ID, stored))

	// A logs in between B's compare and B's delete.
	require.NoError(t, a.SetToken(prod, "token-2"))

	// B deletes what it compared.
	require.NoError(t, keyring.Delete(keyringService, recB.Account()))

	_, gerr := a.GetToken(prod)
	require.ErrorIs(t, gerr, ErrNoToken, "loud in A's process, never another identity's credential")

	// A rerun writes and records under the identity A still has.
	before := recordFor(t, dirA, "prod")
	require.NoError(t, a.SetToken(prod, "token-3"))
	assert.Equal(t, before.ID, recordFor(t, dirA, "prod").ID)
}

// TestKeychainTwoCopiesLoggingInAtOnce asserts the accepted cross-copy
// interaction: both copies classify against the credential they share,
// both writes land, both logins succeed, and the loser learns it on its
// next read and separates with one login.
func TestKeychainTwoCopiesLoggingInAtOnce(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dirA, a := keychainDir(t)
	require.NoError(t, a.SetToken(prod, "token-1"))
	dirB := t.TempDir()
	copyIdentityDir(t, dirA, dirB)
	ksA, ksB := a.(*keyringStore), newPinnedKeyringStore(dirB, false).(*keyringStore)

	// Both read the shared credential before either writes.
	wa, err := beginKeychainWrite(t.Context(), ksA, prod, "token-a")
	require.NoError(t, err)
	wb, err := beginKeychainWrite(t.Context(), ksB, prod, "token-b")
	require.NoError(t, err)
	assert.Equal(t, wa.Prior.Account, wb.Prior.Account, "one shared account until they diverge")

	outA, err := ksA.SetTokenWithOutcome(prod, "token-a", wa.Prior)
	require.NoError(t, err)
	require.NoError(t, wa.finish(t.Context(), outA.State))
	outB, err := ksB.SetTokenWithOutcome(prod, "token-b", wb.Prior)
	require.NoError(t, err)
	require.NoError(t, wb.finish(t.Context(), outB.State))

	tok, err := ksB.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-b", tok, "the last write wins the shared account")

	_, gerr := ksA.GetToken(prod)
	require.ErrorContains(t, gerr, "was replaced by another config directory")

	before := recordFor(t, dirA, "prod")
	require.NoError(t, ksA.SetToken(prod, "token-a2"))
	assert.NotEqual(t, before.ID, recordFor(t, dirA, "prod").ID, "the loser separates with one login")
	tok, err = ksB.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-b", tok, "and leaves the winner alone")
}

// TestKeychainNeverReadsAFileTokenAfterAReloginOrRotation asserts the
// read-through stays gone in the two states that used to reach it behind a
// keychain declaration: a directory that re-logged in, and a copy that
// rotated. In both, a token left in credentials.yaml is never served.
func TestKeychainNeverReadsAFileTokenAfterAReloginOrRotation(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}

	t.Run("after a re-login", func(t *testing.T) {
		dir, store := keychainDir(t)
		require.NoError(t, NewFileStore(dir).SetToken(prod, "file-token"))
		require.NoError(t, store.SetToken(prod, "token-1"))
		require.NoError(t, store.SetToken(prod, "token-2"))
		require.NoError(t, store.DeleteToken(prod))

		_, err := store.GetToken(prod)
		require.ErrorIs(t, err, ErrNoToken)
		tok, ferr := NewFileStore(dir).GetToken(prod)
		require.NoError(t, ferr)
		assert.Equal(t, "file-token", tok, "the file entry is still there, and still not read")
	})

	t.Run("after a rotation", func(t *testing.T) {
		dirA, a := keychainDir(t)
		require.NoError(t, a.SetToken(prod, "token-a"))
		dirB := t.TempDir()
		copyIdentityDir(t, dirA, dirB)
		require.NoError(t, NewFileStore(dirB).SetToken(prod, "file-token"))
		b := newPinnedKeyringStore(dirB, false)

		require.NoError(t, a.SetToken(prod, "token-a2"))
		require.NoError(t, b.SetToken(prod, "token-b"))
		require.NoError(t, b.DeleteToken(prod))

		_, err := b.GetToken(prod)
		require.ErrorIs(t, err, ErrNoToken)
	})
}

// TestDeleteLoginTokenKeepsTheRecordWhenTheKeychainDeleteFails asserts
// what a failed keychain delete leaves behind: the credential is still
// there, so the record that names it stays too, on both arms - the
// directory that declares the keychain fails its logout, and the one that
// merely has a leftover record warns and keeps it for the next attempt.
func TestDeleteLoginTokenKeepsTheRecordWhenTheKeychainDeleteFails(t *testing.T) {
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}

	cases := []struct {
		name     string
		policy   string
		wantErr  bool
		wantWarn string
	}{
		{name: "the directory declares the keychain", policy: "keychain", wantErr: true},
		{name: "a leftover record on a file directory", policy: "file", wantWarn: "the credential identity record was kept"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(EnvCredentialStore, "")
			id, err := newKeychainAccountID()
			require.NoError(t, err)
			require.NoError(t, writeIdentityRecord(dir, identityRecord{
				Version:     identityVersion,
				Entry:       "prod",
				State:       identityActive,
				ID:          id,
				Fingerprint: credentialFingerprint(id, "ours"),
			}))
			account := keychainAccount(id, "prod")
			fk := &fakeKeyring{
				items:     map[string]string{account: "ours"},
				deleteErr: errors.New("keychain refused the delete"),
			}
			withFakeKeyring(t, fk)

			from, notices, derr := DeleteLoginToken(t.Context(), dir, prod, tc.policy)
			assert.NotContains(t, from, "keychain")
			if tc.wantErr {
				require.ErrorContains(t, derr, "keychain refused the delete")
			} else {
				require.NoError(t, derr)
				require.Len(t, notices, 1)
				assert.Contains(t, notices[0], tc.wantWarn)
			}

			assert.Equal(t, map[string]string{account: "ours"}, fk.items,
				"the credential is still there")
			rec, rerr := readIdentityRecord(dir, "prod")
			require.NoError(t, rerr)
			require.NotNil(t, rec, "so the record that names it stays")
			assert.Equal(t, id, rec.ID)
		})
	}
}

// TestKeychainFirstLoginCrashBeforeTheSetStartsFresh asserts the crash
// point a first login has and a re-login does not: the pending record
// names no earlier identity, so the next login has nothing to go back to
// and must start over rather than leave the entry unsettled for good.
func TestKeychainFirstLoginCrashBeforeTheSetStartsFresh(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}
	dir, store := keychainDir(t)
	ks := store.(*keyringStore)

	write, err := beginKeychainWrite(t.Context(), ks, prod, "token-1")
	require.NoError(t, err)
	pending := recordFor(t, dir, "prod")
	require.Equal(t, identityPending, pending.State)
	require.Nil(t, pending.Prior, "a first login has nothing to revert to")
	// The process ends here: the credential was never written.

	_, gerr := ks.GetToken(prod)
	require.ErrorContains(t, gerr, "did not complete")

	require.NoError(t, ks.SetToken(prod, "token-2"))
	final := recordFor(t, dir, "prod")
	assert.Equal(t, identityActive, final.State)
	assert.NotEqual(t, write.pending.ID, final.ID, "the abandoned identity is not reused")
	tok, err := ks.GetToken(prod)
	require.NoError(t, err)
	assert.Equal(t, "token-2", tok)
}

// TestIdentityErrorsCarryTheIdentityMarker asserts the one classification
// every caller outside this package branches on. Each way the identity
// layer can fail marks its error, so a credential read that cannot say
// who an entry is can be told apart from a store that could not be
// reached; the marker rides as the cause, so the typed error a command
// renders - its code, hint and exit status - is unchanged.
func TestIdentityErrorsCarryTheIdentityMarker(t *testing.T) {
	keyring.MockInit()
	prod := ServerRef{Name: "prod", URL: "https://prod.ex"}

	dir, store := keychainDir(t)
	require.NoError(t, os.MkdirAll(identityDirPath(dir), identityDirPerm))
	require.NoError(t, os.WriteFile(
		filepath.Join(identityDirPath(dir), identityFileName("prod")),
		[]byte("not a record"), identityFilePerm))

	_, err := readIdentityRecord(dir, "prod")
	require.Error(t, err)
	assert.True(t, IsCredentialIdentityError(err), "a record that does not parse")

	_, err = listIdentityRecords(dir)
	require.Error(t, err)
	assert.True(t, IsCredentialIdentityError(err), "the same file through the enumerator")

	_, err = store.GetToken(prod)
	require.Error(t, err)
	assert.True(t, IsCredentialIdentityError(err), "and through a credential read")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "the marker rides as the cause; the typed error is still what renders")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 2, ce.ExitCode())

	assert.True(t, IsCredentialIdentityError(credentialReplacedError("prod")))
	assert.True(t, IsCredentialIdentityError(loginIncompleteError("prod")))

	notADirectory := t.TempDir()
	require.NoError(t, os.WriteFile(identityDirPath(notADirectory), []byte("x"), identityFilePerm))
	_, err = readIdentityRecord(notADirectory, "prod")
	require.Error(t, err)
	assert.True(t, IsCredentialIdentityError(err), "a directory that cannot be trusted")

	assert.False(t, IsCredentialIdentityError(ErrNoToken), "not logged in is not an identity failure")
	assert.False(t, IsCredentialIdentityError(errors.New(`keyring get "prod": dbus: no session bus`)),
		"a store that could not be reached says nothing about identity")
	assert.False(t, IsCredentialIdentityError(nil))

	fresh := t.TempDir()
	rec, err := readIdentityRecord(fresh, "prod")
	require.NoError(t, err)
	assert.Nil(t, rec, "an absent identity is not an error at all")
}

// TestIdentityEntriesReachesAnEntryRemovedByHand asserts what `logout
// --all` enumerates on top of the configured servers: an entry whose config
// record is gone still has a credential, and the identity record is the
// only thing that names it.
func TestIdentityEntriesReachesAnEntryRemovedByHand(t *testing.T) {
	keyring.MockInit()
	dir, store := keychainDir(t)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "token-1"))
	require.NoError(t, store.SetToken(ServerRef{Name: "gone"}, "token-2"))

	entries, err := IdentityEntries(dir)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"prod", "gone"}, entries)
}
