package register

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestCommitCreateRefusesANameThatPrintsAsAnotherBeforeItRemovesAnything
// asserts the commit step refuses a new name with an invisible character
// before the step that removes credentials left under the name runs, adds no
// entry and writes nothing; the same calls with an ordinary name succeed.
func TestCommitCreateRefusesANameThatPrintsAsAnotherBeforeItRemovesAnything(t *testing.T) {
	probe := Probe{CanonicalURL: "https://qa1.example.com"}

	t.Run("commitCreateWith does not run the hook", func(t *testing.T) {
		cfg := writableCfg(t, map[string]config.Server{}, "")
		before := readBytes(t, cfg.Path())
		hooked := false
		_, err := commitCreateWith(cfg, "\u2063-q", probe, func() error { hooked = true; return nil })
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, ce, "U+2063")
		assert.False(t, hooked, "the check precedes the removal of credentials left under the name")
		assert.Empty(t, cfg.Servers)
		assert.Equal(t, before, readBytes(t, cfg.Path()))
	})

	t.Run("CommitCreateUnderLock writes nothing and keeps a credential left under the name", func(t *testing.T) {
		t.Setenv(config.EnvCredentialStore, "file")
		dir := t.TempDir()
		name := "\u2063-q"
		ref := config.ServerRef{Name: name, URL: probe.CanonicalURL}
		require.NoError(t, config.NewFileStore(dir).SetToken(ref, "left-over"))

		_, err := CommitCreateUnderLock(t.Context(), dir, name, probe)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		_, statErr := os.Stat(config.FilePath(dir))
		require.ErrorIs(t, statErr, os.ErrNotExist, "no config file is written")
		tok, gerr := config.NewFileStore(dir).GetToken(ref)
		require.NoError(t, gerr)
		assert.Equal(t, "left-over", tok, "the credential left under the name is not removed")
	})

	t.Run("an ordinary name is created", func(t *testing.T) {
		cfg := writableCfg(t, map[string]config.Server{}, "")
		out, err := commitCreateWith(cfg, "-q", probe, nil)
		require.NoError(t, err)
		assert.True(t, out.Created)

		dir := t.TempDir()
		out, err = CommitCreateUnderLock(t.Context(), dir, "-q", probe)
		require.NoError(t, err)
		assert.True(t, out.Created)
	})
}

// TestCommitCreateKeepsAnEntryAlreadyStoredUnderSuchAName asserts a name that
// is already an entry is not refused: the same URL and CA answer "unchanged".
func TestCommitCreateKeepsAnEntryAlreadyStoredUnderSuchAName(t *testing.T) {
	stored := "qa\u200b1"
	cfg := writableCfg(t, map[string]config.Server{stored: {URL: "https://qa1.example.com"}}, stored)
	before := readBytes(t, cfg.Path())

	out, err := commitCreate(cfg, stored, Probe{CanonicalURL: "https://qa1.example.com"})
	require.NoError(t, err)
	assert.False(t, out.Created)
	assert.Equal(t, before, readBytes(t, cfg.Path()))
}
