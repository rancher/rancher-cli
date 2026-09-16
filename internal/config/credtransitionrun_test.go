package config

import (
	"errors"
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

// fileMoveStep is a move of one entry's file-store credential into the
// keychain, with the credential present so the move can run.
func fileMoveStep(t *testing.T, dir, entry, url, token string) transitionStep {
	t.Helper()
	vid, err := newTransitionID()
	require.NoError(t, err)
	acct, aerr := newKeychainAccountID()
	require.NoError(t, aerr)
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: entry, URL: url}, token))
	return transitionStep{
		Kind: transitionMove, Entry: entry, Source: backendFile, Destination: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, token),
		DestAccountID: acct,
	}
}

// TestPrepareMutationFinishesTheWorkAndLetsTheCommandThrough asserts the
// ordinary case: the prelude replays what is left, the manifest goes away,
// and the command proceeds.
func TestPrepareMutationFinishesTheWorkAndLetsTheCommandThrough(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir,
		fileMoveStep(t, dir, "staging", "https://s.ex", "staging-tok"),
		transitionStep{Kind: transitionPin, Target: backendKeychain})

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{Entries: []string{"prod"}})
	require.NoError(t, err)

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	assert.Nil(t, m, "every step finished, so the record of the change is gone")
	assert.Equal(t, "keychain", ReadPin(dir), "the pin is the last step")

	ClearFinishedTransition(dir, cfg)
	assert.Empty(t, cfg.CredentialTransitionID, "the next config write forgets the name")
}

// TestPrepareMutationRefusesASecondStoreChange asserts the rule that keeps
// two changes from stacking. The first has credentials in a backend the
// directory no longer names; a second would forget it, and they would stay
// there with nothing to find them.
func TestPrepareMutationRefusesASecondStoreChange(t *testing.T) {
	dir := t.TempDir()
	stuck := fileMoveStep(t, dir, "staging", "https://s.ex", "staging-tok")
	// The credential is not where the change says it is, so the step
	// cannot finish and stays.
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "staging", URL: "https://s.ex"}, "something-else"))
	cfg := activeTransition(t, dir, stuck)

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{Entries: []string{"prod"}, ChangesStore: true})
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

// TestPrepareMutationRefusesALoginWhoseEntryAMoveNames asserts why a move
// blocks a login of its entry and a delete does not. The move is carrying
// a credential: retried after the login, it would write that credential
// over the one the login just stored. A delete carries only a digest, and
// the login's own credential does not match it.
func TestPrepareMutationRefusesALoginWhoseEntryAMoveNames(t *testing.T) {
	t.Run("a move for the entry refuses", func(t *testing.T) {
		dir := t.TempDir()
		stuck := fileMoveStep(t, dir, "prod", "https://r.ex", "prod-tok")
		require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "something-else"))
		cfg := activeTransition(t, dir, stuck)

		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{Entries: []string{"prod"}})
		require.Error(t, err)
		require.ErrorContains(t, err, "prod")
	})

	t.Run("a delete for the entry does not", func(t *testing.T) {
		dir := t.TempDir()
		vid, verr := newTransitionID()
		require.NoError(t, verr)
		// An unreachable keychain copy: the step stays, and it can only
		// ever remove a credential whose digest it carries.
		acct, aerr := newKeychainAccountID()
		require.NoError(t, aerr)
		cfg := activeTransition(t, dir, transitionStep{
			Kind: transitionDelete, Entry: "prod", Source: backendKeychain,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "old"),
			SourceAccountID: acct,
		})

		_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{Entries: []string{"prod"}})
		require.NoError(t, err, "a delete cannot restore anything, so it does not block a login")
	})
}

// TestPrepareMutationRewritesMovesAsDeletesForALogout asserts what lets a
// logout run while a change has work left. A move standing afterwards
// would undo the logout by writing the credential it is carrying into the
// place the logout just cleared; as two conditional deletes, whatever
// survives can only take a copy away.
func TestPrepareMutationRewritesMovesAsDeletesForALogout(t *testing.T) {
	dir := t.TempDir()
	stuck := fileMoveStep(t, dir, "prod", "https://r.ex", "prod-tok")
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "something-else"))
	cfg := activeTransition(t, dir, stuck)

	notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{
		Entries: []string{"prod"}, ClearsCredentials: true,
	})
	require.NoError(t, err, "the logout is not blocked by the change it just defused")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[0], "can no longer restore the credential being removed")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	assert.Nil(t, m, "both deletes found nothing of theirs to remove, so the change is over")

	tok, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, "something-else", tok,
		"the credential the move was carrying was never written back, and the one that is there was left alone")
}

// TestRewriteMovesAsDeletesSplitsBothSides asserts the shape of the
// rewrite itself: each move for a target entry becomes two conditional
// deletes, one for the place the credential was coming from and one for
// the place it was going to, so whichever copy exists is cleaned up and
// neither can be recreated. Moves for other entries are untouched.
func TestRewriteMovesAsDeletesSplitsBothSides(t *testing.T) {
	dir := t.TempDir()
	vid, err := newTransitionID()
	require.NoError(t, err)
	src, serr := newKeychainAccountID()
	require.NoError(t, serr)
	dst, derr := newKeychainAccountID()
	require.NoError(t, derr)
	id, ierr := newTransitionID()
	require.NoError(t, ierr)

	m := &transitionManifest{Version: transitionVersion, ID: id, Steps: []transitionStep{
		{
			Kind: transitionMove, Entry: "prod", Source: backendKeychain, Destination: backendFile,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
			SourceAccountID: src, DestAccountID: dst,
			// A move that had already been judged impossible: the verdict
			// is the move's, not the cleanup's.
			Terminal: true, LastError: "the credential is in neither store",
		},
		{
			Kind: transitionMove, Entry: "staging", Source: backendKeychain, Destination: backendFile,
			VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "tok"),
			SourceAccountID: src,
		},
		{Kind: transitionPin, Target: backendFile},
	}}

	notices, werr := rewriteMovesAsDeletes(dir, m, []string{"prod"})
	require.NoError(t, werr)
	require.Len(t, notices, 1)

	require.Len(t, m.Steps, 4)
	assert.Equal(t, transitionDelete, m.Steps[0].Kind)
	assert.Equal(t, backendKeychain, m.Steps[0].Source, "the side it was coming from")
	assert.Equal(t, src, m.Steps[0].SourceAccountID)
	assert.Equal(t, transitionDelete, m.Steps[1].Kind)
	assert.Equal(t, backendFile, m.Steps[1].Source, "the side it was going to")
	assert.Empty(t, m.Steps[1].Destination, "a delete has no destination")
	assert.Equal(t, transitionMove, m.Steps[2].Kind, "another entry's move is untouched")
	assert.Equal(t, "staging", m.Steps[2].Entry)
	assert.Equal(t, transitionPin, m.Steps[3].Kind)

	// Both deletes carry the digest of the credential the move was
	// carrying, so neither can remove anything else.
	assert.Equal(t, credentialFingerprint(vid, "tok"), m.Steps[0].CredentialDigest)
	assert.Equal(t, credentialFingerprint(vid, "tok"), m.Steps[1].CredentialDigest)

	// Neither inherits the move's verdict: they are new work no attempt
	// has judged, so a reader that has not replayed them yet reports work
	// left, never a terminal state with a logout that has just run as its
	// remedy.
	for _, d := range m.Steps[:2] {
		assert.False(t, d.Terminal, "%s %s", d.Kind, d.Source)
		assert.Empty(t, d.LastError)
	}
	rce, ok := errorsAsClierr(refusedBy(m, MutationIntent{ChangesStore: true}))
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, rce.Code)
	assert.True(t, rce.Retryable, "a delete nobody has tried is retryable work, whatever the move it replaced was")
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

// TestRefusedByTransitionAnswersBeforeAuthentication asserts the
// unlocked check a login runs before the sign-in. It exists so an
// operator is not asked to authenticate for a login that the locked
// prelude will refuse a moment later.
func TestRefusedByTransitionAnswersBeforeAuthentication(t *testing.T) {
	dir := t.TempDir()
	cfg := activeTransition(t, dir, fileMoveStep(t, dir, "prod", "https://r.ex", "prod-tok"))

	require.Error(t, RefusedByTransition(dir, cfg, MutationIntent{ChangesStore: true}))
	require.Error(t, RefusedByTransition(dir, cfg, MutationIntent{Entries: []string{"prod"}}))
	require.NoError(t, RefusedByTransition(dir, cfg, MutationIntent{Entries: []string{"staging"}}),
		"an entry no remaining move names is free to log in")
	require.NoError(t, RefusedByTransition(dir, &Config{}, MutationIntent{ChangesStore: true}),
		"a directory with no change recorded refuses nothing")
}

// TestErrNoTokenHintNamesTheStatusCommand asserts what an entry's "not
// logged in" says while a change is unfinished. Telling the operator to
// log in would be wrong: the credential is very likely sitting in the
// backend the change has not finished moving it out of.
func TestErrNoTokenHintNamesTheStatusCommand(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, ErrNoTokenHint(dir, &Config{}))

	cfg := activeTransition(t, dir, fileMoveStep(t, dir, "prod", "https://r.ex", "prod-tok"))
	assert.Equal(t, "rancher config status", ErrNoTokenHint(dir, cfg))
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

	notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{
		Entries: []string{"prod"}, ClearsCredentials: true,
	})
	require.NoError(t, err, "the logout goes on")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[len(notices)-1], "could not finish for prod",
		"and is told the work is still there, in the words the step recorded")
	assert.Contains(t, notices[len(notices)-1], "hint: rancher config status",
		"with where to look, since the logout goes on and no refusal will carry the hint; the logout that is running is no remedy for the delete it leaves")

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

// TestATerminalStepDoesNotRefuseAnotherEntrysCommand asserts the rule
// terminal and retained steps share: work the directory still has is a
// warning, and who may proceed is decided by the command's own entries
// and intent. A step that can never finish for one entry is no more
// reason to stop a logout of another than one that merely could not
// finish now. It also asserts what the step says when it does refuse: a
// terminal step keeps its own non-retryable verdict and the logout that
// clears it, never the retryable code - an agent branching on retryable
// must not be told to retry a state no retry can change.
func TestATerminalStepDoesNotRefuseAnotherEntrysCommand(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	vid, verr := newTransitionID()
	require.NoError(t, verr)
	// A move whose credential is in neither backend: terminal.
	cfg := activeTransition(t, dir, transitionStep{
		Kind: transitionMove, Entry: "other", Source: backendFile, Destination: backendKeychain,
		VerificationID: vid, CredentialDigest: credentialFingerprint(vid, "gone"),
		DestAccountID: mustAccount(t),
	})

	notices, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{
		Entries: []string{"prod"}, ClearsCredentials: true,
	})
	require.NoError(t, err, "the logout of prod is not this step's business")
	require.NotEmpty(t, notices)
	assert.Contains(t, notices[len(notices)-1], "other", "and is told what is stuck")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.True(t, m.Steps[0].Terminal, "the step records that no retry can help")
	assert.Contains(t, m.Steps[0].LastError, "neither the store", "and the cause, in its own words")
	assert.NotContains(t, m.Steps[0].LastError, "cannot finish for", "not the sentence that wraps it")

	// The same directory still refuses a second store change, with the
	// verdict the step earned rather than a retryable relabeling of it.
	for name, refuse := range map[string]func() error{
		"the locked prelude": func() error {
			_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
			return err
		},
		"the pre-authentication check": func() error {
			return RefusedByTransition(dir, cfg, MutationIntent{ChangesStore: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			serr := refuse()
			require.Error(t, serr)
			ce, ok := errorsAsClierr(serr)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code, "a terminal step keeps its own code")
			assert.False(t, ce.Retryable, "no retry can change where the credential is not")
			assert.Equal(t, "rancher auth logout 'other'", ce.Hint, "the remedy is the logout that turns the move into cleanup")
			require.ErrorContains(t, serr, "other")
		})
	}
}

// TestStepErrorKeepsATerminalVerdict asserts how a remaining step reports
// when it stops a command: one whose last attempt was terminal keeps its
// own non-retryable error and the logout that clears it; one that may
// still finish carries the retryable code and the status command.
func TestStepErrorKeepsATerminalVerdict(t *testing.T) {
	t.Parallel()
	terminal := stepError(transitionStep{
		Kind: transitionMove, Entry: "prod", Terminal: true,
		LastError: "the credential is in neither the store it was moving from nor the one it was moving to",
	})
	ce, ok := errorsAsClierr(terminal)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, "rancher auth logout 'prod'", ce.Hint)
	require.ErrorContains(t, terminal, "cannot finish for prod")
	require.ErrorContains(t, terminal, "neither the store")

	retained := stepError(transitionStep{Kind: transitionMove, Entry: "prod", LastError: "the keychain is locked"})
	rce, rok := errorsAsClierr(retained)
	require.True(t, rok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, rce.Code)
	assert.True(t, rce.Retryable)
	assert.Equal(t, "rancher auth logout 'prod'", rce.Hint, "an attempted step names the way out beside the retry")
	require.ErrorContains(t, retained, "could not finish for prod: the keychain is locked")

	untried := stepError(transitionStep{Kind: transitionDelete, Entry: "prod"})
	uce, uok := errorsAsClierr(untried)
	require.True(t, uok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, uce.Code)
	assert.True(t, uce.Retryable)
	assert.Equal(t, "rancher config status", uce.Hint)
	require.ErrorContains(t, untried, "delete prod); run this command again")
	assert.NotContains(t, untried.Error(), "resolved", "nothing to resolve is named for a step no attempt has judged")

	pin := stepError(transitionStep{Kind: transitionPin, Target: backendFile, LastError: "writing the pin: permission denied"})
	pce, pok := errorsAsClierr(pin)
	require.True(t, pok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, pce.Code)
	assert.True(t, pce.Retryable)
	assert.Equal(t, "rancher config status", pce.Hint, "a step naming no entry has no logout to name")
	require.ErrorContains(t, pin, "(pin): writing the pin: permission denied; run this command again once that is resolved")
	assert.NotContains(t, pin.Error(), "for :", "and no empty subject")
}

// TestARetainedStepReportsTheRetryableCode asserts the other half of the
// same rule: a step that may still finish once its backend answers is
// reported with the retryable code, and records that no verdict was
// final.
func TestARetainedStepReportsTheRetryableCode(t *testing.T) {
	dir := t.TempDir()
	stuck := fileMoveStep(t, dir, "other", "https://o.ex", "other-tok")
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "other", URL: "https://o.ex"}, "something-else"))
	cfg := activeTransition(t, dir, stuck)

	_, err := PrepareMutation(t.Context(), dir, cfg, MutationIntent{ChangesStore: true})
	require.Error(t, err)
	ce, ok := errorsAsClierr(err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, "rancher auth logout 'other'", ce.Hint,
		"the logout that turns the move into cleanup rides along, since some retained steps never clear on their own")
	require.ErrorContains(t, err, "could not finish for other")

	m, rerr := readTransition(dir)
	require.NoError(t, rerr)
	require.NotNil(t, m)
	assert.False(t, m.Steps[0].Terminal)
	assert.Contains(t, m.Steps[0].LastError, "not the one the change recorded")
}

// mustAccount is a fresh keychain account id for a test fixture.
func mustAccount(t *testing.T) string {
	t.Helper()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	return id
}
