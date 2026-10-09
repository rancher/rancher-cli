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

// activeTransition writes a manifest and the config that names it, which
// together are what "this directory has a store change in flight" means.
func activeTransition(t *testing.T, dir string, steps ...transitionStep) *Config {
	t.Helper()
	keyring.MockInit()
	id, err := newTransitionID()
	require.NoError(t, err)
	require.NoError(t, writeTransition(dir, transitionManifest{
		Version: transitionVersion, ID: id, Steps: steps,
	}))
	cfg := &Config{
		CredentialTransitionID: id,
		Servers:                map[string]Server{"prod": {URL: "https://r.ex"}, "staging": {URL: "https://s.ex"}},
	}
	cfg.SetPath(FilePath(dir))
	return cfg
}

// fileDeleteStep is the delete of one entry's old file-store copy, with
// the credential present so the delete can run.
func fileDeleteStep(t *testing.T, dir, entry, url, token string) transitionStep {
	t.Helper()
	vid, err := newTransitionID()
	require.NoError(t, err)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: entry, URL: url}, token))
	return transitionStep{
		Kind: transitionDelete, Entry: entry, Source: backendFile,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, token),
	}
}

// unreachableDeleteStep is the delete of one entry's old keychain copy.
// The caller makes the keychain unreachable, so the step cannot run and
// stays.
func unreachableDeleteStep(t *testing.T, entry string) transitionStep {
	t.Helper()
	vid, err := newTransitionID()
	require.NoError(t, err)
	return transitionStep{
		Kind: transitionDelete, Entry: entry, Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "old"),
		SourceAccountID: mustAccount(t),
	}
}

// terminalDeleteStep is the delete of one entry's old keychain copy whose
// slot is already empty and whose identity record does not parse, so the
// record it has to retire can never be trusted: no retry can finish it.
func terminalDeleteStep(t *testing.T, dir, entry string) transitionStep {
	t.Helper()
	require.NoError(t, os.MkdirAll(identityDirPath(dir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(identityDirPath(dir), identityFileName(entry)), []byte("not a record"), 0o600))
	return unreachableDeleteStep(t, entry)
}

// TestPrepareMutationFinishesTheWorkAndLetsTheCommandThrough asserts the
// ordinary case: the prelude replays what is left, the manifest goes away,
// and the command proceeds.
func TestPrepareMutationFinishesTheWorkAndLetsTheCommandThrough(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir,
		fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-old"),
		transitionStep{Kind: transitionPin, Target: backendKeychain})

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
	require.NoError(t, err)

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	assert.Nil(t, m, "every step finished, so the record of the change is gone")
	assert.Equal(t, "keychain", ReadPin(dir), "the pin is the last step")
	_, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.ErrorIs(t, gerr, ErrNoToken, "and the old copy is gone")

	ClearFinishedTransition(dir, cfg)
	assert.Empty(t, cfg.CredentialTransitionID, "the next config write forgets the name")
}

// TestPrepareMutationRefusesASecondStoreChange asserts the rule that keeps
// two changes from stacking. The first has an old copy in a backend the
// directory no longer names; a second would forget it, and it would stay
// there with nothing to find it.
func TestPrepareMutationRefusesASecondStoreChange(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir, unreachableDeleteStep(t, "staging"))
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.True(t, ce.Retryable)

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m, "the first change is still recorded")
	assert.Equal(t, cfg.CredentialTransitionID, m.ID, "and it is still the same one")
}

// TestPrepareMutationLetsALoginThroughWhileADeleteRemains asserts why a
// remaining delete for the entry does not block a login that changes no
// store: the delete carries only a digest, and the login's own credential
// does not match it, so a later retry can never remove what the login is
// about to store.
func TestPrepareMutationLetsALoginThroughWhileADeleteRemains(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir, unreachableDeleteStep(t, "prod"))
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
	require.NoError(t, err, "a delete cannot restore anything, so it does not block a login")
	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m, "the delete stays for the next command")
}

// TestPrepareMutationRemovesAbandonedWork asserts the state a login leaves
// when it dies between recording a change and activating it: a valid file
// the config never named. Nothing was moved, so it is removed rather than
// replayed, and the next store change is free to start.
func TestPrepareMutationRemovesAbandonedWork(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeTransition(dir, validManifest(t)))
	cfg := &Config{}
	cfg.SetPath(FilePath(dir))

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.NoError(t, err, "work that never started does not refuse a new change")
	assert.NoFileExists(t, TransitionPath(dir))
}

// TestTheCheckBeforeTheSignInReplaysAndWritesNothing asserts the unlocked
// check a login runs before the sign-in only reads: a step that the
// locked prelude would finish is reported as left, and the credential it
// would delete and the change record are both still there afterwards.
func TestTheCheckBeforeTheSignInReplaysAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir, fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-tok"))

	left, err := transitionStepsLeft(dir, cfg)
	require.NoError(t, err)
	assert.True(t, left)
	assert.Equal(t, "prod-tok", storedFileToken(t, dir), "nothing was deleted")
	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.Len(t, m.Steps, 1, "and the record is as it was")

	left, err = transitionStepsLeft(dir, &Config{})
	require.NoError(t, err)
	assert.False(t, left, "a directory with no change recorded has nothing left")
}

// TestUnfinishedChangeNoteNamesTheStatusCommand asserts what an error with
// a remedy of its own - an entry's "not logged in", say - adds about the
// directory's store change. While a change is unfinished the login is
// still the remedy, and what is missing may be the pin the change has not
// written yet, so the message says where to see it. While a record
// config.yaml names cannot be read, the login would be refused on it, so
// the status report becomes the hint and the login moves into the
// message. With no change named it opens nothing, which is what keeps the
// paths that call it free.
func TestUnfinishedChangeNoteNamesTheStatusCommand(t *testing.T) {
	dir := t.TempDir()
	ops := map[string]int{}
	restore := SetIdentityObserverForTest(func(op string) { ops[op]++ })
	assert.Equal(t, UnfinishedChange{}, UnfinishedChangeOf(dir, &Config{}))
	require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
	assert.Equal(t, UnfinishedChange{}, UnfinishedChangeOf(dir, &Config{}), "a record config.yaml does not name is not looked at here")
	restore()
	assert.Empty(t, ops, "no change named, no file opened")
	require.NoError(t, os.Remove(TransitionPath(dir)))

	notLoggedIn := func() error {
		return clierr.New(clierr.CodeNotLoggedIn, `not logged in to "prod"`).WithHint(clierr.Rancher + " auth login 'prod'")
	}
	cfg := activeTransition(t, dir, fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-tok"))
	const note = "a credential store change is unfinished; run " + clierr.Rancher + " config status to see it"
	active := UnfinishedChangeOf(dir, cfg)
	assert.Equal(t, UnfinishedChange{Note: note}, active)
	err := active.Apply(notLoggedIn())
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Equal(t, `not logged in to "prod"; `+note, ce.Message)
	assert.Equal(t, clierr.Rancher+" auth login 'prod'", ce.Hint, "the login still fixes it")
	assert.Equal(t, err, active.Apply(err), "a message that carries the note is not given it twice")

	require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
	damaged := UnfinishedChangeOf(dir, cfg)
	assert.True(t, damaged.Refuses, "a named record that cannot be read refuses the login")
	err = damaged.Apply(notLoggedIn())
	ce, ok = errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint)
	assert.Equal(t, `not logged in to "prod"; the credential store change record cannot be read, so every auth login, auth logout, config delete-server and config update-server --url refuses until it is restored or moved aside; run `+clierr.Rancher+` config status to see it; after that: `+clierr.Rancher+` auth login 'prod'`, ce.Message,
		"the login stays in the message, as what to run once the record is restored")

	require.NoError(t, os.Remove(TransitionPath(dir)))
	assert.Equal(t, UnfinishedChange{}, UnfinishedChangeOf(dir, cfg), "a change whose last step finished is not")
}

// TestALogoutFinishesWhileAStepCannot asserts the guarantee the merge
// rules exist for: a headless directory whose old keychain copy is
// unreachable can still log out. The step that cannot run stays and is
// reported as a warning; it does not refuse the command, because a delete
// can only ever take a copy away and never bring one back.
func TestALogoutFinishesWhileAStepCannot(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, verr := newTransitionID()
	require.NoError(t, verr)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)

	// A keychain delete whose backend cannot be reached.
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	cfg := activeTransition(t, dir, transitionStep{
		Kind: transitionDelete, Entry: "prod", Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "old"),
		SourceAccountID: acct,
	})

	notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
	require.NoError(t, err, "the logout goes on")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[len(notices)-1], "could not finish for prod",
		"and is told the work is still there, in the words the step recorded")
	assert.Contains(t, notices[len(notices)-1], "hint: "+clierr.Rancher+" config status",
		"and names the report that shows it: the logout that is running is no remedy for the delete it leaves")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m, "the step stays for the next command")
	assert.NotEmpty(t, m.Steps[0].LastError, "carrying why, for config status")
}

// TestAStoreChangeIsStillRefusedWhileAStepCannot asserts the other side of
// the same state: a command that would start a second change is refused,
// because two changes cannot stack.
func TestAStoreChangeIsStillRefusedWhileAStepCannot(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, verr := newTransitionID()
	require.NoError(t, verr)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	cfg := activeTransition(t, dir, transitionStep{
		Kind: transitionDelete, Entry: "prod", Source: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "old"),
		SourceAccountID: acct,
	})

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
}

// unreachableKeyring is a keychain no operation can reach: a locked
// keychain, a headless machine with no D-Bus, a Credential Manager that
// will not answer.
type unreachableKeyring struct{}

func (unreachableKeyring) Set(string, string, string) error { return errUnreachableKeyring }
func (unreachableKeyring) Get(string, string) (string, error) {
	return "", errUnreachableKeyring
}
func (unreachableKeyring) Delete(string, string) error { return errUnreachableKeyring }

var errUnreachableKeyring = errors.New("the keychain is not reachable")

// TestATerminalStepDoesNotRefuseAnotherCommand asserts the rule terminal
// and retained steps share: work the directory still has is a warning, and
// who may proceed is decided by the command's own intent. A step that can
// never finish is no more reason to stop a command that changes no store
// than one that merely could not finish now. It also asserts what the step
// says when it does refuse: a terminal step keeps its own non-retryable
// verdict, never the retryable code - an agent branching on retryable must
// not be told to retry a state no retry can change.
func TestATerminalStepDoesNotRefuseAnotherCommand(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := activeTransition(t, dir, terminalDeleteStep(t, dir, "other"))

	notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{})
	require.NoError(t, err, "a command that changes no store is not this step's business")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[len(notices)-1], "other", "and is told what is stuck")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.True(t, m.Steps[0].Terminal, "the step records that no retry can help")
	assert.Contains(t, m.Steps[0].LastError, "retiring the credential identity for other", "and the cause, in its own words")
	assert.NotContains(t, m.Steps[0].LastError, "cannot finish for", "not the sentence that wraps it")

	// The same directory still refuses a second store change under the
	// lock, with the verdict the step earned rather than a retryable
	// relabeling of it. The check before the sign-in leaves that to the
	// lock: a retry may clear what the last attempt found.
	_, serr := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.Error(t, serr)
	ce, ok := errorsAsClierr(serr)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code, "a terminal step keeps its own code")
	assert.False(t, ce.Retryable, "no retry can change a record that cannot be read")
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint, "the report that shows the step")
	require.ErrorContains(t, serr, "other")
}

// TestStepErrorKeepsATerminalVerdict asserts how a remaining step reports
// when it stops a command: one whose last attempt was terminal keeps its
// own non-retryable error; one that may still finish carries the retryable
// code. Each names rancher config status, the report that shows the step:
// no other command finishes it.
func TestStepErrorKeepsATerminalVerdict(t *testing.T) {
	t.Parallel()
	terminal := stepError(transitionStep{
		Kind: transitionDelete, Entry: "prod", Terminal: true,
		LastError: "retiring the credential identity for prod: the record is a symlink",
	})
	ce, ok := errorsAsClierr(terminal)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint)
	require.ErrorContains(t, terminal, "cannot finish for prod")
	require.ErrorContains(t, terminal, "the record is a symlink")

	retained := stepError(transitionStep{Kind: transitionDelete, Entry: "prod", LastError: "the keychain is locked"})
	rce, rok := errorsAsClierr(retained)
	require.True(t, rok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, rce.Code)
	assert.True(t, rce.Retryable)
	assert.Equal(t, clierr.Rancher+" config status", rce.Hint)
	require.ErrorContains(t, retained, "could not finish for prod: the keychain is locked")

	untried := stepError(transitionStep{Kind: transitionDelete, Entry: "prod"})
	uce, uok := errorsAsClierr(untried)
	require.True(t, uok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, uce.Code)
	assert.True(t, uce.Retryable)
	assert.Equal(t, clierr.Rancher+" config status", uce.Hint)
	require.ErrorContains(t, untried, "delete prod); run this command again")
	assert.NotContains(t, untried.Error(), "resolved", "nothing to resolve is named for a step no attempt has judged")

	pin := stepError(transitionStep{Kind: transitionPin, Target: backendFile, LastError: "writing the pin: permission denied"})
	pce, pok := errorsAsClierr(pin)
	require.True(t, pok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, pce.Code)
	assert.True(t, pce.Retryable)
	assert.Equal(t, clierr.Rancher+" config status", pce.Hint, "a pin step too: no other command writes it")
	require.ErrorContains(t, pin, "(pin): writing the pin: permission denied; run this command again once that is resolved")
	assert.NotContains(t, pin.Error(), "for :", "and no empty subject")
}

// TestARetainedStepReportsTheRetryableCode asserts the other half of the
// same rule: a step that may still finish once its backend answers is
// reported with the retryable code, and records that no verdict was
// final.
func TestARetainedStepReportsTheRetryableCode(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir, unreachableDeleteStep(t, "other"))
	restore := SetKeyringBackendForTest(unreachableKeyring{})
	defer restore()

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, clierr.Rancher+" config status", ce.Hint)
	require.ErrorContains(t, err, "could not finish for other")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.False(t, m.Steps[0].Terminal)
	assert.Contains(t, m.Steps[0].LastError, errUnreachableKeyring.Error())
}

// mustAccount is a fresh keychain account id for a test fixture.
func mustAccount(t *testing.T) string {
	t.Helper()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	return id
}
