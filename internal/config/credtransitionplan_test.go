package config

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// planFixture is a directory with two entries beside the one logging in,
// which is the case a store change has to answer for: the operator named
// one entry, and the others must not be logged out behind their back.
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

// TestPlanTransitionMovesTheOtherEntries asserts what a store change does
// to entries the operator did not name: it moves them, one step each, and
// says which. Logging them out would destroy logins nobody asked about.
func TestPlanTransitionMovesTheOtherEntries(t *testing.T) {
	dir, cfg := planFixture(t)
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))
	require.NoError(t, store.SetToken(ServerRef{Name: "qa", URL: "https://q.ex"}, "qa-tok"))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, backendKeychain, "file")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, []string{"qa", "staging"}, plan.Moved)
	require.Len(t, plan.manifest.Steps, 2)
	for _, s := range plan.manifest.Steps {
		assert.Equal(t, transitionMove, s.Kind)
		assert.Equal(t, backendFile, s.Source)
		assert.Equal(t, backendKeychain, s.Destination)
		assert.NotEmpty(t, s.DestAccountID, "a keychain destination is addressed by an account of its own")
		assert.NotEqual(t, "prod", s.Entry, "the entry logging in commits its own new credential")
	}
	assert.Empty(t, plan.PrePin, "the directory is already pinned")
}

// TestPlanTransitionSkipsEntriesWithNothingToMove asserts a directory
// whose other entries hold no credential records no move for them.
func TestPlanTransitionSkipsEntriesWithNothingToMove(t *testing.T) {
	dir, cfg := planFixture(t)
	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, backendKeychain, "file")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Empty(t, plan.Moved)
	assert.True(t, plan.Empty(), "a change with nothing to move records nothing")
}

// TestPlanTransitionPinsTheFileStoreFirstWhenUnpinned asserts the
// pre-commit pin. A directory declared `file` never reads the pin, so one
// leaving the file store may have none; writing `file` before anything
// else states what is already true, stays truthful if the login fails, and
// makes a change into `auto` the same case as a change away from a `file`
// pin.
func TestPlanTransitionPinsTheFileStoreFirstWhenUnpinned(t *testing.T) {
	dir, cfg := planFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, backendKeychain, "")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, backendFile, plan.PrePin)
}

// TestPlanTransitionRefusesAKeychainSlotAnotherDirectoryOwns asserts the
// answer arrives before the sign-in. A slot whose credential this
// directory did not store is another config directory's: moving it would
// take their login, so the change is refused with the entry named and the
// way out.
func TestPlanTransitionRefusesAKeychainSlotAnotherDirectoryOwns(t *testing.T) {
	dir, cfg := planFixture(t)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "staging", State: identityActive,
		ID: acct, Fingerprint: credentialFingerprint(acct, "ours"),
	}))
	require.NoError(t, keyringSet(keychainAccount(acct, "staging"), "another-directorys-token"))

	_, err := PlanTransition(t.Context(), dir, cfg, "prod", backendKeychain, backendFile, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "staging")
	require.ErrorContains(t, err, "another config directory")
	require.ErrorContains(t, err, "log that entry out first")
}

// TestPlanTransitionRefusesAnUnsettledLogin asserts the other
// pre-authentication refusal: an entry whose login never finished has a
// credential nobody can identify, so it cannot be moved.
func TestPlanTransitionRefusesAnUnsettledLogin(t *testing.T) {
	dir, cfg := planFixture(t)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "staging", State: identityPending,
		ID: acct, Fingerprint: credentialFingerprint(acct, "half-written"),
	}))

	_, err := PlanTransition(t.Context(), dir, cfg, "prod", backendKeychain, backendFile, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "did not finish")
	require.ErrorContains(t, err, "staging")
}

// TestPlanTransitionRefusesAHelperDestinationWithCredentialsToMove
// asserts the one place moving yields to a refusal. A credential helper's
// `store` cannot be probed without storing and its `get` may create a
// credential rather than return the one it was given, so no replay could
// ever prove a move into it landed - and a move that cannot be verified is
// one that can silently lose a credential.
func TestPlanTransitionRefusesAHelperDestinationWithCredentialsToMove(t *testing.T) {
	dir, cfg := planFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))

	_, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, "exec:vault", "")
	require.Error(t, err)
	require.ErrorContains(t, err, "credential helper cannot receive")
	require.ErrorContains(t, err, "staging")
}

// TestPlanTransitionIntoAHelperIsFineWithNothingToMove asserts the
// refusal above is about the other entries, not about helpers: a directory
// whose only entry is the one logging in has nothing that needs verifying.
func TestPlanTransitionIntoAHelperIsFineWithNothingToMove(t *testing.T) {
	dir, cfg := planFixture(t)
	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, "exec:vault", "")
	require.NoError(t, err)
	assert.True(t, plan.Empty())
}

// TestPlanTransitionRefusesAPolicyRatherThanABackend asserts a step can
// never carry `auto`: it is a question, and a pin written later would
// change the answer under work already scheduled.
func TestPlanTransitionRefusesAPolicyRatherThanABackend(t *testing.T) {
	dir, cfg := planFixture(t)
	_, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, "auto", "")
	require.ErrorContains(t, err, "physical backends")
}

// TestPlanTransitionToTheSameBackendIsNoChange asserts a login that does
// not actually move the directory records nothing.
func TestPlanTransitionToTheSameBackendIsNoChange(t *testing.T) {
	dir, cfg := planFixture(t)
	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, backendFile, "file")
	require.NoError(t, err)
	assert.Nil(t, plan)
	assert.True(t, plan.Empty())
}

// TestPlanAddsTheOldCopyDeleteAndThePinLast asserts the order the steps
// end in. The pin is last because under `auto` every entry that has not
// moved yet is still read through the old one.
func TestPlanAddsTheOldCopyDeleteAndThePinLast(t *testing.T) {
	dir, cfg := planFixture(t)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", backendFile, backendKeychain, "file")
	require.NoError(t, err)
	require.NoError(t, plan.AddOldCopyDelete("prod", backendFile, "prod-old-tok", "https://r.ex", ""))
	plan.AddPin(backendKeychain)
	require.NoError(t, plan.Write(dir))

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	require.Len(t, m.Steps, 3)
	assert.Equal(t, transitionMove, m.Steps[0].Kind)
	assert.Equal(t, transitionDelete, m.Steps[1].Kind)
	assert.Equal(t, "prod", m.Steps[1].Entry)
	assert.Equal(t, transitionPin, m.Steps[2].Kind)
	assert.Equal(t, backendKeychain, m.Steps[2].Target)
}

// TestPlanTransitionFromAHelperMovesOnlyWhatTheRecordsName asserts the
// filter on a credential-helper source. A helper's `get` may answer for
// credentials this CLI never stored - provisioned outside it - and those
// are not the directory's to move or to erase afterwards. Only the entries
// the helper records name for that helper are read, and each move carries
// the canonical URL the helper keys the credential on.
func TestPlanTransitionFromAHelperMovesOnlyWhatTheRecordsName(t *testing.T) {
	useFakeHelper(t, "ok")
	dir, cfg := planFixture(t)
	store := newExecStore("fake", dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))
	require.NoError(t, store.SetToken(ServerRef{Name: "qa", URL: "https://q.ex"}, "qa-tok"))
	// qa's credential stays in the helper, but the directory no longer
	// records that it stored it: the helper answers for it, the plan must
	// not.
	require.NoError(t, withHelperRecordsLock(t.Context(), dir, func(st *helperRecords) error {
		for _, e := range st.forServer("qa") {
			st.remove(e)
		}
		return nil
	}))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", "exec:fake", backendKeychain, "")
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, []string{"staging"}, plan.Moved)
	require.Len(t, plan.manifest.Steps, 1)
	step := plan.manifest.Steps[0]
	assert.Equal(t, "exec:fake", step.Source)
	assert.Equal(t, backendKeychain, step.Destination)
	assert.Equal(t, "https://s.ex", step.CanonicalURL, "a helper step is addressed by the URL it was scheduled with")
}

// TestPlanTransitionRefusesAHelperThatCreatesCredentialsOnDemand asserts
// the `expiresAt` refusal: a helper that answers `get` with a short-lived
// credential created on the way out stored nothing that could move, and
// the change is refused before the sign-in with the entry named.
func TestPlanTransitionRefusesAHelperThatCreatesCredentialsOnDemand(t *testing.T) {
	useFakeHelper(t, "ok")
	dir, cfg := planFixture(t)
	require.NoError(t, newExecStore("fake", dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))
	t.Setenv("FAKEHELPER_MODE", "expiresat")
	t.Setenv("FAKEHELPER_EXPIRESAT", time.Now().Add(time.Minute).UTC().Format(time.RFC3339))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", "exec:fake", backendFile, "")
	require.Error(t, err)
	assert.Nil(t, plan)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "on demand")
	require.ErrorContains(t, err, "staging")
	assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
}

// TestPlanTransitionRefusesWhenASourceCannotBeRead asserts that a backend
// which cannot answer at all is a typed refusal like any entry that cannot
// move - naming the entry, carrying the logout as the hint and the
// backend's error as the cause - rather than a bare error with no code an
// agent can branch on and no remedy.
func TestPlanTransitionRefusesWhenASourceCannotBeRead(t *testing.T) {
	dir, cfg := planFixture(t)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "staging", State: identityActive,
		ID: acct, Fingerprint: credentialFingerprint(acct, "ours"),
	}))
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	_, err := PlanTransition(t.Context(), dir, cfg, "prod", backendKeychain, backendFile, "keychain")
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
	require.ErrorContains(t, err, "staging")
	assert.ErrorIs(t, err, errUnreachableKeyring, "the backend's own error is the cause")
}

// TestPlanTransitionIntoTheFileStoreTakesNoOtherCredential asserts the
// refusal for a change into the plaintext file store from a credential
// helper: the helper's other entries are not moved into plaintext on a
// login that names one entry, and the entry is named with the logout that
// clears the way.
func TestPlanTransitionIntoTheFileStoreTakesNoOtherCredential(t *testing.T) {
	useFakeHelper(t, "ok")
	dir, cfg := planFixture(t)
	require.NoError(t, newExecStore("fake", dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "staging-tok"))

	plan, err := PlanTransition(t.Context(), dir, cfg, "prod", "exec:fake", backendFile, "")
	require.Error(t, err)
	assert.Nil(t, plan)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "the plaintext file store takes only the credential of the entry logging in")
	require.ErrorContains(t, err, "the credential of staging is still in exec:fake; log that entry out first")
	assert.Equal(t, "rancher auth logout 'staging'", ce.Hint)
}
