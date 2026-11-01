package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestUnpinnedStoreCountsConfiguredEntriesOnly asserts the four answers and
// what they are made of. Only entries in config.yaml count: an orphan
// token or identity record has no entry, so no reader can be served it,
// and counting it would turn a directory with one store into the
// both-stores error no command could clear. A pending record counts - it
// is a keychain credential on its way in.
func TestUnpinnedStoreCountsConfiguredEntriesOnly(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		file, keychain, pending      []string
		want                         UnpinnedRow
		wantKeychain, wantFileTokens []string
	}{
		{name: "nothing", want: UnpinnedNothing},
		{name: "file tokens", file: []string{"prod"}, want: UnpinnedFile, wantFileTokens: []string{"prod"}},
		{name: "records", keychain: []string{"prod"}, want: UnpinnedKeychain, wantKeychain: []string{"prod"}},
		{name: "a pending record", pending: []string{"prod"}, want: UnpinnedKeychain, wantKeychain: []string{"prod"}},
		{name: "both", file: []string{"staging"}, keychain: []string{"prod"}, want: UnpinnedBoth,
			wantKeychain: []string{"prod"}, wantFileTokens: []string{"staging"}},
		{name: "an orphan token beside records", file: []string{"orphan"}, keychain: []string{"prod"}, want: UnpinnedKeychain,
			wantKeychain: []string{"prod"}},
		{name: "an orphan record beside file tokens", file: []string{"prod"}, keychain: []string{"orphan"}, want: UnpinnedFile,
			wantFileTokens: []string{"prod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			dir := t.TempDir()
			cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}, "staging": {URL: "https://s.ex"}}}
			for _, e := range tc.file {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: e}, e+"-tok"))
			}
			for _, e := range tc.keychain {
				seedKeychainCredential(t, dir, e, e+"-tok")
			}
			for _, e := range tc.pending {
				id := mustAccount(t)
				require.NoError(t, writeIdentityRecord(dir, identityRecord{
					Version: identityVersion, Entry: e, State: identityPending,
					ID: id, Fingerprint: credentialFingerprint(id, "half"),
				}))
			}
			u, err := UnpinnedStoreOf(dir, cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.want, u.Row)
			assert.Equal(t, tc.wantKeychain, u.KeychainEntries)
			assert.Equal(t, tc.wantFileTokens, u.FileEntries)
			assert.Equal(t, tc.want == UnpinnedBoth, IsCredentialsInBothStores(u.Err()))
			assert.Equal(t, tc.want == UnpinnedKeychain || tc.want == UnpinnedBoth, u.UsesKeychain())
		})
	}
}

// TestTheBothStoresErrorNamesTheWayOut asserts the error's text: the
// entries in each store, the logouts that keep the keychain, which holds
// no plaintext, that a keychain entry's logout needs the keychain, and a
// different --config - and nothing that tells the operator to write the
// pin by hand, which would pick a copy that may serve the wrong user.
// While a store change is unfinished the message also names it, with the
// logout still the hint.
func TestTheBothStoresErrorNamesTheWayOut(t *testing.T) {
	u := Unpinned{Row: UnpinnedBoth, KeychainEntries: []string{"prod"}, FileEntries: []string{"staging"}, dir: "/d"}
	err := u.Err()
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, "VALIDATION", string(ce.Code))
	require.ErrorContains(t, err, "the keychain (prod) and in the plaintext file "+filepath.Join("/d", "credentials.yaml")+" (staging)")
	require.ErrorContains(t, err, "to keep the keychain, which holds no plaintext, run: "+clierr.Rancher+" auth logout 'staging'")
	require.ErrorContains(t, err, "a keychain entry's logout needs the keychain reachable")
	require.ErrorContains(t, err, "or use a different --config")
	assert.Equal(t, clierr.Rancher+" auth logout 'staging'", ce.Hint)
	assert.NotContains(t, err.Error(), pinFileName)
	assert.NotContains(t, err.Error(), "config status", "with no change unfinished there is nothing more to see")

	// While a store change is unfinished, the logout stays the hint and
	// the message names the change, which is what may have left the copy.
	dir := t.TempDir()
	pending := activeTransition(t, dir, fileDeleteStep(t, dir, "staging", "https://s.ex", "staging-old"))
	withChange := Unpinned{Row: UnpinnedBoth, KeychainEntries: []string{"prod"}, FileEntries: []string{"staging"}, dir: dir, cfg: pending}
	wce, ok := errorsAsClierr(withChange.Err())
	require.True(t, ok)
	require.ErrorContains(t, withChange.Err(), "a credential store change is unfinished; see it with: "+clierr.Rancher+" config status")
	assert.Equal(t, clierr.Rancher+" auth logout 'staging'", wce.Hint, "the logout is still what fixes it")

	// While the record it names cannot be read, the logout would be
	// refused on it: the report is the hint, the logout stays in the
	// message.
	require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
	dce, ok := errorsAsClierr(withChange.Err())
	require.True(t, ok)
	require.ErrorContains(t, withChange.Err(), "the credential store change record cannot be read")
	require.ErrorContains(t, withChange.Err(), "run: "+clierr.Rancher+" auth logout 'staging'")
	assert.Equal(t, 1, strings.Count(withChange.Err().Error(), clierr.Rancher+" auth logout 'staging'"), "a remedy the message already names is not added again")
	assert.Equal(t, clierr.Rancher+" config status", dce.Hint)

	two := Unpinned{Row: UnpinnedBoth, KeychainEntries: []string{"prod"}, FileEntries: []string{"qa", "staging"}, dir: "/d"}
	tce, ok := errorsAsClierr(two.Err())
	require.True(t, ok)
	require.ErrorContains(t, two.Err(), "run: "+clierr.Rancher+" auth logout 'qa'; "+clierr.Rancher+" auth logout 'staging'")
	assert.Empty(t, tce.Hint, "no single command covers two entries")
}

// TestAnUnpinnedReadCountsItsOperations asserts the cost item by item.
// A read on an `auto` directory with no pin looks at the names in
// credstore-id/ once and reads credentials.yaml once to decide, opens no
// record to decide, takes no lock, calls the keychain only to read the
// credential it decided on, probes nothing and writes nothing.
func TestAnUnpinnedReadCountsItsOperations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keychain bool
		// wantDirStat counts the stat of credstore-id/: one to decide,
		// and on the keychain one more for the record the read opens;
		// wantList is the look at its names, which an absent directory
		// does not need.
		wantDirStat, wantList, wantRecordReads, wantKeyring int
	}{
		{name: "the file store", wantDirStat: 1},
		{name: "the keychain", keychain: true, wantDirStat: 2, wantList: 1, wantRecordReads: 1, wantKeyring: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(EnvCredentialStore, "")
			dir := t.TempDir()
			cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}, "staging": {URL: "https://s.ex"}}}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			if tc.keychain {
				seedKeychainCredential(t, dir, "prod", "prod-tok")
				seedKeychainCredential(t, dir, "staging", "staging-tok")
			} else {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "prod-tok"))
			}
			probes := 0
			orig := keyringUsable
			keyringUsable = func() error { probes++; return nil }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })

			ops := map[string]int{}
			t.Cleanup(SetIdentityObserverForTest(func(op string) { ops[op]++ }))
			keyringOps := 0
			SetKeyringObserverForTest(func(string) { keyringOps++ })
			t.Cleanup(func() { SetKeyringObserverForTest(nil) })
			locks, flocks, writes := 0, 0, 0
			t.Cleanup(SetLockHookForTest(func() { locks++ }))
			t.Cleanup(SetWaitForFlockHookForTest(func() { flocks++ }))
			t.Cleanup(SetAfterWriteHookForTest(func(string) { writes++ }))

			store, err := NewLazyStore(cfg, dir, false)
			require.NoError(t, err)
			tok, err := store.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
			require.NoError(t, err)
			assert.Equal(t, "prod-tok", tok)

			assert.Equal(t, tc.wantDirStat, ops["dirstat"])
			assert.Equal(t, tc.wantList, ops["list"], "at most one look at the names in credstore-id/")
			assert.Equal(t, tc.wantRecordReads, ops["recordread"], "no record is opened to decide")
			assert.Equal(t, tc.wantKeyring, keyringOps, "the keychain is called only to read the credential")
			assert.Zero(t, probes, "no probe")
			assert.Zero(t, locks, "no config lock")
			assert.Zero(t, flocks, "no other lock")
			assert.Zero(t, writes, "and nothing written")
			assert.NoFileExists(t, PinPath(dir))
			_, statErr := os.Stat(filepath.Join(dir, "credstore-id"))
			assert.Equal(t, tc.keychain, statErr == nil, "no identity directory is created by a read")
		})
	}
}

// TestDeleteLoginTokenOnAnUnpinnedDirectory asserts the logout's answer for
// an `auto` directory with no pin: the keychain is the directory's store
// whenever an entry in config.yaml has an identity record - the
// records-only and the both-stores answers alike - so a keychain it cannot
// reach fails the logout with the way out rather than exiting 0 with the
// record kept. A directory whose configured entries keep their
// credentials in the file store only warns about a leftover record, as a
// declared file directory does.
func TestDeleteLoginTokenOnAnUnpinnedDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fileEntry seeds staging's token in credentials.yaml; orphan
		// makes prod's record the only one while prod is not configured.
		fileEntry, orphan bool
		wantErr           bool
	}{
		{name: "records only", wantErr: true},
		{name: "both stores", fileEntry: true, wantErr: true},
		{name: "a leftover record beside file tokens", fileEntry: true, orphan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Setenv(EnvCredentialStore, "")
			dir := t.TempDir()
			cfg := &Config{Servers: map[string]Server{"staging": {URL: "https://s.ex"}}}
			if !tc.orphan {
				cfg.Servers["prod"] = Server{URL: "https://r.ex"}
			}
			cfg.SetPath(FilePath(dir))
			require.NoError(t, cfg.Write())
			seedKeychainCredential(t, dir, "prod", "prod-tok")
			if tc.fileEntry {
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))
			}
			t.Cleanup(SetKeyringBackendForTest(unreachableKeyring{}))

			_, notices, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://r.ex"}, "auto")
			rec, rerr := readIdentityRecord(dir, "prod")
			require.NoError(t, rerr)
			assert.NotNil(t, rec, "the record stays while the credential may still be there")
			if tc.wantErr {
				require.ErrorIs(t, err, errUnreachableKeyring)
				require.ErrorContains(t, err, "make the keychain reachable, or use a different --config")
				assert.False(t, IsCredentialsInBothStores(err), "the both-stores error is never raised here: a logout is its remedy")
				return
			}
			require.NoError(t, err)
			assert.Contains(t, joinWarnings(notices), "the credential identity record was kept")
		})
	}
}
