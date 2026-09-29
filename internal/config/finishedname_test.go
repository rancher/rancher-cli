package config

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// finishedName is an id config.yaml names with no record under it: what a
// change whose last step finished leaves when the process ends before a
// config write forgets the name.
var finishedName = strings.Repeat("f", transitionIDLen)

// nameFinished writes finishedName into dir's config.yaml.
func nameFinished(t *testing.T, dir string) {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialTransitionID = finishedName
	require.NoError(t, cfg.Write())
}

// namedID is the change id dir's config.yaml names now.
func namedID(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	return cfg.CredentialTransitionID
}

// TestThePreludeClearsAFinishedName asserts the prelude forgets a change
// whose last step finished, in the config the caller writes: the one it
// found finished, the one another command finished before the lock was
// taken, and the one its own replay just finished. A name left
// behind would make the next change record, written by a login that dies
// before naming it, read as a change the directory does not name - which
// refuses every credential command - instead of as work that never
// started.
func TestThePreludeClearsAFinishedName(t *testing.T) {
	t.Run("found finished", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &Config{CredentialTransitionID: finishedName}
		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
		require.NoError(t, err)
		assert.Empty(t, cfg.CredentialTransitionID)
	})

	t.Run("finished by its replay", func(t *testing.T) {
		dir := t.TempDir()
		cfg := activeTransition(t, dir, fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-old"))
		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
		require.NoError(t, err)
		assert.NoFileExists(t, TransitionPath(dir))
		assert.Empty(t, cfg.CredentialTransitionID)
	})

	t.Run("finished by another command before the lock is taken", func(t *testing.T) {
		dir := t.TempDir()
		cfg := activeTransition(t, dir, fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-old"))
		// The other command's last step removes the record in the moment
		// between this prelude's first read and its read under the lock.
		defer SetTransitionLockHookForTest(func() {
			require.NoError(t, removeTransition(dir))
		})()
		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
		require.NoError(t, err)
		assert.Empty(t, cfg.CredentialTransitionID)
	})

	t.Run("a change left in flight keeps its name", func(t *testing.T) {
		dir := t.TempDir()
		cfg := activeTransition(t, dir, unreachableDeleteStep(t, "staging"))
		restore := SetKeyringBackendForTest(unreachableKeyring{})
		defer restore()
		id := cfg.CredentialTransitionID
		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
		require.NoError(t, err)
		assert.Equal(t, id, cfg.CredentialTransitionID)
	})
}

// TestALoginClearsAFinishedNameBeforeItsChangeRecord asserts the login
// forgets a finished name in its first write, before it writes the record
// of its own change.
func TestALoginClearsAFinishedNameBeforeItsChangeRecord(t *testing.T) {
	dir := repinFixture(t)
	nameFinished(t, dir)

	var atRecord string
	defer SetAfterWriteHookForTest(func(what string) {
		if what == transitionFileName && atRecord == "" {
			atRecord = namedID(t, dir) + "|"
		}
	})()
	_, err := PersistLoginCredential(t.Context(), dir, repinInput())
	require.NoError(t, err)
	assert.Equal(t, "|", atRecord, "config.yaml named nothing when the record was written")
	assert.Empty(t, namedID(t, dir))
}

// errCrash stands for the process ending: a test panics with it from a
// write hook and recovers it, so nothing after that write runs.
var errCrash = errors.New("the process ended here")

// TestALoginThatEndsAfterItsChangeRecordLeavesWorkThatNeverStarted asserts
// the two ways a login can leave its record unnamed - it ends right after
// writing it, or its rollback cannot remove it - on a directory whose
// config.yaml named a finished change. Either way the record reads as a
// change that never started, which the next credential command removes,
// and never as a change the directory does not name, which would refuse
// every credential command until the file was moved aside by hand.
func TestALoginThatEndsAfterItsChangeRecordLeavesWorkThatNeverStarted(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{name: "the process ends after the record is written", run: func(t *testing.T, dir string) {
			t.Helper()
			defer SetAfterWriteHookForTest(func(what string) {
				if what == transitionFileName {
					panic(errCrash)
				}
			})()
			defer func() { assert.Equal(t, errCrash, recover()) }()
			_, _ = PersistLoginCredential(t.Context(), dir, repinInput())
		}},
		{name: "the rollback cannot remove the record", run: func(t *testing.T, dir string) {
			t.Helper()
			var record []byte
			// The removal succeeds and the file is put straight back,
			// which is what a removal that failed leaves on disk.
			defer SetAfterWriteHookForTest(func(what string) {
				switch what {
				case transitionFileName:
					b, err := os.ReadFile(TransitionPath(dir))
					require.NoError(t, err)
					record = b
				case transitionFileName + " removed":
					require.NoError(t, os.WriteFile(TransitionPath(dir), record, transitionFilePerm))
				}
			})()
			in := repinInput()
			in.OpenStore = func(string, string) (CredStore, string, error) {
				return nil, "", errors.New("the destination store cannot be opened")
			}
			_, err := PersistLoginCredential(t.Context(), dir, in)
			require.ErrorContains(t, err, "cannot be opened")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := repinFixture(t)
			nameFinished(t, dir)
			tc.run(t, dir)

			require.FileExists(t, TransitionPath(dir), "the record is left behind")
			assert.Empty(t, namedID(t, dir), "and nothing names it, not the finished change either")
			cfg, err := Load(FilePath(dir))
			require.NoError(t, err)
			state, _, lerr := LoadTransition(dir, cfg)
			require.NoError(t, lerr)
			assert.Equal(t, TransitionAbandoned, state)

			_, perr := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
			require.NoError(t, perr, "the next credential command goes on")
			assert.NoFileExists(t, TransitionPath(dir), "and removes the record")
		})
	}
}

// TestACommandsOwnDeleteNeverDeletesByAStepsAccount asserts the order every
// credential command runs in - the prelude, then its own delete - never
// removes a credential by the account a step names. The step's slot now
// holds another directory's credential: the step is dropped as superseded,
// and the command's own delete, which reads the entry's identity record
// and compares before it deletes, leaves the slot alone too.
func TestACommandsOwnDeleteNeverDeletesByAStepsAccount(t *testing.T) {
	for _, del := range []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{name: "a logout's or a server change's delete", run: func(t *testing.T, dir string) {
			t.Helper()
			_, _, _, err := DeleteTokenEverywhere(t.Context(), dir, ServerRef{Name: "prod", URL: "https://r.ex"}, "file")
			require.NoError(t, err)
		}},
		{name: "logout --all's delete", run: func(t *testing.T, dir string) {
			t.Helper()
			_, _, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://r.ex"}, "file")
			require.NoError(t, err)
		}},
	} {
		t.Run(del.name, func(t *testing.T) {
			keyring.MockInit()
			dir := t.TempDir()
			// prod's old copy was in the keychain, and its identity record
			// still names that slot with the old copy's fingerprint.
			seedKeychainCredential(t, dir, "prod", "prod-old")
			rec, err := readIdentityRecord(dir, "prod")
			require.NoError(t, err)
			require.NotNil(t, rec)
			vid := mustAccount(t)
			id, err := newTransitionID()
			require.NoError(t, err)
			require.NoError(t, writeTransition(dir, transitionManifest{Version: transitionVersion, ID: id, Steps: []transitionStep{{
				Kind: transitionDelete, Entry: "prod", Source: backendKeychain,
				VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "prod-old"),
				SourceAccountID: rec.ID,
			}}}))
			cfg := &Config{CredentialStore: "file", CredentialTransitionID: id, Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			// Another directory sharing this keychain has since put its
			// own credential in that slot.
			require.NoError(t, keyringSet(rec.Account(), "theirs"))

			notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
			require.NoError(t, err)
			assert.Contains(t, strings.Join(notices, "\n"), "was replaced since the change started; it was left alone")
			del.run(t, dir)

			got, gerr := keyringGet(rec.Account())
			require.NoError(t, gerr)
			assert.Equal(t, "theirs", got, "the other directory's credential is still there")
		})
	}
}
