package config

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// planFixture is a directory with two entries beside the one logging in,
// which is the case a store change has to answer for: the operator named
// one entry, and the others must not be changed behind their back.
func planFixture(t *testing.T) (dir string, cfg *Config) {
	t.Helper()
	keyring.MockInit()
	dir = t.TempDir()
	cfg = &Config{Servers: map[string]Server{
		"prod":    {URL: "https://r.ex"},
		"staging": {URL: "https://s.ex"},
		"qa":      {URL: "https://q.ex"},
	}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, cfg
}

// TestAStoreChangeRefusesWhileAnotherEntryHoldsAFileCredential asserts
// the rule for the file store: a login that takes its own credential out
// of credentials.yaml leaves the others where they are, so it is refused
// while any of them is still there, naming each one. One entry gets the
// logout as a runnable hint; several get their names in the message.
func TestAStoreChangeRefusesWhileAnotherEntryHoldsAFileCredential(t *testing.T) {
	dir, cfg := planFixture(t)
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-tok"))
	require.NoError(t, store.SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))

	err := CheckTransitionSources(dir, cfg, "prod", backendFile, backendKeychain)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "a login changes the store of its own credential only")
	require.ErrorContains(t, err, "the credential of staging is still in the plaintext file "+CredentialsFilePath(dir))
	require.ErrorContains(t, err, "log that entry out first")
	assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)

	require.NoError(t, store.SetToken(ServerRef{Name: "qa", URL: "https://q.ex"}, "qa-tok"))
	err = CheckTransitionSources(dir, cfg, "prod", backendFile, backendKeychain)
	ce, ok = errorsAsClierr(err)
	require.True(t, ok)
	require.ErrorContains(t, err, "the credential of qa is still in")
	require.ErrorContains(t, err, "the credential of staging is still in")
	require.ErrorContains(t, err, "log those entries out first")
	assert.Empty(t, ce.Hint, "no single command covers two entries")
}

// TestAStoreChangeProceedsWhenOnlyItsOwnCredentialIsThere asserts the
// other side of the rule: the entry logging in may hold a credential in
// the store being left - it is the one that changes - and entries with
// nothing there do not stop it.
func TestAStoreChangeProceedsWhenOnlyItsOwnCredentialIsThere(t *testing.T) {
	dir, cfg := planFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-tok"))
	require.NoError(t, CheckTransitionSources(dir, cfg, "prod", backendFile, backendKeychain))

	seedKeychainCredential(t, dir, "prod", "prod-keychain")
	require.NoError(t, CheckTransitionSources(dir, cfg, "prod", backendKeychain, backendFile))
}

// TestAStoreChangeRefusesWhileAnotherEntryHoldsAKeychainCredential
// asserts every keychain answer other than absent refuses: a credential
// this directory stored, a login that never settled, and a slot another
// config directory owns now. A record whose slot is gone holds nothing.
func TestAStoreChangeRefusesWhileAnotherEntryHoldsAKeychainCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, dir string)
		want string
	}{
		{name: "a credential this directory stored", want: "the credential of staging is still in the keychain",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				seedKeychainCredential(t, dir, "staging", "staging-tok")
			}},
		{name: "a login that did not finish", want: "a login for staging did not finish",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				acct, err := newKeychainAccountID()
				require.NoError(t, err)
				require.NoError(t, writeIdentityRecord(dir, identityRecord{
					Version: identityVersion, Entry: "staging", State: identityPending,
					ID: acct, Fingerprint: credentialFingerprint(acct, "half-written"),
				}))
			}},
		{name: "a slot another directory owns", want: "belongs to another config directory",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				id := seedKeychainCredential(t, dir, "staging", "ours")
				require.NoError(t, keyringSet(keychainAccount(id, "staging"), "another-directorys-token"))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg := planFixture(t)
			tc.seed(t, dir)
			err := CheckTransitionSources(dir, cfg, "prod", backendKeychain, backendFile)
			ce, ok := errorsAsClierr(err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
		})
	}

	t.Run("a record whose slot is gone holds nothing", func(t *testing.T) {
		dir, cfg := planFixture(t)
		id := seedKeychainCredential(t, dir, "staging", "staging-tok")
		require.NoError(t, keyringDelete(keychainAccount(id, "staging")))
		require.NoError(t, CheckTransitionSources(dir, cfg, "prod", backendKeychain, backendFile))
	})
}

// TestAStoreChangeRefusesWhenTheStoreBeingLeftCannotBeRead asserts the
// refusal for a store that cannot answer at all. Logging the entry out
// cannot clear it - the logout has to reach the same store - so the
// remedy is the store itself or another directory, and no logout is
// offered as a hint.
func TestAStoreChangeRefusesWhenTheStoreBeingLeftCannotBeRead(t *testing.T) {
	t.Run("an unreachable keychain", func(t *testing.T) {
		dir, cfg := planFixture(t)
		seedKeychainCredential(t, dir, "staging", "staging-tok")
		restore := SetKeyringBackendForTest(unreachableKeyring{})
		defer restore()

		err := CheckTransitionSources(dir, cfg, "prod", backendKeychain, backendFile)
		ce, ok := errorsAsClierr(err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, "staging")
		require.ErrorContains(t, err, "make that store reachable, or use a different --config")
		assert.Empty(t, ce.Hint, "a logout cannot reach the store either")
		assert.ErrorIs(t, err, errUnreachableKeyring, "the backend's own error is the cause")
	})

	t.Run("an unreadable credentials file", func(t *testing.T) {
		dir, cfg := planFixture(t)
		require.NoError(t, os.WriteFile(CredentialsFilePath(dir), []byte("{not yaml"), 0o600))

		err := CheckTransitionSources(dir, cfg, "prod", backendFile, backendKeychain)
		ce, ok := errorsAsClierr(err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, CredentialsFilePath(dir)+" cannot be read")
		require.ErrorContains(t, err, "use a different --config")
		assert.Empty(t, ce.Hint)
	})
}

// TestAStoreChangeFromAHelperCountsEveryOtherEntry asserts the rule for a
// credential helper source. A helper's `get` may answer for any entry,
// and one that creates credentials keeps no record of them, so no read can
// say which entries it serves: every other entry in config.yaml counts,
// the helper is never asked, and the remedies are another directory or
// removing the entries, since a logout does not stop a helper answering.
func TestAStoreChangeFromAHelperCountsEveryOtherEntry(t *testing.T) {
	dir, cfg := planFixture(t)
	// No helper on PATH: asking it anything would be an error of its own.
	t.Setenv("PATH", t.TempDir())

	err := CheckTransitionSources(dir, cfg, "prod", "exec:fake", backendKeychain)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "the credential helper fake may answer for qa, staging")
	require.ErrorContains(t, err, "use a different --config for the new store")
	require.ErrorContains(t, err, "rancher config delete-server <name>")
	assert.Empty(t, ce.Hint, "two ways out, neither of them a logout")

	alone := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	require.NoError(t, CheckTransitionSources(dir, alone, "prod", "exec:fake", backendKeychain),
		"with no other entry there is nothing to strand")
}

// TestAStoreChangeRefusesWhileAnotherEntryHasACredentialLeftInTheDestination
// asserts the store a change moves the directory's readers to is read too.
// An entry logged out while its keychain copy could not be reached keeps
// the copy and its identity record; a change that sends every reader to
// the keychain would serve that entry the credential the operator logged
// out. So a credential another entry has left in the destination refuses
// like one it holds in the source, with the logout as the remedy - for the
// keychain, with the keychain reachable, since that logout has to reach
// it. For the keychain the record's name is enough: nothing is opened.
func TestAStoreChangeRefusesWhileAnotherEntryHasACredentialLeftInTheDestination(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		seed           func(t *testing.T, dir string)
		want           string
	}{
		{name: "an identity record in the keychain", from: backendFile, to: backendKeychain,
			want: "the keychain still holds a credential of staging from an earlier login, which a reader of staging would be served after the change; log that entry out first with the keychain reachable",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				// A record whose slot is gone still counts: its name is all
				// that is read.
				id := seedKeychainCredential(t, dir, "staging", "staging-a")
				require.NoError(t, keyringDelete(keychainAccount(id, "staging")))
			}},
		{name: "a token in credentials.yaml", from: backendKeychain, to: backendFile,
			want: "still holds a credential of staging from an earlier login, which a reader of staging would be served after the change; log that entry out first, then",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-a"))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg := planFixture(t)
			tc.seed(t, dir)
			err := CheckTransitionSources(dir, cfg, "prod", tc.from, tc.to)
			ce, ok := errorsAsClierr(err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, "a login changes the store of its own credential only")
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
		})
	}
}

// TestAStoreChangeIntoAHelperCountsEveryOtherEntry asserts the helper
// rule from the destination's side. A helper that creates credentials on
// demand keeps no record and answers for any entry, so after a change into
// it every other entry - a logged-out one included - would be served a
// credential it never logged in with. Every other configured entry counts,
// the helper is never asked, and the remedies are another directory or
// removing the entries.
func TestAStoreChangeIntoAHelperCountsEveryOtherEntry(t *testing.T) {
	dir, cfg := planFixture(t)
	t.Setenv("PATH", t.TempDir())

	err := CheckTransitionSources(dir, cfg, "prod", backendFile, "exec:fake")
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "the credential helper fake may answer for qa, staging: after the change each of them would be served a credential it never logged in with")
	require.ErrorContains(t, err, "use a different --config for the helper")
	require.ErrorContains(t, err, "rancher config delete-server <name>")
	assert.Empty(t, ce.Hint, "two ways out, neither of them a logout")

	alone := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	require.NoError(t, CheckTransitionSources(dir, alone, "prod", backendFile, "exec:fake"),
		"with no other entry there is nothing to serve")
}

// TestAStoreChangeNeedsAPhysicalBackendToLeave asserts a policy is never
// treated as a store: `auto` is a question, and nothing could be read from
// it.
func TestAStoreChangeNeedsAPhysicalBackendToLeave(t *testing.T) {
	dir, cfg := planFixture(t)
	require.ErrorContains(t, CheckTransitionSources(dir, cfg, "prod", "auto", backendFile), "physical backend")
}

// TestPlanAddsTheOldCopyDeleteAndThePinLast asserts the order the steps
// end in: the old copy's delete, then the pin.
func TestPlanAddsTheOldCopyDeleteAndThePinLast(t *testing.T) {
	dir, _ := planFixture(t)
	plan, err := newTransitionPlan()
	require.NoError(t, err)
	require.NoError(t, plan.AddOldCopyDelete("prod", backendFile, "prod-old-tok", "https://r.ex", ""))
	plan.AddPin(backendKeychain)
	require.NoError(t, plan.Write(dir))

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	require.Len(t, m.Steps, 2)
	assert.Equal(t, transitionDelete, m.Steps[0].Kind)
	assert.Equal(t, "prod", m.Steps[0].Entry)
	assert.Equal(t, transitionPin, m.Steps[1].Kind)
	assert.Equal(t, backendKeychain, m.Steps[1].Target)
}
