package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
)

const orphanHelperURL = "https://r.ex"

// undeclaredHelperOrphanFixture is a directory that declares no credential
// store, whose one entry is staging, where the fake helper holds a
// credential under prod that this directory recorded when it stored it and
// config.yaml has no prod. The creating process uses the keychain and has
// no credential store variable; a reader whose variable names the fake
// helper is the one the orphan would reach.
func undeclaredHelperOrphanFixture(t *testing.T) (dir, statePath string) {
	t.Helper()
	statePath = useFakeHelper(t, "ok")
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	orig := keyringUsable
	keyringUsable = func() error { return nil }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })
	dir = t.TempDir()
	cfg := &Config{Servers: map[string]Server{"staging": {URL: "https://staging.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, newExecStore("fake", dir).SetToken(ServerRef{Name: "prod", URL: orphanHelperURL}, "prod-orphan-helper"))
	return dir, statePath
}

// creatingLoginInput is a login that creates prod on a directory that
// declares nothing, committing to the keychain.
func creatingLoginInput(provenance clierr.Provenance) PersistLoginInput {
	return PersistLoginInput{
		Name: "prod", CanonicalURL: orphanHelperURL, Bearer: "id:new",
		Provenance: provenance, OwnsEntry: true,
		Policy: "auto", ObservedDeclaration: "",
		Declaration: DeclarationFor("", "", "auto"),
		OpenStore:   OpenResolvedStore,
	}
}

// readAsHelperReader is what a process reads for prod when
// RANCHER_CLI_CREDENTIAL_STORE names the fake helper, as a kubectl run from
// a kubeconfig that recorded the variable would.
func readAsHelperReader(t *testing.T, dir string) (string, error) {
	t.Helper()
	t.Setenv(EnvCredentialStore, "exec:fake")
	defer t.Setenv(EnvCredentialStore, "")
	return readMatrixEntry(dir, "prod")
}

// helperRecordedNames is the entry names the directory's helper records
// hold for helper.
func helperRecordedNames(t *testing.T, dir, helper string) []string {
	t.Helper()
	st, err := readHelperRecords(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range st.Entries {
		if e.Helper == helper {
			names = append(names, e.Name)
		}
	}
	return names
}

// recordOrphan adds a helper record for prod without a credential behind
// it, as an earlier store through that helper would have left.
func recordOrphan(t *testing.T, dir, helper string) {
	t.Helper()
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(st *helperRecords) error {
		st.add(HelperRecord{Helper: helper, Name: "prod", CanonicalURL: orphanHelperURL})
		return nil
	}))
}

// orphanRemedy is the refusal text for a recorded helper that cannot erase
// prod's leftover credential, with cause in the parentheses.
func orphanRemedy(dir, helper, cause string) string {
	records := filepath.Join(CanonicalDir(dir), helperRecordsFileName)
	return "the credential helper " + helper + " holds a credential stored under 'prod' that no entry names and could not erase it (" + cause +
		"); make " + helper + " available on PATH (or working) and run this command again, or use a different --config; if " +
		helper + " is gone for good, remove the 'prod' record for " + helper + " from " + records
}

// TestALoginCreatingAnEntryErasesAnOrphanFromAHelperItDoesNotUse asserts
// the erase covers every helper this directory's records name for the new
// entry, not only the one the creating process uses. On a directory that
// declares no store, a reader whose credential store variable names that
// other helper is served whatever the helper holds under the new entry's
// name and URL, from the entry write on. The login erases it first, with a
// notice, so that reader finds no credential at any write the login makes
// and the helper's own state no longer holds it. The plugin's login and
// `auth login` share the primitive.
func TestALoginCreatingAnEntryErasesAnOrphanFromAHelperItDoesNotUse(t *testing.T) {
	for name, provenance := range map[string]clierr.Provenance{
		"the plugin's login": clierr.ProvenanceExecLogin,
		"auth login":         clierr.ProvenanceDerived,
	} {
		t.Run(name, func(t *testing.T) {
			dir, statePath := undeclaredHelperOrphanFixture(t)
			require.Contains(t, fakeHelperCreds(t, statePath), orphanHelperURL+"\nprod", "the helper holds the orphan before")
			type observation struct {
				what string
				tok  string
				err  error
			}
			var seen []observation
			t.Cleanup(SetAfterWriteHookForTest(func(what string) {
				tok, err := readAsHelperReader(t, dir)
				seen = append(seen, observation{what, tok, err})
			}))

			out, err := PersistLoginCredential(t.Context(), dir, creatingLoginInput(provenance))
			require.NoError(t, err)
			assert.Contains(t, joinWarnings(out.Warnings), "removed a credential stored under prod that no entry named, from the credential helper fake")
			assert.Empty(t, fakeHelperCreds(t, statePath), "the helper no longer holds the orphan")
			assert.Empty(t, helperRecordedNames(t, dir, "fake"), "and the directory no longer records it")
			var noToken bool
			for _, o := range seen {
				assert.NotEqual(t, "prod-orphan-helper", o.tok, "no reader is served the orphan after %s", o.what)
				noToken = noToken || errors.Is(o.err, ErrNoToken)
			}
			assert.True(t, noToken, "a reader finds no stored token once the entry exists: %+v", seen)
		})
	}
}

// TestAFailedLoginCommitLeavesNoHelperOrphanToServe asserts the erase comes
// before the entry exists: a login whose credential commit fails and whose
// entry stays on a directory that declares no store leaves the helper
// holding nothing under the entry's name, so a reader whose credential
// store variable names that helper is not served the leftover for as long
// as the entry stays.
func TestAFailedLoginCommitLeavesNoHelperOrphanToServe(t *testing.T) {
	dir, statePath := undeclaredHelperOrphanFixture(t)
	t.Cleanup(SetKeyringBackendForTest(unreachableKeyring{}))

	_, err := PersistLoginCredential(t.Context(), dir, creatingLoginInput(clierr.ProvenanceExecLogin))
	require.Error(t, err, "the keychain cannot take the credential")
	assert.Empty(t, fakeHelperCreds(t, statePath), "the helper no longer holds the orphan")
	tok, rerr := readAsHelperReader(t, dir)
	assert.NotEqual(t, "prod-orphan-helper", tok)
	require.Error(t, rerr, "a reader is served no credential, whether or not the entry stayed")
}

// TestACreatingCommandRefusesARecordedHelperThatCannotErase asserts the
// refusal for each way a recorded helper can fail to erase the leftover
// credential: it fails, it answers with something unreadable, it is not on
// PATH. The code is the helper's own (CREDENTIAL_STORE_UNAVAILABLE, or
// VALIDATION for an unreadable answer); the message names the helper, the
// entry, the cause and the way out, which is the same for every recorded
// helper and never says to change credentialStore. Nothing is written: no
// entry, and the helper's record stays for a later attempt.
func TestACreatingCommandRefusesARecordedHelperThatCannotErase(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		helper string
		code   string
		cause  string
	}{
		{
			name: "a helper that fails",
			setup: func(t *testing.T, _ string) {
				t.Helper()
				t.Setenv("FAKEHELPER_MODE", "fail")
			},
			helper: "fake",
			code:   clierr.CodeCredentialStoreUnavailable,
			cause:  "credential helper rancher-credential-fake could not erase the credential for 'prod': fakehelper: deliberate failure",
		},
		{
			name: "a helper that answers with something unreadable",
			setup: func(t *testing.T, _ string) {
				t.Helper()
				t.Setenv("FAKEHELPER_MODE", "garbage")
			},
			helper: "fake",
			code:   clierr.CodeValidation,
			cause:  "",
		},
		{
			name: "a helper not on PATH",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				recordOrphan(t, dir, "gone")
			},
			helper: "gone",
			code:   clierr.CodeCredentialStoreUnavailable,
			cause:  "credential helper rancher-credential-gone could not erase the credential for 'prod': it is not installed in PATH",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, statePath := undeclaredHelperOrphanFixture(t)
			tc.setup(t, dir)
			before := helperRecordedNames(t, dir, tc.helper)
			require.Equal(t, []string{"prod"}, before)

			_, err := PersistLoginCredential(t.Context(), dir, creatingLoginInput(clierr.ProvenanceExecLogin))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, tc.code, ce.Code)
			cause := tc.cause
			if cause == "" {
				// The message carries the helper error's own text, which
				// is the JSON decoder's for an unreadable answer.
				require.Error(t, errors.Unwrap(ce))
				cause = errors.Unwrap(ce).Error()
				assert.Contains(t, cause, "credential helper rancher-credential-fake erase: malformed response")
			}
			assert.Equal(t, orphanRemedy(dir, tc.helper, cause), ce.Message)
			assert.NotContains(t, ce.Message, "credentialStore")

			after, lerr := Load(FilePath(dir))
			require.NoError(t, lerr)
			assert.NotContains(t, after.Servers, "prod", "no entry was written")
			assert.Equal(t, before, helperRecordedNames(t, dir, tc.helper), "the record stays for a later attempt")
			assert.NoFileExists(t, PinPath(dir))
			assert.NoFileExists(t, TransitionPath(dir))
			if tc.helper == "fake" {
				t.Setenv("FAKEHELPER_MODE", "ok")
				assert.Contains(t, fakeHelperCreds(t, statePath), orphanHelperURL+"\nprod", "the helper still holds what it could not erase")
			}
		})
	}
}

// TestARecordedHelperThatFailsLeavesTheEraseBeforeItDone asserts the order
// of the erase when the directory records two helpers for the entry name:
// the first helper's credential is erased and its record removed with its
// notice, the second helper fails, and nothing else is written - no entry,
// and the second helper's record stays.
func TestARecordedHelperThatFailsLeavesTheEraseBeforeItDone(t *testing.T) {
	dir, statePath := undeclaredHelperOrphanFixture(t)
	state2 := filepath.Join(t.TempDir(), "state2.json")
	t.Setenv("FAKEHELPER2_STATE", state2)
	t.Setenv("FAKEHELPER2_MODE", "ok")
	require.NoError(t, newExecStore("fake2", dir).SetToken(ServerRef{Name: "prod", URL: orphanHelperURL}, "prod-orphan-helper-2"))
	require.Equal(t, []string{"prod"}, helperRecordedNames(t, dir, "fake2"))
	t.Setenv("FAKEHELPER2_MODE", "fail")

	out, err := PersistLoginCredential(t.Context(), dir, creatingLoginInput(clierr.ProvenanceExecLogin))
	require.ErrorContains(t, err, "the credential helper fake2 holds a credential stored under 'prod' that no entry names and could not erase it")
	assert.Contains(t, joinWarnings(out.Warnings), "removed a credential stored under prod that no entry named, from the credential helper fake\n",
		"the first erase is done and reported")
	assert.Empty(t, fakeHelperCreds(t, statePath), "the first helper holds nothing")
	assert.Empty(t, helperRecordedNames(t, dir, "fake"))
	assert.Equal(t, []string{"prod"}, helperRecordedNames(t, dir, "fake2"), "the failing helper's record stays")
	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.NotContains(t, after.Servers, "prod", "no entry was written")
	assert.Empty(t, keychainTokenFor(t, dir, "prod"), "and no credential")
}

// TestCheckOrphanCredentialsRefusesAMissingRecordedHelperAndWritesNothing
// asserts the check before the sign-in: a helper the directory's records
// name for the entry that is not on PATH refuses with the error the removal
// under the lock gives, and the check creates no directory and no lock file.
// A helper that is on PATH, or an entry that exists, passes; a corrupt
// records file, or one naming an invalid helper, refuses as the locked
// loader does.
func TestCheckOrphanCredentialsRefusesAMissingRecordedHelperAndWritesNothing(t *testing.T) {
	dir, _ := undeclaredHelperOrphanFixture(t)
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	require.NoError(t, CheckOrphanCredentials(dir, cfg, "prod"), "a recorded helper on PATH passes")

	recordOrphan(t, dir, "gone")
	entries := func() []string {
		des, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		var names []string
		for _, de := range des {
			names = append(names, de.Name())
		}
		return names
	}
	before := entries()

	cerr := CheckOrphanCredentials(dir, cfg, "prod")
	ce := assertStoreUnavailable(t, cerr)
	assert.Contains(t, ce, "the credential helper gone holds a credential stored under 'prod'")
	assert.Equal(t, before, entries(), "the check wrote nothing")

	_, rerr := RemoveOrphanCredentials(t.Context(), dir, cfg, "prod")
	require.Error(t, rerr)
	assert.Equal(t, cerr.Error(), rerr.Error(), "the removal under the lock gives the same error")

	require.NoError(t, CheckOrphanCredentials(dir, &Config{Servers: map[string]Server{"prod": {}}}, "prod"), "an entry that exists is not an orphan")

	bare := filepath.Join(t.TempDir(), "never-created")
	require.NoError(t, CheckOrphanCredentials(bare, cfg, "prod"))
	assert.NoDirExists(t, bare, "a directory that does not exist is not created")

	corrupt := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(corrupt, helperRecordsFileName), []byte("entries: [unterminated"), 0o600))
	require.ErrorContains(t, CheckOrphanCredentials(corrupt, cfg, "prod"), "credential-helper records file "+helperRecordsFileName+" is corrupt")

	invalid := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(invalid, helperRecordsFileName),
		[]byte("entries:\n- helper: ../evil\n  name: prod\n  canonicalURL: "+orphanHelperURL+"\n"), 0o600))
	require.ErrorContains(t, CheckOrphanCredentials(invalid, cfg, "prod"), `names invalid helper "../evil"`)
}

// TestCheckOrphanCredentialsTakesNoRecordsLockWhenTheRecordsFileExists
// asserts the check reads an existing records file without the records
// lock: with the file present and no lock file beside it, the check
// returns its refusal and no lock file exists afterwards.
func TestCheckOrphanCredentialsTakesNoRecordsLockWhenTheRecordsFileExists(t *testing.T) {
	dir, _ := undeclaredHelperOrphanFixture(t)
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	recordOrphan(t, dir, "gone")
	lock := filepath.Join(dir, helperRecordsLockName)
	require.NoError(t, os.Remove(lock))
	require.FileExists(t, filepath.Join(dir, helperRecordsFileName))

	require.ErrorContains(t, CheckOrphanCredentials(dir, cfg, "prod"), "the credential helper gone holds a credential stored under 'prod'")
	assert.NoFileExists(t, lock, "the check took no records lock")
}

// TestOrphanRefusalNamesTheCanonicalConfigDirectory asserts the remedy
// names the records file under the canonical (absolute) config directory,
// not the spelling the command was given: a relative directory reaches the
// remedy as an absolute path.
func TestOrphanRefusalNamesTheCanonicalConfigDirectory(t *testing.T) {
	abs, _ := undeclaredHelperOrphanFixture(t)
	cfg, err := Load(FilePath(abs))
	require.NoError(t, err)
	recordOrphan(t, abs, "gone")
	t.Chdir(filepath.Dir(abs))
	dir := filepath.Base(abs)
	require.False(t, filepath.IsAbs(dir))

	canonical := CanonicalDir(dir)
	require.True(t, filepath.IsAbs(canonical))
	want := filepath.Join(canonical, helperRecordsFileName)
	for name, run := range map[string]func() error{
		"the removal": func() error {
			_, rerr := RemoveOrphanCredentials(t.Context(), dir, cfg, "prod")
			return rerr
		},
		"the check": func() error { return CheckOrphanCredentials(dir, cfg, "prod") },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Contains(t, ce.Message, " from "+want)
		})
	}
}

// TestRemovingAnOrphanOnADirectoryWithoutHelperRecordsTakesNoLock asserts a
// directory that never stored through a helper has nothing to erase, and
// the removal neither takes the records lock nor leaves a lock file.
func TestRemovingAnOrphanOnADirectoryWithoutHelperRecordsTakesNoLock(t *testing.T) {
	dir := orphanFixture(t)
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)

	notices, rerr := RemoveOrphanCredentials(t.Context(), dir, cfg, "prod")
	require.NoError(t, rerr)
	assert.Empty(t, notices)
	assert.NoFileExists(t, filepath.Join(dir, helperRecordsLockName))
	assert.NoFileExists(t, filepath.Join(dir, helperRecordsFileName))
}

// TestRemovingAnOrphanRefusesACorruptHelperRecordsFile asserts a records
// file that cannot be read refuses the removal, so no command creating an
// entry goes ahead while it cannot tell which helpers hold a leftover.
func TestRemovingAnOrphanRefusesACorruptHelperRecordsFile(t *testing.T) {
	dir := orphanFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, helperRecordsFileName), []byte("entries: [unterminated"), 0o600))
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)

	_, rerr := RemoveOrphanCredentials(t.Context(), dir, cfg, "prod")
	require.ErrorContains(t, rerr, "credential-helper records file "+helperRecordsFileName+" is corrupt")
}
