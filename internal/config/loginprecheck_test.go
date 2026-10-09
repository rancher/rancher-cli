package config

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinLeftStep is the pin a store change ends with, still to be written.
func pinLeftStep(target string) transitionStep {
	return transitionStep{Kind: transitionPin, Target: target}
}

// crossEntryChangeFixture is an `auto` directory whose store change moved
// prod's credential to the keychain and left only the pin to write: prod's
// credential and identity record are in the keychain, the pin still names
// the file store, and the change record is named in config.yaml. Logging
// staging in to the keychain is then a store change the lock accepts after
// the replay writes the pin, while prod's keychain credential, read on the
// directory as it is before the replay, looks like another entry's
// credential in the store being moved to.
func crossEntryChangeFixture(t *testing.T) (string, *Config) {
	t.Helper()
	dir := repinFixture(t)
	require.NoError(t, os.Remove(CredentialsFilePath(dir)))
	seedKeychainCredential(t, dir, "prod", "prod-keychain")
	return dir, namedChange(t, dir, pinLeftStep(backendKeychain))
}

// namedChange writes a change record with these steps in dir and the
// config.yaml that names it, and returns the config as loaded after.
func namedChange(t *testing.T, dir string, steps ...transitionStep) *Config {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	id, err := newTransitionID()
	require.NoError(t, err)
	require.NoError(t, writeTransition(dir, transitionManifest{
		Version: transitionVersion, ID: id, Steps: steps,
	}))
	cfg.CredentialTransitionID = id
	require.NoError(t, cfg.Write())
	return cfg
}

// TestTheCheckBeforeTheSignInLeavesLeftoverStepsToTheLock asserts the rule
// for a directory whose earlier store change has steps left: whatever the
// step is - a delete that can run now, a pin to write, a delete whose
// keychain cannot be reached, a step with a recorded failure, one whose
// last attempt was terminal - the check before the sign-in refuses nothing,
// because the login under the lock replays the steps first and decides on
// what the replay left. A check made before the replay would refuse what
// the lock then accepts.
func TestTheCheckBeforeTheSignInLeavesLeftoverStepsToTheLock(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	for name, step := range map[string]func(t *testing.T, dir string) transitionStep{
		"a delete that can run now": func(t *testing.T, dir string) transitionStep {
			t.Helper()
			return fileDeleteStep(t, dir, "staging", "https://s.ex", "staging-tok")
		},
		"a pin left": func(*testing.T, string) transitionStep { return pinLeftStep(backendKeychain) },
		"a delete whose keychain cannot be reached": func(t *testing.T, _ string) transitionStep {
			t.Helper()
			return unreachableDeleteStep(t, "staging")
		},
		"a step with a recorded failure": func(t *testing.T, _ string) transitionStep {
			t.Helper()
			s := unreachableDeleteStep(t, "staging")
			s.LastError = "the keychain is not reachable"
			return s
		},
		"a terminal step": func(t *testing.T, dir string) transitionStep {
			t.Helper()
			s := terminalDeleteStep(t, dir, "staging")
			s.Terminal = true
			s.LastError = "retiring the credential identity for staging: the record cannot be read"
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := activeTransition(t, dir, step(t, dir))

			left, err := transitionStepsLeft(dir, cfg)
			require.NoError(t, err)
			assert.True(t, left)

			commitsTo, err := LoginStoreChange(dir, cfg, "prod", "keychain", "keychain")
			require.NoError(t, err, "the lock decides after the replay")
			assert.Equal(t, backendKeychain, commitsTo)
		})
	}
}

// TestTheCheckBeforeTheSignInAcceptsATerminalStepAndTheLockRefusesWithItsError
// asserts the case where the earlier change has a step whose Terminal
// field is set: the check before the sign-in accepts, because a retry may
// clear what the last attempt found, and the login under the lock replays
// the step, finds it impossible again and refuses with that step's own
// non-retryable error.
func TestTheCheckBeforeTheSignInAcceptsATerminalStepAndTheLockRefusesWithItsError(t *testing.T) {
	dir := repinFixture(t)
	step := terminalDeleteStep(t, dir, "other")
	step.Terminal = true
	step.LastError = "retiring the credential identity for other: the record cannot be read"
	cfg := namedChange(t, dir, step)

	commitsTo, err := LoginStoreChange(dir, cfg, "prod", "keychain", "keychain")
	require.NoError(t, err, "a terminal step is the lock's to decide")
	assert.Equal(t, backendKeychain, commitsTo)

	in := repinInput()
	in.Policy, in.Declaration = "keychain", "keychain"
	_, err = PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code, "a terminal step keeps its own code")
	assert.False(t, ce.Retryable)
	require.ErrorContains(t, err, "the credential store change cannot finish for other")
}

// TestTheCheckBeforeTheSignInSkipsTheStoreRulesWhileStepsAreLeft asserts
// the case the store rules, not the transition refusal, would get wrong:
// prod's keychain credential counts against a login of staging that moves
// to the keychain, yet the replay of the pin step the earlier change left
// makes the keychain the store the directory already stands at. The check
// accepts, and the login under the lock accepts and finishes the change.
func TestTheCheckBeforeTheSignInSkipsTheStoreRulesWhileStepsAreLeft(t *testing.T) {
	dir, cfg := crossEntryChangeFixture(t)

	commitsTo, err := LoginStoreChange(dir, cfg, "staging", "keychain", "keychain")
	require.NoError(t, err)
	assert.Equal(t, backendKeychain, commitsTo)

	in := repinInput()
	in.Name, in.CanonicalURL = "staging", "https://staging.ex"
	in.Policy, in.Declaration = "keychain", "keychain"
	_, err = PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err, "the lock accepts what the check accepted")
	assert.Equal(t, backendKeychain, ReadPin(dir), "the replay wrote the pin")
	assert.Equal(t, "id:new", keychainTokenFor(t, dir, "staging"))
	assert.NoFileExists(t, TransitionPath(dir), "and the change finished")
}

// TestTheCheckBeforeTheSignInRunsTheStoreRulesWithNothingLeft asserts the
// check is whole when the earlier change has no steps left: the same
// directory as the one above, with the change record gone, is refused
// before the sign-in because prod still holds a credential in the keychain
// the pin does not yet name.
func TestTheCheckBeforeTheSignInRunsTheStoreRulesWithNothingLeft(t *testing.T) {
	dir, cfg := crossEntryChangeFixture(t)
	require.NoError(t, removeTransition(dir))
	cfg.CredentialTransitionID = ""
	require.NoError(t, cfg.Write())

	left, err := transitionStepsLeft(dir, cfg)
	require.NoError(t, err)
	assert.False(t, left)

	_, err = LoginStoreChange(dir, cfg, "staging", "keychain", "keychain")
	require.ErrorContains(t, err, "prod")
}

// TestTheCheckBeforeTheSignInRefusesAChangeRecordItCannotRead asserts the
// one answer the check gives about an earlier change: a record config.yaml
// names that cannot be read, or that is another change than the one it
// names. The lock refuses both for certain and the replay has nothing to
// offer, so nobody is asked to sign in, and the check's error is the one
// the lock gives.
func TestTheCheckBeforeTheSignInRefusesAChangeRecordItCannotRead(t *testing.T) {
	for name, mangle := range map[string]func(t *testing.T, dir string, cfg *Config){
		"a malformed record": func(t *testing.T, dir string, _ *Config) {
			t.Helper()
			require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [a, change"), 0o600))
		},
		"a record of another change": func(t *testing.T, _ string, cfg *Config) {
			t.Helper()
			cfg.CredentialTransitionID = "fedcba9876543210fedcba9876543210"
			require.NoError(t, cfg.Write())
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := repinFixture(t)
			cfg := namedChange(t, dir, pinLeftStep(backendKeychain))
			mangle(t, dir, cfg)

			_, err := transitionStepsLeft(dir, cfg)
			require.Error(t, err)
			in := repinInput()
			_, err = LoginStoreChange(dir, cfg, in.Name, in.Policy, in.Declaration)
			require.Error(t, err)
			ce, ok := errorsAsClierr(err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, TransitionPath(dir))

			_, perr := PersistLoginCredential(t.Context(), dir, in)
			require.Error(t, perr, "the lock refuses the same record")
			pce, pok := errorsAsClierr(perr)
			require.True(t, pok)
			assert.Equal(t, ce.Code, pce.Code)
			assert.Equal(t, err.Error(), perr.Error())
		})
	}
}

// TestTheCheckBeforeTheSignInResolvesTheBackendsBeforeItReadsTheRecord
// asserts the order the lock uses: a directory that has both an
// unreadable change record and a store the login cannot be given is
// refused for the store, not the record. Here the environment's credential
// store conflicts with the one the directory declares.
func TestTheCheckBeforeTheSignInResolvesTheBackendsBeforeItReadsTheRecord(t *testing.T) {
	dir := repinFixture(t)
	cfg := namedChange(t, dir, pinLeftStep(backendKeychain))
	require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [a, change"), 0o600))
	t.Setenv(EnvCredentialStore, "file")

	in := repinInput()
	_, err := LoginStoreChange(dir, cfg, in.Name, in.Policy, in.Declaration)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), TransitionPath(dir), "not the record")

	_, perr := PersistLoginCredential(t.Context(), dir, in)
	require.Error(t, perr)
	assert.Equal(t, perr.Error(), err.Error(), "the lock refuses for the same reason first")
}

// TestTheCheckBeforeTheSignInFindsNothingLeftWithNoChangeToSettle asserts
// the answers for a directory with no active change: none named, a record
// the config does not name (a login that died before activating it), and a
// change named whose record is already gone.
func TestTheCheckBeforeTheSignInFindsNothingLeftWithNoChangeToSettle(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir string, cfg *Config){
		"none named": func(t *testing.T, dir string, cfg *Config) {
			t.Helper()
			require.NoError(t, removeTransition(dir))
			cfg.CredentialTransitionID = ""
		},
		"a record the config does not name": func(_ *testing.T, _ string, cfg *Config) {
			cfg.CredentialTransitionID = ""
		},
		"named, with no record": func(t *testing.T, dir string, _ *Config) {
			t.Helper()
			require.NoError(t, removeTransition(dir))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := activeTransition(t, dir, pinLeftStep(backendKeychain))
			setup(t, dir, cfg)

			left, err := transitionStepsLeft(dir, cfg)
			require.NoError(t, err)
			assert.False(t, left)
		})
	}
}
