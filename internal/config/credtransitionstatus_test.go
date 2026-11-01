package config

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestTheStatusReportSeesWhatThePreludeSees asserts the one reading both
// status commands report: a change in flight with every step left, its
// kind, entry, store, whether it can still finish and why it stopped; a
// change that never started; and a record that cannot be used, named by
// config.yaml or not, with the diagnosis the prelude refuses with. A
// change whose last step finished is no row at all.
func TestTheStatusReportSeesWhatThePreludeSees(t *testing.T) {
	t.Run("nothing recorded costs one stat", func(t *testing.T) {
		dir := t.TempDir()
		ops := map[string]int{}
		restore := SetIdentityObserverForTest(func(op string) { ops[op]++ })
		defer restore()
		assert.Nil(t, ChangeRecordStatusOf(dir, &Config{}))
		assert.Equal(t, map[string]int{"manifeststat": 1}, ops)
	})

	t.Run("a finished change is no row", func(t *testing.T) {
		dir := t.TempDir()
		assert.Nil(t, ChangeRecordStatusOf(dir, &Config{CredentialTransitionID: strings.Repeat("a", transitionIDLen)}))
	})

	t.Run("a change in flight lists every step left", func(t *testing.T) {
		dir := t.TempDir()
		del := fileDeleteStep(t, dir, "prod", "https://r.ex", "prod-old")
		del.LastError = "the file store is read-only"
		stuck := unreachableDeleteStep(t, "staging")
		stuck.Terminal, stuck.LastError = true, "retiring the credential identity for staging: the record is a symlink"
		cfg := activeTransition(t, dir, del, stuck, transitionStep{Kind: transitionPin, Target: backendKeychain})

		s := ChangeRecordStatusOf(dir, cfg)
		require.NotNil(t, s)
		assert.Equal(t, ChangeRecordActive, s.State)
		assert.Equal(t, cfg.CredentialTransitionID, s.ID)
		assert.Equal(t, cfg.CredentialTransitionID, s.Named)
		assert.Equal(t, TransitionPath(dir), s.File)
		assert.Empty(t, s.Error)
		assert.Equal(t, []ChangeRecordStep{
			{Kind: "delete", Entry: "prod", Store: "file", LastError: "the file store is read-only"},
			{Kind: "delete", Entry: "staging", Store: "keychain", CannotFinish: true, LastError: "retiring the credential identity for staging: the record is a symlink"},
			{Kind: "pin", Store: "keychain"},
		}, s.Remaining)
		assert.True(t, s.Unfinished())
		assert.Equal(t, []string{
			"state: active - unfinished; the next auth login, auth logout, config delete-server or config update-server --url retries the work left",
			"id: " + cfg.CredentialTransitionID,
			"file: " + TransitionPath(dir),
			"remaining:",
			"  delete the old copy of prod's credential from file - could not finish: the file store is read-only",
			"  delete the old copy of staging's credential from keychain - cannot finish: retiring the credential identity for staging: the record is a symlink",
			"  switch the pin to keychain - not tried yet",
		}, s.TextLines())
	})

	t.Run("a change that never started", func(t *testing.T) {
		dir := t.TempDir()
		m := validManifest(t)
		require.NoError(t, writeTransition(dir, m))

		s := ChangeRecordStatusOf(dir, &Config{})
		require.NotNil(t, s)
		assert.Equal(t, ChangeRecordAbandoned, s.State)
		assert.Equal(t, m.ID, s.ID)
		assert.Empty(t, s.Named)
		assert.False(t, s.Unfinished(), "nothing it records was ever done")
		assert.Equal(t, "state: abandoned - a change that never started; the next auth login, auth logout, config delete-server or config update-server --url removes it", s.TextLines()[0])
	})

	for _, tc := range []struct {
		name  string
		named bool
		file  func(t *testing.T, dir string)
		want  string
	}{
		{name: "a named record that cannot be read", named: true, want: "is unreadable",
			file: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
			}},
		{name: "a record nothing names that cannot be read", want: "is unreadable",
			file: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600))
			}},
		{name: "a valid record under an id nothing names", named: true, want: "a change this directory does not name",
			file: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, writeTransition(dir, validManifest(t)))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.file(t, dir)
			cfg := &Config{}
			if tc.named {
				cfg.CredentialTransitionID = strings.Repeat("b", transitionIDLen)
			}

			s := ChangeRecordStatusOf(dir, cfg)
			require.NotNil(t, s)
			assert.Equal(t, ChangeRecordDamaged, s.State)
			assert.Equal(t, cfg.CredentialTransitionID, s.Named)
			assert.Empty(t, s.ID)
			assert.Empty(t, s.Remaining)
			assert.Contains(t, s.Error, TransitionPath(dir))
			assert.Contains(t, s.Error, tc.want)
			assert.Contains(t, s.Error, "restore a valid copy, or move it aside", "the diagnosis the prelude refuses with")
			assert.True(t, s.Unfinished(), "every command that runs the prelude refuses on it")
			assert.Equal(t, "state: damaged - "+s.Error, s.TextLines()[0])
		})
	}
}

// TestEffectiveCarriesTheUnfinishedChangeNote asserts the note rides from
// the one place an entry's missing credential is found to the error that
// reports it, read only when that error asks for it: a stored credential
// carries none.
func TestEffectiveCarriesTheUnfinishedChangeNote(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvURL, "")
	t.Setenv(EnvCACert, "")
	dir := t.TempDir()
	cfg := activeTransition(t, dir, fileDeleteStep(t, dir, "staging", "https://s.ex", "staging-old"))
	cfg.CurrentServer = "prod"
	require.NoError(t, cfg.Write())
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)

	// Effective itself reads no change record: the exec plugin takes the
	// same branch and goes on to serve its cache. The record is read only
	// when the "not logged in" error is built.
	ops := map[string]int{}
	restore := SetIdentityObserverForTest(func(op string) { ops[op]++ })
	eff, err := loaded.Effective(NewFileStore(dir))
	restore()
	require.NoError(t, err)
	assert.Empty(t, eff.Token)
	assert.Zero(t, ops["manifeststat"], "no change record is read to find a missing credential")
	assert.Equal(t, UnfinishedChange{Note: "a credential store change is unfinished; see it with: " + clierr.Rancher + " config status"}, eff.UnfinishedChange())

	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod", URL: "https://r.ex"}, "prod-tok"))
	eff, err = loaded.Effective(NewFileStore(dir))
	require.NoError(t, err)
	assert.Equal(t, "prod-tok", eff.Token)
	assert.Equal(t, UnfinishedChange{}, eff.UnfinishedChange(), "a stored credential is not the not-logged-in path")
}
