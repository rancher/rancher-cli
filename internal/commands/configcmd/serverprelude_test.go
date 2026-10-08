package configcmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// deleteServerRun runs config delete-server on prod.
func deleteServerRun(t *testing.T, dir string) ([]string, error) {
	t.Helper()
	res, err := NewConfigDeleteServer(dir).(*configDeleteServerCmd).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	return res.Warnings, err
}

// updateServerURLRun runs config update-server on prod with a new URL,
// which clears its credentials.
func updateServerURLRun(t *testing.T, dir string) ([]string, error) {
	t.Helper()
	res, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://prod2.ex"}, Pos: []string{"prod"}}, nil)
	return res.Warnings, err
}

// TestServerChangesFinishTheStoreChangeFirst asserts config delete-server
// and config update-server with a URL change run the prelude after their
// own validation and before their first write, with the rows every
// credential command shares.
func TestServerChangesFinishTheStoreChangeFirst(t *testing.T) {
	t.Run("delete-server", func(t *testing.T) { cmdtest.RunPreludeRows(t, deleteServerRun) })
	t.Run("update-server --url", func(t *testing.T) { cmdtest.RunPreludeRows(t, updateServerURLRun) })
}

// TestServerChangesValidateBeforeThePrelude asserts a server change that
// fails its own validation - an unknown server, a store variable that
// disagrees with the declaration, an update whose entry changed under it
// - or that clears no credential, replays nothing and is not refused by a
// record that cannot be read.
func TestServerChangesValidateBeforeThePrelude(t *testing.T) {
	conflicting := func(t *testing.T) {
		t.Helper()
		t.Setenv(config.EnvCredentialStore, "keychain")
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir string) error
	}{
		{"delete-server with a store variable against the declaration", func(t *testing.T, dir string) error {
			t.Helper()
			conflicting(t)
			_, err := deleteServerRun(t, dir)
			return err
		}},
		{"update-server --url with a store variable against the declaration", func(t *testing.T, dir string) error {
			t.Helper()
			conflicting(t)
			_, err := updateServerURLRun(t, dir)
			return err
		}},
		{"update-server --url whose entry changed before the lock", func(t *testing.T, dir string) error {
			t.Helper()
			// The probe runs between the command's first read and its
			// locked one; an entry that moved there fails the plan.
			c := &configUpdateServerCmd{dir: dir, tlsPreflight: func(context.Context, string, string) (string, error) {
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				s := cfg.Servers["prod"]
				s.URL = "https://moved.ex"
				cfg.Servers["prod"] = s
				require.NoError(t, cfg.Write())
				return "", nil
			}}
			_, err := c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://prod2.ex"}, Pos: []string{"prod"}}, nil)
			return err
		}},
		{"delete-server of an unknown server", func(t *testing.T, dir string) error {
			t.Helper()
			_, err := NewConfigDeleteServer(dir).(*configDeleteServerCmd).Execute(t.Context(), cmdtest.Input{Pos: []string{"typo"}}, ui.Discard())
			return err
		}},
		{"update-server of an unknown server", func(t *testing.T, dir string) error {
			t.Helper()
			_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://x.ex"}, Pos: []string{"typo"}}, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cmdtest.PreludeDir(t)
			cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
			require.Error(t, tc.run(t, dir))
			assert.FileExists(t, config.TransitionPath(dir), "the change is still there")
			assert.Empty(t, config.ReadPin(dir), "and none of it ran")
		})
	}

	t.Run("update-server of the CA alone", func(t *testing.T) {
		dir := cmdtest.PreludeDir(t)
		cmdtest.WriteDamagedChangeRecord(t, dir, true)
		_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"clear-cacert": true}, Pos: []string{"prod"}}, nil)
		require.NoError(t, err, "a change of trust alone touches no credential, so no change record stops it")
	})
}

// TestServerChangesWithNoChangeRecordTakeNoTransitionLock asserts what the
// prelude costs them on a directory with no change: one stat of the
// record's path and no lock beyond the ones they already took. delete-server
// loads the config a second time in its last span, which removes the entry
// after the revokes.
func TestServerChangesWithNoChangeRecordTakeNoTransitionLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir string) ([]string, error)
		want map[string]int
	}{
		{"delete-server", deleteServerRun, map[string]int{"configload": 2, "credentialsread": 2, "dirstat": 1, "manifeststat": 2}},
		{"update-server --url", updateServerURLRun, map[string]int{"configload": 2, "credentialsread": 2, "dirstat": 1, "manifeststat": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cmdtest.PreludeDir(t)
			c := cmdtest.CountOperations(t, func() {
				_, err := tc.run(t, dir)
				require.NoError(t, err)
			})
			assert.Zero(t, c.TransitionLocks)
			assert.Equal(t, tc.want, c.Ops)
			assert.Zero(t, c.Keychain)
		})
	}
}

// TestAURLChangeErasesAHelperStepUnderTheOldURL asserts a helper delete
// step left for the entry whose URL changes is erased under the URL the
// step recorded - the old one - by the prelude that runs before the change.
func TestAURLChangeErasesAHelperStepUnderTheOldURL(t *testing.T) {
	dir := cmdtest.PreludeDir(t)
	state := cmdtest.HelperHolding(t, "https://prod.ex", "prod", "prod-old")
	cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "delete", Entry: "prod", Source: "exec:fake",
		Token: "prod-old", CanonicalURL: "https://prod.ex", LastError: "the helper timed out"})

	_, err := updateServerURLRun(t, dir)
	require.NoError(t, err)
	assert.Contains(t, cmdtest.HelperLog(t, state), "erase https://prod.ex prod")
	assert.Empty(t, cmdtest.HelperCreds(t, state), "the old copy is gone")
	assert.NoFileExists(t, config.TransitionPath(dir))
}

// TestADeleteServerWhoseLastWriteFailsCarriesThePreludesNotices asserts the
// notices of the work the prelude did ride on the error of delete-server's
// last write, the one that removes the entry, beside the delete's own.
func TestADeleteServerWhoseLastWriteFailsCarriesThePreludesNotices(t *testing.T) {
	dir := cmdtest.PreludeDir(t)
	gone := config.ServerRef{Name: "gone", URL: "https://gone.ex"}
	require.NoError(t, config.NewFileStore(dir).SetToken(gone, "gone-new"))
	cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "delete", Entry: "gone", Source: "file", Token: "gone-old"})
	t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
		if step == "remove" {
			return errors.New("the config directory is full")
		}
		return nil
	}))
	_, err := deleteServerRun(t, dir)
	require.ErrorContains(t, err, "the config directory is full")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Contains(t, strings.Join(ce.Warnings(), "\n"), "was replaced since the change started; it was left alone")
}
