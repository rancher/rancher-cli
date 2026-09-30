package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// repinFixture is an `auto` directory pinned to the file store, holding
// the logging-in entry's old token beside another entry with no stored
// credential, on a machine whose keychain works: the state the auto repin
// starts from.
func repinFixture(t *testing.T) string {
	t.Helper()
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	orig := keyringUsable
	keyringUsable = func() bool { return true }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

	cfg := &Config{CredentialStore: "auto", CurrentServer: "prod", Servers: map[string]Server{
		"prod":    {URL: "https://r.ex"},
		"staging": {URL: "https://staging.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, WritePin(dir, backendFile))
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
	return dir
}

// repinInput is the login that runs on that directory: policy auto, no
// declaration change, the production opener.
func repinInput() PersistLoginInput {
	return PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance:          clierr.ProvenanceDerived,
		Policy:              "auto",
		ObservedDeclaration: "auto",
		Declaration:         DeclarationFor("auto", "", "auto"),
		OpenStore:           OpenResolvedStore,
	}
}

// keychainTokenFor reads what the keychain holds for an entry through the
// entry's own identity record, which is the only thing that names the
// account.
func keychainTokenFor(t *testing.T, dir, entry string) string {
	t.Helper()
	rec, err := readIdentityRecord(dir, entry)
	require.NoError(t, err)
	if rec == nil {
		return ""
	}
	tok, gerr := keyringGet(rec.Account())
	if gerr != nil {
		return ""
	}
	return tok
}

// TestTheAutoRepinWritesNoPinUntilTheChangeFinishes asserts the whole
// order for the repin: the credential commits to the keychain, this
// entry's old file copy is removed, nothing is written for any other
// entry, and the pin - the one thing that makes a reader resolve to the
// new store - is written last, after the commit rather than before it.
func TestTheAutoRepinWritesNoPinUntilTheChangeFinishes(t *testing.T) {
	dir := repinFixture(t)

	pinAtWrite := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		pinAtWrite[step] = ReadPin(dir)
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)

	assert.Equal(t, backendFile, pinAtWrite["entry"], "the pin is untouched before the commit")
	assert.Equal(t, backendFile, pinAtWrite["transition"], "and still is when the change is put into effect")
	assert.Equal(t, backendKeychain, ReadPin(dir), "the change's last step is what flips it")

	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"), "the credential committed to the keychain")
	assert.Empty(t, storedFileToken(t, dir), "this entry's old copy is gone")
	rec, rerr := readIdentityRecord(dir, "staging")
	require.NoError(t, rerr)
	assert.Nil(t, rec, "nothing was written for the entry that did not log in")
	assert.NoFileExists(t, TransitionPath(dir), "a change with nothing left removes its own record")
	assert.NotEmpty(t, out.CacheBindingID)
}

// TestAFailedLoginLeavesThePinAndEveryFileTokenAsTheyWere asserts what a
// login that never commits is allowed to have changed: nothing a reader
// can see. The pin still names the store the tokens are in, every token is
// where it was, and no record of a change is left behind.
func TestAFailedLoginLeavesThePinAndEveryFileTokenAsTheyWere(t *testing.T) {
	dir := repinFixture(t)
	in := repinInput()
	in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})

	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)

	assert.Equal(t, backendFile, ReadPin(dir), "a login that failed decides nothing")
	assert.Equal(t, "prod-old", storedFileToken(t, dir))
	assert.NoFileExists(t, TransitionPath(dir), "the record of a change that did not happen goes with it")

	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.CredentialTransitionID, "and is never named")
}

// TestAFailureBeforeActivationRemovesTheManifest asserts the rule for
// every path that ends before the config write that puts a change into
// effect: the record of the change goes, so the next command has nothing
// to replay for a login that never committed.
func TestAFailureBeforeActivationRemovesTheManifest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, in *PersistLoginInput)
	}{
		{name: "the store cannot be opened", setup: func(_ *testing.T, in *PersistLoginInput) {
			in.OpenStore = func(string, string) (CredStore, string, error) {
				return nil, "", assert.AnError
			}
		}},
		{name: "the keychain write cannot begin", setup: func(t *testing.T, _ *PersistLoginInput) {
			t.Helper()
			// The first identity write is the pending record that
			// addresses the new keychain account.
			t.Cleanup(SetIdentityWriteHookForTest(func() error { return errors.New("read-only file system") }))
		}},
		{name: "the commit fails", setup: func(_ *testing.T, in *PersistLoginInput) {
			in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
		}},
		{name: "the credential landed and its identity record could not be settled", setup: func(t *testing.T, _ *PersistLoginInput) {
			t.Helper()
			writes := 0
			t.Cleanup(SetIdentityWriteHookForTest(func() error {
				writes++
				if writes >= 2 {
					return errors.New("read-only file system")
				}
				return nil
			}))
		}},
		{name: "a new entry's credential may have been stored", setup: func(_ *testing.T, in *PersistLoginInput) {
			in.Name, in.CanonicalURL, in.OwnsEntry = "fresh", "https://fresh.ex", true
			in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: CommitPossiblyCommitted}, err: errors.New("connection reset")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := repinFixture(t)
			in := repinInput()
			tc.setup(t, &in)

			_, err := PersistLoginCredential(t.Context(), dir, in)
			require.Error(t, err)
			assert.NoFileExists(t, TransitionPath(dir))
			cfg, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			assert.Empty(t, cfg.CredentialTransitionID)
		})
	}
}

// TestTheLoginOrderActivatesTheManifestWithItsFirstConfigWrite asserts
// that the change is named by the first config write after the commit and
// by no write of its own: a change named before the credential landed
// would be replayed for a login that never happened, and a second write
// would be a window in which the change is neither inert nor in effect.
func TestTheLoginOrderActivatesTheManifestWithItsFirstConfigWrite(t *testing.T) {
	dir := repinFixture(t)

	var steps []string
	idAtWrite := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		steps = append(steps, step)
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		idAtWrite[step] = cfg.CredentialTransitionID
		return nil
	})
	defer restore()

	_, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)

	assert.Equal(t, []string{"entry", "transition", "resume"}, steps,
		"a repin changes neither the declaration nor trust, so the write that names the change is its own")
	assert.Empty(t, idAtWrite["entry"], "nothing names the change before the commit")
	assert.Empty(t, idAtWrite["transition"], "the write that names it is the one that has not landed yet")
	assert.NotEmpty(t, idAtWrite["resume"], "and by the next write it is named")
}

// TestActivationThatLosesTheManifestIsANamedState asserts what a login
// says when the config write put the change into effect and the record of
// what the change has to do is gone: no command can finish work nobody
// has the record of, so the login names the state rather than reporting a
// success that did not happen.
func TestActivationThatLosesTheManifestIsANamedState(t *testing.T) {
	for _, tc := range []struct {
		name string
		// step is the write this login makes to put the change into
		// effect; every arm of that switch writes the name, so every arm
		// has to look for the record afterwards.
		step  string
		trust *PendingTrust
		// replace puts a different change in its place instead of
		// removing it, which is the other way a process with the flock
		// skipped loses this one.
		replace bool
		// want is what the login says it found, which differs by state:
		// a record that is gone reads as one this login did not write, a
		// record replaced by another cannot be read at all.
		want string
	}{
		{name: "the change's own write", step: "transition",
			want: "no longer the one this login wrote"},
		{name: "the write that also carries a trust change", step: "trust",
			trust: &PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", Fingerprint: "f1"},
			want:  "no longer the one this login wrote"},
		{name: "the record replaced rather than removed", step: "transition", replace: true,
			want: "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := repinFixture(t)
			in := repinInput()
			in.PendingTrust = tc.trust

			restore := SetPersistWriteHookForTest(func(step string) error {
				if step != tc.step {
					return nil
				}
				// A concurrent command with the flock skipped, in the one
				// window where that is visible: between this write being
				// decided and the record being looked for again.
				if rerr := os.Remove(TransitionPath(dir)); rerr != nil {
					return rerr
				}
				if tc.replace {
					seedActivePin(t, dir)
					// seedActivePin names its own change; this login's
					// write is about to name a different one.
					cfg, lerr := Load(FilePath(dir))
					require.NoError(t, lerr)
					cfg.CredentialTransitionID = ""
					require.NoError(t, cfg.Write())
				}
				return nil
			})
			defer restore()

			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.Error(t, err)
			require.ErrorContains(t, err, "the record of the credential store change")
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, "the change did not run")
			if tc.want == "cannot be read" {
				// Running the login again meets the same file, and the
				// read's own cause says what to do with it.
				ce, ok := errorsAsClierr(err)
				require.True(t, ok)
				assert.Equal(t, "rancher config status", ce.Hint, "the report that shows the file and why it cannot be used")
			}
			assert.Equal(t, clierr.ConfigTransitionLost, out.ConfigState)
			assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"), "the credential is committed and stays committed")
		})
	}
}

// TestATrustWriteFailureNeverActivatesTheChange asserts that the write
// which would have put the change into effect, having failed, leaves
// nothing of it behind. The config field that names a change is written
// by the same call as the declaration and the trust, so a rollback that
// restored only the declaration would put into effect the very change it
// is undoing, and the next command would replay it for a login whose
// credential was compensated away.
func TestATrustWriteFailureNeverActivatesTheChange(t *testing.T) {
	dir := repinFixture(t)
	in := repinInput()
	in.PendingTrust = &PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("config directory is full")
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, clierr.CredentialCompensated, out.CredentialState)

	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "auto", cfg.CredentialStore, "the directory declares what it declared")
	assert.Empty(t, cfg.CredentialTransitionID, "and names no change")
	assert.NoFileExists(t, TransitionPath(dir), "the record of the change goes with it")
	assert.Equal(t, backendFile, ReadPin(dir))
	assert.Equal(t, "prod-old", storedFileToken(t, dir), "every credential is where it was")
}

// TestACommitWithNoActivationLeavesTheChangeUnnamed asserts the crash
// boundary between the credential commit and the write that puts the
// change into effect: the credential is in the destination, and the
// change - which nothing has named - is removed rather than left for a
// config write that will never come.
func TestACommitWithNoActivationLeavesTheChangeUnnamed(t *testing.T) {
	dir := repinFixture(t)

	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "transition" {
			return errors.New("config directory is full")
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.Error(t, err)
	require.ErrorContains(t, err, "starting the credential store change failed")
	assert.Equal(t, clierr.ConfigTransitionNotRecorded, out.ConfigState)

	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"), "the credential is committed and stays committed")
	assert.NoFileExists(t, TransitionPath(dir))
	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, cfg.CredentialTransitionID)
	assert.Equal(t, backendFile, ReadPin(dir), "the change never ran, so the pin still says where the tokens are")
}

// mustFileToken is one entry's token in credentials.yaml.
func mustFileToken(t *testing.T, dir, entry, url string) string {
	t.Helper()
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: entry, URL: url})
	require.NoError(t, err)
	return tok
}

// TestTheOldCopyIsRemovedBetweenTheFirstWriteAndTheLast asserts where the
// old copy's delete sits while the pin still points at the store being
// left: after the first write, which advanced the entry's cache binding
// id and paused its cache writes before any of its credential was
// touched, and before the last write, which resumes them.
func TestTheOldCopyIsRemovedBetweenTheFirstWriteAndTheLast(t *testing.T) {
	dir := repinFixture(t)

	oldCopyAt := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		oldCopyAt[step] = storedFileToken(t, dir)
		return nil
	})
	defer restore()

	_, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)

	assert.Equal(t, "prod-old", oldCopyAt["entry"], "the first write lands before any of the entry's credential is touched")
	assert.Equal(t, "prod-old", oldCopyAt["transition"], "the old copy is still there when the change starts")
	assert.Empty(t, oldCopyAt["resume"], "and is gone before cache writes resume")
}

// breakPath makes every later read and write of a path fail, by putting
// something there that no read parses and no atomic rename can replace.
func breakPath(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
}

// TestWhenTheOldCopyCannotGoThePinFlipsFirst asserts the fallback: the old copy
// of this entry's credential cannot be removed, so the pin flips in the
// old-copy step instead. Either one stops a reader from resolving to the old
// copy; the step that could not run stays, and says so, and holds no pause:
// readers have left the store it deletes from.
func TestWhenTheOldCopyCannotGoThePinFlipsFirst(t *testing.T) {
	dir := repinFixture(t)

	pinAt := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "transition" {
			// The write that puts the change into effect is the last
			// moment before the old-copy step, so this is where the file store
			// becomes unreadable: the credential is committed by now.
			breakPath(t, CredentialsFilePath(dir))
		}
		pinAt[step] = ReadPin(dir)
		return nil
	})
	defer restore()

	in := repinInput()

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err, "the credential is committed and recorded; the leftover work is a warning")
	assert.Equal(t, backendKeychain, pinAt["resume"], "the pin flipped before the last write")
	assert.NotEmpty(t, out.CacheBindingID)
	assert.False(t, out.CacheWritesPaused, "a delete step left holds no pause")

	require.NotEmpty(t, out.Warnings)
	assert.Contains(t, joinWarnings(out.Warnings), "prod", "the step that could not run names its entry")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m, "the work that is left stays recorded")
	assert.True(t, slices.ContainsFunc(m.Steps, func(s transitionStep) bool {
		return s.Kind == transitionDelete && s.Entry == "prod"
	}), "including the old copy nobody could remove")
}

// TestWhenTheOldCopyAndThePinBothFailTheLoginStopsPaused asserts the state
// a login stops in when it can neither remove the old copy nor flip the
// pin: readers still resolve the previous user's credential, the login
// names the state, the change stays active with both steps, and the pin
// step left keeps the entry's cache writes paused, under the id the first
// write advanced, until a later login finishes it.
func TestWhenTheOldCopyAndThePinBothFailTheLoginStopsPaused(t *testing.T) {
	dir := repinFixture(t)
	restore := SetPersistWriteHookForTest(func(step string) error {
		if step == "transition" {
			breakPath(t, CredentialsFilePath(dir))
			breakPath(t, PinPath(dir))
		}
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.Error(t, err)
	require.ErrorContains(t, err, "neither the old copy nor the credential store pin could be changed, so this directory still reads the previous credential")
	assert.Equal(t, clierr.ConfigStoreChangeNotApplied, out.ConfigState)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, "rancher auth login 'prod'", ce.Hint)

	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.NotEmpty(t, cfg.Servers["prod"].CacheBindingID, "the first write advanced the id")
	assert.Equal(t, PauseLocked, cfg.Servers["prod"].CacheWritesPaused, "and the pin step left keeps cache writes paused")
	assert.NotEmpty(t, cfg.CredentialTransitionID, "the change is still in effect")
	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.Len(t, m.Steps, 2, "the old copy and the pin both still have to happen")
}

// TestALoginProceedsPastARemainingDeleteForItsEntry asserts the prelude's
// gate for a login that changes no store: a remaining delete naming its
// entry does not stop it. The delete removes only the credential whose
// digest it carries, and the login's own does not match, so a headless
// file directory whose old keychain copy is unreachable can still log in.
func TestALoginProceedsPastARemainingDeleteForItsEntry(t *testing.T) {
	dir := settledFixture(t)
	seedActiveDelete(t, dir, "prod")

	out, err := PersistLoginCredential(t.Context(), dir, settledInput())
	require.NoError(t, err)
	assert.Equal(t, "id:new", storedFileToken(t, dir))
	assert.Contains(t, joinWarnings(out.Warnings), "could not finish for prod", "the step left behind says so")
	assert.FileExists(t, TransitionPath(dir), "and stays for the next command")
}

// TestAStoreChangeRefusesWhileAnotherIsUnfinished asserts the other arm:
// two store changes cannot stack, whatever entries they name. The second
// would forget the first, and the old copy the first had still to delete
// would be left in a backend nothing names.
func TestAStoreChangeRefusesWhileAnotherIsUnfinished(t *testing.T) {
	dir := repinFixture(t)
	seedActiveDelete(t, dir, "an-entry-this-login-never-touches")

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.Equal(t, clierr.ConfigNotWritten, out.ConfigState, "nothing was written")
	assert.Equal(t, backendFile, ReadPin(dir), "and nothing was decided")
}

// seedActiveDelete records a change that cannot finish, naming entry, and
// puts it into effect: the delete of an old keychain copy, on a keychain
// this test makes unreachable, so every replay leaves the step exactly
// where it is - work the directory still has.
func seedActiveDelete(t *testing.T, dir, entry string) {
	t.Helper()
	t.Cleanup(SetKeyringBackendForTest(unreachableKeyring{}))
	vid, err := newTransitionID()
	require.NoError(t, err)
	id, err := newTransitionID()
	require.NoError(t, err)
	m := transitionManifest{Version: transitionVersion, ID: id, Steps: []transitionStep{{
		Kind: transitionDelete, Entry: entry, Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "the-old-copy"),
		SourceAccountID: mustAccount(t),
	}}}
	require.NoError(t, writeTransition(dir, m))
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialTransitionID = id
	require.NoError(t, cfg.Write())
}

// settledFixture is a directory that declares the file store and has
// nothing to decide: a login on it changes no store at all.
func settledFixture(t *testing.T) string {
	t.Helper()
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]Server{
		"prod": {URL: "https://r.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
	return dir
}

func settledInput() PersistLoginInput {
	return PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance:          clierr.ProvenanceDerived,
		Policy:              "file",
		ObservedDeclaration: "file",
		Declaration:         DeclarationFor("file", "", "file"),
		OpenStore:           OpenResolvedStore,
	}
}

// TestTheBackendSetIsResolvedAfterTheReplay asserts that what the login
// does to the store is decided from the directory as the prelude left it.
// The prelude can finish a change - writing the pin among other things -
// so a set resolved before it would have the login recording a change that
// has already happened.
func TestTheBackendSetIsResolvedAfterTheReplay(t *testing.T) {
	dir := repinFixture(t)
	// A change with one step left: the pin this directory is about to get
	// anyway. The prelude runs it, and after that the repin has nothing
	// left to do.
	seedActivePin(t, dir)

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)

	assert.Equal(t, backendKeychain, ReadPin(dir), "the prelude wrote it")
	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Empty(t, after.CredentialTransitionID, "a change with no file left is forgotten by the next config write")
	assert.NoFileExists(t, TransitionPath(dir), "and the login recorded no change of its own")
	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"))
	assert.Contains(t, joinWarnings(out.Warnings), "re-pinned", "the pin write says so once")
	// What a set resolved before the replay would have done: seen the
	// directory still reading the file store and recorded a change of its
	// own - a delete of the old copy and a second pin - that the directory
	// had already made.
	assert.Equal(t, "prod-old", storedFileToken(t, dir),
		"the old copy a second change would have deleted is still there, since this login changed no store")
}

// seedActivePin records a change whose only step is the keychain pin, and
// puts it into effect.
func seedActivePin(t *testing.T, dir string) {
	t.Helper()
	id, err := newTransitionID()
	require.NoError(t, err)
	require.NoError(t, writeTransition(dir, transitionManifest{Version: transitionVersion, ID: id,
		Steps: []transitionStep{{Kind: transitionPin, Target: backendKeychain}}}))
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialTransitionID = id
	require.NoError(t, cfg.Write())
}

// TestTheTransitionLockIsTakenOnceAroundTheLogin asserts the login holds
// the change lock for its whole body and takes it exactly once. The lock
// is not reentrant: a second take inside the first waits on this very
// process until the deadline and then fails, so counting the acquisitions
// is what keeps a nested one from being added.
func TestTheTransitionLockIsTakenOnceAroundTheLogin(t *testing.T) {
	dir := repinFixture(t)
	// A change to replay, so the prelude reaches the work that needs the
	// lock: with nothing recorded it answers before taking anything and a
	// nested take would go unnoticed.
	seedActivePin(t, dir)
	takes := 0
	restore := SetTransitionLockHookForTest(func() { takes++ })
	defer restore()

	_, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	// The prelude and everything the login writes run under the one take
	// the body made.
	assert.Equal(t, 1, takes, "the change lock is taken once, around the whole login")
}

// TestALoginThatNamesAStoreChangesOnlyItsOwnCredential asserts the flag's
// whole effect on a change into the keychain. While another entry still
// holds a credential in the file store the change is refused - before the
// sign-in and again under the lock - naming it, and nothing is written.
// Once that entry is logged out the same login goes through: the
// credential commits to the store it names, the directory records it, and
// this entry's old copy is removed after the commit, never before.
func TestALoginThatNamesAStoreChangesOnlyItsOwnCredential(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]Server{
		"prod":    {URL: "https://r.ex"},
		"staging": {URL: "https://staging.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
	require.NoError(t, fs.SetToken(ServerRef{Name: "staging", URL: "https://staging.ex"}, "staging-tok"))

	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "keychain",
		ObservedDeclaration: "file",
		Declaration:         DeclarationFor("file", "keychain", "keychain"),
		OpenStore:           OpenResolvedStore,
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		ce, ok := errorsAsClierr(err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, "a login changes the store of its own credential only")
		require.ErrorContains(t, err, "the credential of staging is still in the plaintext file")
		assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
	}
	refused(t, loginRefusal(dir, cfg, "prod", in.Policy, in.Declaration))
	_, err := PersistLoginCredential(t.Context(), dir, in)
	refused(t, err)
	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "file", after.CredentialStore, "the directory still says where its credentials are")
	assert.NoFileExists(t, TransitionPath(dir))
	assert.Equal(t, "prod-old", storedFileToken(t, dir), "nothing was committed")
	assert.Empty(t, keychainTokenFor(t, dir, "staging"), "and nothing moved")

	require.NoError(t, fs.DeleteToken(ServerRef{Name: "staging", URL: "https://staging.ex"}))
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	after, lerr = Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "keychain", after.CredentialStore, "the directory records the store it was told to keep")
	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"))
	assert.Empty(t, storedFileToken(t, dir), "this entry's old copy is gone")
	assert.NotContains(t, joinWarnings(out.Warnings), "also moves", "no other entry is named, because none changed")
	assert.NoFileExists(t, TransitionPath(dir))
}

// TestAChangeIntoTheFileStoreMovesNoOtherEntry asserts the same rule in
// the direction where it matters most. A login names one entry; taking
// every other entry out of the keychain into a plaintext file on that
// answer would lower their protection without anyone asking for it. So the
// change is refused before the sign-in and again under the lock, naming
// the entries still in the keychain, and nothing is written. Once they are
// logged out, the same login goes through and only its own credential
// lands in the file store.
func TestAChangeIntoTheFileStoreMovesNoOtherEntry(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "keychain", CurrentServer: "prod", Servers: map[string]Server{
		"prod":    {URL: "https://r.ex"},
		"staging": {URL: "https://staging.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	seedKeychainCredential(t, dir, "prod", "prod-old")
	seedKeychainCredential(t, dir, "staging", "staging-tok")

	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "file",
		ObservedDeclaration: "keychain",
		Declaration:         DeclarationFor("keychain", "file", "file"),
		OpenStore:           OpenResolvedStore,
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		ce, ok := errorsAsClierr(err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, "a login changes the store of its own credential only")
		require.ErrorContains(t, err, "the credential of staging is still in the keychain")
		assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
	}

	refused(t, loginRefusal(dir, cfg, "prod", in.Policy, in.Declaration))
	_, err := PersistLoginCredential(t.Context(), dir, in)
	refused(t, err)

	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "keychain", after.CredentialStore, "the directory still says where its credentials are")
	assert.Empty(t, after.CredentialTransitionID)
	assert.NoFileExists(t, TransitionPath(dir))
	assert.Equal(t, "prod-old", keychainTokenFor(t, dir, "prod"), "nothing was committed")
	assert.Equal(t, "staging-tok", keychainTokenFor(t, dir, "staging"))
	_, serr := NewFileStore(dir).GetToken(ServerRef{Name: "staging", URL: "https://staging.ex"})
	require.ErrorIs(t, serr, ErrNoToken, "and nothing reached the plaintext file")

	_, _, derr := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "staging", URL: "https://staging.ex"}, "keychain")
	require.NoError(t, derr)
	cfg, lerr = Load(FilePath(dir))
	require.NoError(t, lerr)
	require.NoError(t, loginRefusal(dir, cfg, "prod", in.Policy, in.Declaration),
		"with the other entry logged out there is nothing it would take")
	_, err = PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	after, lerr = Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "file", after.CredentialStore)
	assert.Equal(t, "id:new", storedFileToken(t, dir), "the login's own credential is the one in plaintext")
	assert.Empty(t, keychainTokenFor(t, dir, "prod"), "and its keychain copy is gone")
}

// seedKeychainCredential stores a credential the way a login would: an
// identity record naming the account, and the slot itself.
func seedKeychainCredential(t *testing.T, dir, entry, token string) string {
	t.Helper()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: entry, State: identityActive,
		ID: id, Fingerprint: credentialFingerprint(id, token),
	}))
	require.NoError(t, keyringSet(keychainAccount(id, entry), token))
	return id
}

// TestAnEntryInBothStoresRefusesTheChange asserts another entry with a
// credential in each store refuses a change in either direction: the
// store being left holds one of its credentials whichever it is.
func TestAnEntryInBothStoresRefusesTheChange(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{
		"prod":  {URL: "https://r.ex"},
		"other": {URL: "https://other.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "other", URL: "https://other.ex"}, "other-file"))
	seedKeychainCredential(t, dir, "other", "other-keychain")

	for from, want := range map[string]string{
		backendFile:     "the credential of other is still in the plaintext file",
		backendKeychain: "the credential of other is still in the keychain",
	} {
		to := backendKeychain
		if from == backendKeychain {
			to = backendFile
		}
		err := CheckTransitionSources(dir, cfg, "prod", from, to)
		ce, ok := errorsAsClierr(err)
		require.True(t, ok, from)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, want)
		assert.Equal(t, "rancher auth logout "+clierr.ShellQuote("other"), ce.Hint, "one entry gets the one command that settles it")
	}
}

// TestTheFilePrePinStatesWhatIsAlreadyTrue asserts the one pin a login
// writes before its credential commits: an unpinned directory whose
// credentials are in the file store is written down as such before the
// commit, so the record stays true if the login fails and a change into
// `auto` is the same case as a change away from a `file` pin. A directory
// with nothing in the file store gets no such pin.
func TestTheFilePrePinStatesWhatIsAlreadyTrue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fileTokens bool
		wantPrePin string
	}{
		{name: "tokens in the file store", fileTokens: true, wantPrePin: backendFile},
		{name: "nothing in the file store", fileTokens: false, wantPrePin: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, "")
			keyring.MockInit()
			dir := t.TempDir()
			orig := keyringUsable
			keyringUsable = func() bool { return true }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			if tc.fileTokens {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
			}

			b, err := loginBackendsOf(dir, cfg, "prod", "auto", "")
			require.NoError(t, err)
			ch, perr := planStoreChange(dir, cfg, "prod", "https://r.ex", b)
			require.NoError(t, perr)
			plan := ch.plan
			require.NotNil(t, plan)

			assert.Equal(t, tc.wantPrePin, b.PrePin)
			assert.Equal(t, tc.wantPrePin, ReadPin(dir), "the pre-pin is written where there is one")
			assert.Equal(t, transitionPin, plan.manifest.Steps[len(plan.manifest.Steps)-1].Kind,
				"the pin the change ends with is its last step")
			require.NoError(t, removeTransition(dir))
		})
	}
}

// TestACreateOnlyDestinationStillDeclaresTheStore asserts the one path
// where a login that stored nothing still writes config.yaml: a helper
// that creates credentials on demand cannot be given one, and the login
// warns and succeeds. The directory still records the store it was told
// to keep, the change that removes the old copy is put into effect, the
// entry stops naming a token that is scheduled for deletion, and the
// cache records of the previous login stop being valid.
func TestACreateOnlyDestinationStillDeclaresTheStore(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "file", Servers: map[string]Server{"prod": {
		URL: "https://r.ex", CacheBindingID: "before",
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, TokenUserID: "user-a",
		CredentialExpiresAt: "2026-10-01T00:00:00Z",
	}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))

	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "exec:vault",
		ObservedDeclaration: "file",
		Declaration:         DeclarationFor("file", "exec:vault", "exec:vault"),
		OpenStore:           fileOpen(readOnlyStore{}),
	}
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorIs(t, err, ErrTokenStoreUnsupported)
	assert.Equal(t, clierr.ConfigWritten, out.ConfigState)

	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "exec:vault", after.CredentialStore, "the directory records the store it was told to keep")
	assert.NotEmpty(t, after.CredentialTransitionID, "the change that removes the old copy is in effect")
	entry := after.Servers["prod"]
	assert.Nil(t, entry.TokenRef, "the entry stops naming a token scheduled for deletion")
	assert.Empty(t, entry.TokenUserID)
	assert.Empty(t, entry.CredentialExpiresAt)
	assert.NotEqual(t, "before", entry.CacheBindingID, "and the previous login's cache records stop being valid")
	assert.False(t, entry.Paused(), "a change with delete steps only is resumed in the write that puts it into effect")
}

// readOnlyStore is a credential helper that creates credentials on demand
// and cannot be given one to keep.
type readOnlyStore struct{ failStore }

func (readOnlyStore) SetTokenWithOutcome(ServerRef, string, PriorCredential) (CommitOutcome, error) {
	return CommitOutcome{State: clierr.CredentialNotWritten}, ErrTokenStoreUnsupported
}

// joinWarnings is the warnings as one string, for an assertion about what
// a login said without depending on how many lines it took.
func joinWarnings(warnings []string) string {
	out := ""
	for _, w := range warnings {
		out += w + "\n"
	}
	return out
}

// TestTheLoginOpensThePhysicalBackendItResolved asserts what the store
// opener is handed: the backend the login resolved under the lock, which
// is already an answer. A policy would be resolved a second time, against
// the variable, and a directory whose destination the variable does not
// spell the same way would have its own login refused.
func TestTheLoginOpensThePhysicalBackendItResolved(t *testing.T) {
	t.Setenv(EnvCredentialStore, "auto")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, WritePin(dir, backendKeychain))

	var asked []string
	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "auto", ObservedDeclaration: "auto",
		OpenStore: func(backend, d string) (CredStore, string, error) {
			asked = append(asked, backend)
			return OpenResolvedStore(backend, d)
		},
	}
	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, []string{backendKeychain}, asked, "opened once, by name")
	assert.Equal(t, backendKeychain, ReadPin(dir), "and nothing re-pinned it")
}

// TestAStepThatCouldNotFinishIsAWarning asserts that work the login could
// not do comes back as a warning, from both places a step is run. The
// credential is committed and recorded by then, so the login succeeds -
// and a step left behind without a word would leave an entry unreadable
// with nothing on screen to say why.
func TestAStepThatCouldNotFinishIsAWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		// breakAt is the config write after which a path stops answering:
		// `transition` is before the old-copy step. breakAfter is a write after
		// which it does: the old copy's delete in the old-copy step, before the
		// replay.
		breakAt, breakAfter string
		// breakFile takes credentials.yaml, so the old-copy step cannot remove
		// the old copy; breakPin takes the pin's path. Both at `transition` is
		// the one state where the old-copy step can do neither.
		breakFile, breakPin bool
		// want is the whole sentence the step that could not finish
		// produces, which no other line of the login says; cause names
		// the path the backend's own error is about, since the OS words
		// for reading a directory differ between platforms.
		want, cause string
		// fails says the login ends in an error rather than a warning.
		fails bool
	}{
		{name: "the old copy, in the old-copy step", breakAt: "transition", breakFile: true,
			want: "the credential store change could not finish for prod", cause: "credentials.yaml"},
		{name: "the pin, in the replay", breakAfter: "credentials.yaml", breakPin: true,
			want: "a credential store change is unfinished for this directory (pin)", cause: pinFileName},
		{name: "the pin, in the old-copy step's fallback", breakAt: "transition", breakFile: true, breakPin: true, fails: true,
			want: "a credential store change is unfinished for this directory (pin)", cause: pinFileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := repinFixture(t)
			broken := false
			breakPaths := func() {
				if broken {
					return
				}
				broken = true
				if tc.breakFile {
					breakPath(t, CredentialsFilePath(dir))
				}
				if tc.breakPin {
					breakPath(t, PinPath(dir))
				}
			}
			restore := SetPersistWriteHookForTest(func(step string) error {
				if tc.breakAt != "" && step == tc.breakAt {
					breakPaths()
				}
				return nil
			})
			defer restore()
			defer SetAfterWriteHookForTest(func(what string) {
				if tc.breakAfter != "" && what == tc.breakAfter {
					breakPaths()
				}
			})()

			out, err := PersistLoginCredential(t.Context(), dir, repinInput())
			if tc.fails {
				// With neither the old copy removed nor the pin flipped
				// the login stops, which is its own named state - and the
				// step that could not run still has to say why.
				require.Error(t, err)
			} else {
				require.NoError(t, err, "the credential is committed; what is left over is a warning")
			}
			assert.Contains(t, joinWarnings(out.Warnings), tc.want,
				"the step that could not finish says so in its own words")
			assert.Contains(t, joinWarnings(out.Warnings), tc.cause,
				"with the backend's own cause behind it")
			line := ""
			for _, w := range out.Warnings {
				if strings.Contains(w, tc.want) {
					line = w
				}
			}
			require.NotEmpty(t, line)
			assert.Contains(t, line, "hint: rancher config status", "the report that shows the step")
			if tc.fails {
				return
			}

			m, rerr := readTransition(dir)
			require.NoError(t, rerr)
			require.NotNil(t, m, "the work that is left stays recorded for the next command")
		})
	}
}

// TestAnOldCopyAnotherDirectoryOwnsIsLeftAlone asserts the verdict that
// keeps a store change from deleting a credential that is not this
// directory's. The keychain slot holds what another directory stored
// there, so no delete step is recorded for it: a step is bound to the
// digest of the credential the planner read, and recording one over that
// slot's token would make the replay remove it.
func TestAnOldCopyAnotherDirectoryOwnsIsLeftAlone(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "keychain", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	// The record this directory holds, and a slot another directory
	// sharing the identity has since written.
	account := seedKeychainCredential(t, dir, "prod", "ours")
	require.NoError(t, keyringSet(keychainAccount(account, "prod"), "another-directorys"))

	old, notice, err := oldCopyOf(dir, "prod", "https://r.ex", backendKeychain)
	require.NoError(t, err)
	assert.Empty(t, notice)
	assert.Empty(t, old.token, "no step is recorded for a slot this directory no longer holds")
	assert.False(t, old.unread, "the slot answered; it just answered with someone else's credential")

	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "file",
		ObservedDeclaration: "keychain",
		Declaration:         DeclarationFor("keychain", "file", "file"),
		OpenStore:           OpenResolvedStore,
	}
	_, perr := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, perr)
	tok, gerr := keyringGet(keychainAccount(account, "prod"))
	require.NoError(t, gerr)
	assert.Equal(t, "another-directorys", tok, "the other directory's credential survives the change")
}

// TestAnOldCopyThatCannotBeReadFlipsThePinInstead asserts what the old-copy
// step does when this entry's old copy is in a backend that cannot answer. No
// delete step can be recorded - a delete is bound to the credential's digest
// and there is none to compute - so the pin flips in the old-copy step, which
// is the other way to stop a reader resolving to the copy that is still there.
// Nothing is left to hold a pause; a writer that read the copy is refused by
// the store check.
func TestAnOldCopyThatCannotBeReadFlipsThePinInstead(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	orig := keyringUsable
	keyringUsable = func() bool { return true }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

	cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, WritePin(dir, backendFile))
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
	// The one entry is the logging-in one, so nothing else reads the
	// source and no refusal fires: the old copy's own read is the only
	// one that meets the broken file store.
	breakPath(t, CredentialsFilePath(dir))

	pinAt := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		pinAt[step] = ReadPin(dir)
		return nil
	})
	defer restore()

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	assert.Contains(t, joinWarnings(out.Warnings), "could not be read and was left where it is")
	assert.Equal(t, backendKeychain, pinAt["resume"],
		"the pin flipped before the last write, because the copy could not be removed")
	assert.NotEmpty(t, out.CacheBindingID)
	assert.False(t, out.CacheWritesPaused)
}

// TestTheRemainingStepsRunBeforeTheLastWrite asserts the order around the
// remaining steps: the id a cache record binds to advanced in the first
// write, before anything, and the pin step runs before the last write,
// which resumes cache writes once nothing left can move readers.
func TestTheRemainingStepsRunBeforeTheLastWrite(t *testing.T) {
	dir := repinFixture(t)

	pinAt := map[string]string{}
	restore := SetPersistWriteHookForTest(func(step string) error {
		pinAt[step] = ReadPin(dir)
		return nil
	})
	defer restore()

	_, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)

	assert.Equal(t, backendFile, pinAt["entry"], "the pin has not moved yet when the id advances")
	assert.Equal(t, backendKeychain, pinAt["resume"], "and has before the write that resumes cache writes")
}

// TestACreateOnlyDestinationNamesItsWrite asserts the write on the
// warn-and-succeed path is named by what it carries. It is a rollback
// only when it carries nothing but the restored entry; when the login
// named a store it carries the declaration, the change's id and the
// binding id, and a test that fails the declaration write has to be able
// to fail this one too.
func TestACreateOnlyDestinationNamesItsWrite(t *testing.T) {
	seed := func(t *testing.T) (string, PersistLoginInput) {
		t.Helper()
		t.Setenv(EnvCredentialStore, "")
		keyring.MockInit()
		dir := t.TempDir()
		cfg := &Config{CredentialStore: "file", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-old"))
		return dir, PersistLoginInput{
			Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
			Provenance: clierr.ProvenanceDerived, Policy: "exec:vault",
			ObservedDeclaration: "file",
			Declaration:         DeclarationFor("file", "exec:vault", "exec:vault"),
			OpenStore:           fileOpen(readOnlyStore{}),
		}
	}

	t.Run("the write that declares is named for it", func(t *testing.T) {
		dir, in := seed(t)
		var steps []string
		restore := SetPersistWriteHookForTest(func(step string) error {
			steps = append(steps, step)
			return nil
		})
		defer restore()

		_, err := PersistLoginCredential(t.Context(), dir, in)
		require.ErrorIs(t, err, ErrTokenStoreUnsupported)
		assert.Equal(t, []string{"entry", "declaration"}, steps,
			"the entry write, then the write that carries the declaration")
	})

	t.Run("and failing it by that name is reported", func(t *testing.T) {
		dir, in := seed(t)
		restore := SetPersistWriteHookForTest(func(step string) error {
			if step == "declaration" {
				return errors.New("config directory is full")
			}
			return nil
		})
		defer restore()

		out, err := PersistLoginCredential(t.Context(), dir, in)
		require.Error(t, err)
		assert.Equal(t, clierr.ConfigRollbackFailed, out.ConfigState)
		require.NotNil(t, out.ConfigRecovery)
		assert.Equal(t, "prod", out.ConfigRecovery.Entry)
	})

	t.Run("a login that declares nothing still rolls back", func(t *testing.T) {
		dir, in := seed(t)
		in.Policy, in.Declaration = "file", ""
		var steps []string
		restore := SetPersistWriteHookForTest(func(step string) error {
			steps = append(steps, step)
			return nil
		})
		defer restore()

		_, err := PersistLoginCredential(t.Context(), dir, in)
		require.ErrorIs(t, err, ErrTokenStoreUnsupported)
		assert.Equal(t, []string{"entry", "rollback"}, steps,
			"nothing but the restored entry, so it is a rollback")
	})
}

// readerResolves is what a reader that takes no lock gets for an entry at
// this moment: config.yaml as it stands, the store opened the way the read
// path opens it, and a read.
func readerResolves(t *testing.T, dir, entry, url string) string {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	store, err := OpenStoreFor(cfg, dir)
	require.NoError(t, err)
	tok, gerr := store.GetToken(ServerRef{Name: entry, URL: url})
	if gerr != nil {
		return "error: " + gerr.Error()
	}
	return tok
}

// TestAPinNamingAThirdStoreIsWrittenBeforeTheCommit asserts the case an
// early pin write exists for. A directory with an explicit declaration can
// carry a pin that names neither the store it reads nor the one a login
// commits to - a stale `file` pin, or no pin at all - and a login that
// asks for `auto` leaves a declaration readers resolve through that pin.
// Were the pin written as the change's last step, every reader between the
// declaration write and that step would read the file store: through the
// stale pin, or through its own probe on a machine whose keychain it
// cannot use, which also writes a `file` pin of its own. Written first,
// while the declaration is explicit and no reader asks the pin, it is
// invisible, and a login that fails leaves readers where they were.
func TestAPinNamingAThirdStoreIsWrittenBeforeTheCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		// declared is the explicit declaration the directory starts
		// with; pin is credstore-pin, empty for none.
		declared, pin string
		// helper keeps prod's credential in a credential helper rather
		// than the keychain, which gives the change an old-copy delete
		// besides the early pin.
		helper bool
		// readerProbeFails makes every reader in between run its own
		// keychain probe and fail it, as a session over SSH would.
		readerProbeFails bool
		fail             bool
	}{
		{name: "a stale file pin, and the login goes through", declared: "keychain", pin: backendFile},
		{name: "a stale file pin, and the login fails", declared: "keychain", pin: backendFile, fail: true},
		{name: "no pin at all, and readers whose keychain cannot be used", declared: "keychain", readerProbeFails: true},
		{name: "a helper directory with a stale file pin", declared: "exec:fake", pin: backendFile, helper: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.helper {
				useFakeHelper(t, "ok")
			}
			t.Setenv(EnvCredentialStore, "")
			keyring.MockInit()
			dir := t.TempDir()
			orig := keyringUsable
			keyringUsable = func() bool { return true }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			cfg := &Config{CredentialStore: tc.declared, Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			if tc.pin != "" {
				require.NoError(t, WritePin(dir, tc.pin))
			}
			old := "prod-keychain-old"
			if tc.helper {
				old = "prod-helper-old"
				require.NoError(t, newExecStore("fake", dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, old))
			} else {
				seedKeychainCredential(t, dir, "prod", old)
			}
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-file-stale"))

			seen := map[string]string{}
			t.Cleanup(SetPersistWriteHookForTest(func(step string) error {
				if tc.readerProbeFails {
					keyringUsable = func() bool { return false }
					resetKeyringUsableCache()
					defer func() { keyringUsable = func() bool { return true }; resetKeyringUsableCache() }()
				}
				seen[step] = readerResolves(t, dir, "prod", "https://r.ex")
				return nil
			}))
			in := PersistLoginInput{
				Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
				Provenance: clierr.ProvenanceDerived, Policy: "auto",
				ObservedDeclaration: tc.declared,
				Declaration:         DeclarationFor(tc.declared, "auto", "auto"),
				OpenStore:           OpenResolvedStore,
			}
			if tc.fail {
				in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
			}

			out, err := PersistLoginCredential(t.Context(), dir, in)
			assert.Equal(t, backendKeychain, ReadPin(dir), "the pin names the keychain either way")
			assert.NoFileExists(t, TransitionPath(dir), "nothing is left to record")
			for step, got := range seen {
				assert.NotEqual(t, "prod-file-stale", got, "no reader is sent to the file store (at the %s write)", step)
			}
			after, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			if tc.fail {
				require.Error(t, err)
				assert.Equal(t, tc.declared, after.CredentialStore)
				assert.Equal(t, old, readerResolves(t, dir, "prod", "https://r.ex"),
					"readers still resolve through the explicit declaration")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "id:new", seen["resume"], "as cache writes resume, readers get the new credential")
			assert.Equal(t, "auto", after.CredentialStore)
			assert.Equal(t, "id:new", readerResolves(t, dir, "prod", "https://r.ex"))
			if tc.pin != "" {
				assert.Contains(t, joinWarnings(out.Warnings), "re-pinned", "the pin write says so")
			}
		})
	}
}

// TestAFailedLoginKeepsTheFilePrePin asserts the one pin a failed login
// leaves behind on purpose. An unpinned directory whose credentials are in
// the file store gets a `file` pin before the commit; it states what was
// already true, so a login that fails keeps it, and the next reader does
// not probe and pin the keychain over the plaintext tokens.
func TestAFailedLoginKeepsTheFilePrePin(t *testing.T) {
	dir := repinFixture(t)
	require.NoError(t, os.Remove(PinPath(dir)))
	in := repinInput()
	in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})

	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, backendFile, ReadPin(dir), "the pin a failed login wrote states where the tokens are")
	assert.Equal(t, "prod-old", storedFileToken(t, dir))
	assert.NoFileExists(t, TransitionPath(dir))
}

// TestAnAutoUpgradeStaysOnTheFileStoreWhileAnotherEntryIsThere asserts
// what an `auto` login does when it could use the keychain on a directory
// that reads the file store and another entry's credential is there. It
// is not refused - the kubectl plugin's re-login after a 401 has to go
// through - and it does not move: the credential stays where the directory
// already reads, nothing is recorded for a change, and the warning names
// the entries and the way out. A declared `file` directory asked for
// `auto` the same way ends declared `auto` and pinned `file`.
func TestAnAutoUpgradeStaysOnTheFileStoreWhileAnotherEntryIsThere(t *testing.T) {
	for _, tc := range []struct {
		name, declared, pin, flag string
		wantNotice                string
	}{
		{name: "a file-pinned auto directory", declared: "auto", pin: backendFile},
		{name: "a declared file directory asked for auto", declared: backendFile, flag: "auto",
			wantNotice: "pinned credential store: file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := repinFixture(t)
			cfg, err := Load(FilePath(dir))
			require.NoError(t, err)
			cfg.CredentialStore = tc.declared
			require.NoError(t, cfg.Write())
			require.NoError(t, os.Remove(PinPath(dir)))
			if tc.pin != "" {
				require.NoError(t, WritePin(dir, tc.pin))
			}
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://staging.ex"}, "staging-tok"))

			policy, _, err := LoginPolicy(tc.flag, tc.declared)
			require.NoError(t, err)
			in := repinInput()
			in.Policy, in.ObservedDeclaration = policy, tc.declared
			in.Declaration = DeclarationFor(tc.declared, tc.flag, policy)
			require.NoError(t, loginRefusal(dir, cfg, "prod", in.Policy, in.Declaration), "not a refusal")
			out, err := PersistLoginCredential(t.Context(), dir, in)
			require.NoError(t, err)

			assert.Equal(t, "id:new", storedFileToken(t, dir), "the credential stays where the directory reads")
			assert.Empty(t, keychainTokenFor(t, dir, "prod"))
			assert.Equal(t, "staging-tok", mustFileToken(t, dir, "staging", "https://staging.ex"))
			assert.Equal(t, backendFile, ReadPin(dir))
			assert.NoFileExists(t, TransitionPath(dir), "no change was recorded")
			after, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			assert.Equal(t, "auto", after.CredentialStore)
			warnings := joinWarnings(out.Warnings)
			assert.Contains(t, warnings, "this directory keeps 1 other credential in the plaintext file "+CredentialsFilePath(dir)+" (staging)")
			assert.Contains(t, warnings, "log that entry out, then log in again")
			if tc.wantNotice != "" {
				assert.Contains(t, warnings, tc.wantNotice)
			}
		})
	}
}

// TestAFailedLoginKeepsTheKeychainPrePin asserts the mirror of the file
// pre-pin. An unpinned `auto` directory whose readers find the keychain -
// identity records, no file token - gets a `keychain` pin before a login
// that leaves it for the file store commits: without it, the login's new
// file token beside the records would read as both stores for every
// entry. It states what readers already use, so a login that fails keeps
// it and readers still get the keychain credential.
func TestAFailedLoginKeepsTheKeychainPrePin(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	seedKeychainCredential(t, dir, "prod", "prod-keychain")

	in := PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceDerived, Policy: "file",
		ObservedDeclaration: "auto",
		Declaration:         DeclarationFor("auto", "file", "file"),
		OpenStore:           fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")}),
	}
	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	assert.Equal(t, backendKeychain, ReadPin(dir), "the pin a failed login wrote states where the credentials are")
	assert.Equal(t, "prod-keychain", readerResolves(t, dir, "prod", "https://r.ex"))
	assert.NoFileExists(t, TransitionPath(dir))
}

// TestACredentialLeftInTheKeychainIsNeverServedAgainByAChange asserts the
// case a change's destination is read for. On a file-pinned `auto`
// directory, staging was logged out while the keychain could not be
// reached: its identity record and keychain copy stayed behind. prod's
// `auto` login could now use the keychain, and moving the directory there
// would serve staging that copy. So the login stays on the file store and
// says why, and a login naming the keychain is refused before anything is
// written. Once staging is logged out with the keychain reachable, the
// same upgrade goes through and staging reads as not logged in.
func TestACredentialLeftInTheKeychainIsNeverServedAgainByAChange(t *testing.T) {
	dir := repinFixture(t)
	seedKeychainCredential(t, dir, "staging", "staging-logged-out")

	out, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	assert.Equal(t, "id:new", storedFileToken(t, dir), "the login stays on the file store")
	assert.Equal(t, backendFile, ReadPin(dir))
	assert.Contains(t, joinWarnings(out.Warnings), "has 1 other credential left in the keychain from an earlier login (staging)")
	assert.Contains(t, joinWarnings(out.Warnings), "log that entry out with the keychain reachable, then log in again")
	assert.Contains(t, readerResolves(t, dir, "staging", "https://staging.ex"), ErrNoToken.Error())

	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	byName := func() error {
		_, err := LoginStoreChange(dir, cfg, "prod", "keychain", "keychain")
		return err
	}
	require.ErrorContains(t, byName(), "the keychain still holds a credential of staging from an earlier login")
	assert.Equal(t, backendFile, ReadPin(dir), "the refusal wrote nothing")

	_, _, derr := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "staging", URL: "https://staging.ex"}, "auto")
	require.NoError(t, derr)
	_, err = PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	assert.Equal(t, backendKeychain, ReadPin(dir), "with staging logged out the upgrade goes through")
	assert.Contains(t, readerResolves(t, dir, "staging", "https://staging.ex"), ErrNoToken.Error(),
		"and staging reads as not logged in, never the credential it was logged out of")
}

// TestTheLockRefusesAChangeIntoAStoreWithACredentialLeftThere asserts the
// authority of the destination rule is the check under the lock, not the
// one before the sign-in: a leftover that appears in between - another
// entry's logout on an unreachable keychain, or a check run against an
// older config - is caught there. PersistLoginCredential runs with no
// check before it, the refusal is the typed one, and nothing the refused
// login would have written is on disk: prod's old token, the declaration,
// the pin, no change record, no keychain copy of the new credential, and
// the config file saw no write at all.
func TestTheLockRefusesAChangeIntoAStoreWithACredentialLeftThere(t *testing.T) {
	for _, tc := range []struct {
		name, to string
		// keychainLeft leaves staging an identity record and slot it was
		// logged out of; otherwise staging has nothing stored at all.
		keychainLeft bool
		// bare is an undeclared directory with no pin where nothing is
		// stored at all, prod included: readers find nothing, and after
		// the change they would ask the helper.
		bare bool
		want string
	}{
		{name: "a keychain copy another entry was logged out of", to: backendKeychain, keychainLeft: true,
			want: "the keychain still holds a credential of staging from an earlier login"},
		{name: "a helper that would answer for another entry", to: "exec:fake",
			want: "the credential helper fake may answer for staging: after the change staging would be served a credential it never logged in with"},
		{name: "a helper, from a directory that has stored nothing", to: "exec:fake", bare: true,
			want: "the credential helper fake may answer for staging: after the change staging would be served a credential it never logged in with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A helper that creates credentials on demand: staging, never
			// logged in, would read "short-lived" after the change.
			useFakeHelper(t, "expiresat")
			t.Setenv("FAKEHELPER_EXPIRESAT", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			t.Setenv(EnvCredentialStore, "")
			keyring.MockInit()
			dir := t.TempDir()
			declared, prodOld := "file", "prod-old"
			if tc.bare {
				declared, prodOld = "", ""
			}
			cfg := &Config{CredentialStore: declared, Servers: map[string]Server{
				"prod": {URL: "https://r.ex"}, "staging": {URL: "https://staging.ex"},
			}}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			if prodOld != "" {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, prodOld))
			}
			if tc.keychainLeft {
				seedKeychainCredential(t, dir, "staging", "staging-logged-out")
			}
			configWrites := 0
			t.Cleanup(SetAfterWriteHookForTest(func(what string) {
				if what == filepath.Base(FilePath(dir)) {
					configWrites++
				}
			}))

			_, err := PersistLoginCredential(t.Context(), dir, PersistLoginInput{
				Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
				Provenance: clierr.ProvenanceDerived, Policy: tc.to,
				ObservedDeclaration: declared,
				Declaration:         DeclarationFor(declared, tc.to, tc.to),
				OpenStore:           OpenResolvedStore,
			})
			ce, ok := errorsAsClierr(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, tc.want)

			assert.Equal(t, prodOld, storedFileToken(t, dir), "nothing was committed")
			assert.Empty(t, keychainTokenFor(t, dir, "prod"))
			after, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			assert.Equal(t, declared, after.CredentialStore, "the declaration is unchanged")
			assert.Empty(t, after.CredentialTransitionID)
			assert.Equal(t, cfg.Servers["prod"].URL, after.Servers["prod"].URL)
			assert.NoFileExists(t, TransitionPath(dir), "no change was recorded")
			assert.NoFileExists(t, PinPath(dir), "no pin was written")
			assert.Zero(t, configWrites, "the refusal comes before any write of the config file")
		})
	}
}
