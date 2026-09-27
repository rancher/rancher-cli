package config

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// orphanFixture is an `auto` directory with no pin whose only entry,
// staging, keeps its credential in the keychain; prod is not in
// config.yaml.
func orphanFixture(t *testing.T) string {
	t.Helper()
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	orig := keyringUsable
	keyringUsable = func() bool { return true }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })
	cfg := &Config{CredentialStore: "auto", Servers: map[string]Server{"staging": {URL: "https://staging.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	seedKeychainCredential(t, dir, "staging", "staging-tok")
	return dir
}

func pluginCreateInput() PersistLoginInput {
	return PersistLoginInput{
		Name: "prod", CanonicalURL: "https://r.ex", Bearer: "id:new",
		Provenance: clierr.ProvenanceExecLogin, OwnsEntry: true,
		Policy: "auto", ObservedDeclaration: "auto",
		Declaration: DeclarationFor("auto", "", "auto"),
		OpenStore:   OpenResolvedStore,
	}
}

// TestALoginCreatingAnEntryRemovesACredentialLeftUnderItsName asserts
// what a login that creates its entry does with a credential stored under
// that name when config.yaml has none: it removes it from the keychain and
// the file store under the locks, before the entry write, and says so for
// each. A login that updates an entry it already has removes nothing.
func TestALoginCreatingAnEntryRemovesACredentialLeftUnderItsName(t *testing.T) {
	dir := orphanFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "prod-orphan-file"))
	seedKeychainCredential(t, dir, "prod", "prod-orphan-keychain")

	out, err := PersistLoginCredential(t.Context(), dir, pluginCreateInput())
	require.NoError(t, err)
	warnings := joinWarnings(out.Warnings)
	assert.Contains(t, warnings, "removed a credential stored under prod that no entry named, from the keychain")
	assert.Contains(t, warnings, "removed a credential stored under prod that no entry named, from the plaintext file "+CredentialsFilePath(dir))
	assert.Empty(t, storedFileToken(t, dir), "the file copy is gone")
	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "prod"), "and the entry holds the new login's credential")
	assert.Equal(t, "staging-tok", keychainTokenFor(t, dir, "staging"), "another entry is untouched")

	again, err := PersistLoginCredential(t.Context(), dir, pluginCreateInput())
	require.NoError(t, err)
	assert.NotContains(t, joinWarnings(again.Warnings), "no entry named", "an entry that exists has no orphan to remove")
}

// TestALoginCreatingAnEntryRefusesAnOrphanInAnUnreachableKeychain asserts
// the refusal both halves give when the orphan is in a keychain the process
// cannot reach: the check before the sign-in, and the login under the lock
// with nothing written - no entry, no pin, no change record.
func TestALoginCreatingAnEntryRefusesAnOrphanInAnUnreachableKeychain(t *testing.T) {
	dir := orphanFixture(t)
	seedKeychainCredential(t, dir, "prod", "prod-orphan-keychain")
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	t.Cleanup(SetKeyringBackendForTest(unreachableKeyring{}))

	for name, run := range map[string]func() error{
		"the check before the sign-in": func() error { return CheckOrphanCredentials(dir, cfg, "prod") },
		"the login under the lock": func() error {
			_, err := PersistLoginCredential(t.Context(), dir, pluginCreateInput())
			return err
		},
	} {
		err := run()
		ce, ok := errorsAsClierr(err)
		require.True(t, ok, "%s: %v", name, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code, name)
		require.ErrorContains(t, err, "make the keychain reachable, or use a different --config", name)
		require.ErrorIs(t, err, errUnreachableKeyring, name)
	}
	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, after.Servers, "prod", "no entry was written")
	assert.NoFileExists(t, PinPath(dir))
	assert.NoFileExists(t, TransitionPath(dir))
	require.NoError(t, CheckOrphanCredentials(dir, &Config{Servers: map[string]Server{"prod": {}}}, "prod"),
		"an entry that exists is not an orphan")
}

// helperOrphanFixture is a directory declared exec:fake, with a
// store-capable helper, where the helper holds a credential under prod -
// recorded in the helper records - and config.yaml has no prod.
func helperOrphanFixture(t *testing.T) (dir, statePath string) {
	t.Helper()
	statePath = useFakeHelper(t, "ok")
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir = t.TempDir()
	cfg := &Config{CredentialStore: "exec:fake", Servers: map[string]Server{"staging": {URL: "https://staging.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, newExecStore("fake", dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-orphan-helper"))
	return dir, statePath
}

// TestALoginCreatingAnEntryRemovesAnOrphanFromTheHelperReadersAsk asserts
// the removal reaches a credential helper the directory reads from. A
// helper is asked directly, never through its records, so a credential it
// holds under the new entry's name and URL would be served to a reader of
// that entry from the moment the entry is written. The login erases it
// through the helper and removes the record, before the entry write, and
// says so; no reader of prod is ever served the orphan.
func TestALoginCreatingAnEntryRemovesAnOrphanFromTheHelperReadersAsk(t *testing.T) {
	dir, _ := helperOrphanFixture(t)
	var served []string
	t.Cleanup(SetAfterWriteHookForTest(func(string) {
		c, err := Load(FilePath(dir))
		if err != nil || c.Servers["prod"].URL == "" {
			return
		}
		if tok, gerr := newExecStore("fake", dir).GetToken(ServerRef{Name: "prod", URL: c.Servers["prod"].URL}); gerr == nil && tok == "prod-orphan-helper" {
			served = append(served, tok)
		}
	}))
	in := pluginCreateInput()
	in.Policy, in.ObservedDeclaration = "exec:fake", "exec:fake"
	in.Declaration = DeclarationFor("exec:fake", "", "exec:fake")

	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Empty(t, served, "no reader of prod is served the orphan")
	assert.Contains(t, joinWarnings(out.Warnings), "removed a credential stored under prod that no entry named, from the credential helper fake")
	tok, gerr := newExecStore("fake", dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, "id:new", tok, "the helper holds the new login's credential")
}

// TestALoginCreatingAnEntryRefusesAnOrphanTheHelperCannotErase asserts
// the refusal: a helper that fails to erase the orphan stops the login
// with nothing written - no entry, and the orphan still recorded.
func TestALoginCreatingAnEntryRefusesAnOrphanTheHelperCannotErase(t *testing.T) {
	dir, _ := helperOrphanFixture(t)
	t.Setenv("FAKEHELPER_MODE", "fail")
	in := pluginCreateInput()
	in.Policy, in.ObservedDeclaration = "exec:fake", "exec:fake"
	in.Declaration = DeclarationFor("exec:fake", "", "exec:fake")

	_, err := PersistLoginCredential(t.Context(), dir, in)
	require.ErrorContains(t, err, "credential helper")
	require.ErrorContains(t, err, "no entry names")
	after, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	assert.NotContains(t, after.Servers, "prod", "no entry was written")
	var recorded bool
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(st *helperRecords) error {
		recorded = len(st.forServer("prod")) > 0
		return nil
	}))
	assert.True(t, recorded, "the orphan is still recorded for a later attempt")
}

// TestRemoveOrphanCredentialsLeavesAConfiguredEntryAlone asserts the guard
// every caller relies on: a name config.yaml has is not an orphan, and
// nothing stored under it is touched.
func TestRemoveOrphanCredentialsLeavesAConfiguredEntryAlone(t *testing.T) {
	dir := orphanFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging"}, "staging-file"))
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)

	notices, err := RemoveOrphanCredentials(t.Context(), dir, cfg, "staging", nil)
	require.NoError(t, err)
	assert.Empty(t, notices)
	assert.Equal(t, "staging-tok", keychainTokenFor(t, dir, "staging"))
	tok, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "staging"})
	require.NoError(t, gerr)
	assert.Equal(t, "staging-file", tok)
}

// TestTheOrphanNoticeSaysWhatWasRemoved asserts the notice names what the
// removal did: when the orphan's keychain slot now holds another config
// directory's credential, or is already gone, only the record goes, and the
// notice says that rather than claiming a removal from the keychain.
func TestTheOrphanNoticeSaysWhatWasRemoved(t *testing.T) {
	for name, seed := range map[string]func(t *testing.T, dir string){
		"a slot another directory owns": func(t *testing.T, dir string) {
			t.Helper()
			id := seedKeychainCredential(t, dir, "prod", "ours")
			require.NoError(t, keyringSet(keychainAccount(id, "prod"), "another-directorys"))
		},
		"a slot already gone": func(t *testing.T, dir string) {
			t.Helper()
			id := seedKeychainCredential(t, dir, "prod", "ours")
			require.NoError(t, keyringDelete(keychainAccount(id, "prod")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := orphanFixture(t)
			seed(t, dir)
			cfg, err := Load(FilePath(dir))
			require.NoError(t, err)
			notices, err := RemoveOrphanCredentials(t.Context(), dir, cfg, "prod", nil)
			require.NoError(t, err)
			joined := joinWarnings(notices)
			assert.Contains(t, joined, "removed the keychain identity record of a credential stored under prod that no entry named; nothing was deleted from the keychain")
			assert.NotContains(t, joined, "that no entry named, from the keychain", "no claim that the keychain copy went")
			rec, rerr := readIdentityRecord(dir, "prod")
			require.NoError(t, rerr)
			assert.Nil(t, rec, "the record is gone")
		})
	}
}
