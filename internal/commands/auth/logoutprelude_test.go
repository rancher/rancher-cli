package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// logoutRun runs auth logout on prod, or with --all.
func logoutRun(all bool) func(t *testing.T, dir string) ([]string, error) {
	return func(t *testing.T, dir string) ([]string, error) {
		t.Helper()
		in := cmdtest.Input{Pos: []string{"prod"}}
		if all {
			in = cmdtest.Input{Flag: map[string]bool{"all": true}}
		}
		res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), in, ui.Discard())
		return res.Warnings, err
	}
}

// TestLogoutFinishesTheStoreChangeFirst asserts both forms of auth logout
// run the prelude after their own validation and before their first
// write: a change's work is retried, dropped or kept as each step
// earns, a pin step never stops a logout, a change that never started is
// removed, a finished change's name is forgotten, and a record that
// cannot be read refuses the logout with nothing deleted and config.yaml
// untouched.
func TestLogoutFinishesTheStoreChangeFirst(t *testing.T) {
	t.Run("by name", func(t *testing.T) { cmdtest.RunPreludeRows(t, logoutRun(false)) })
	t.Run("--all", func(t *testing.T) { cmdtest.RunPreludeRows(t, logoutRun(true)) })
}

// TestLogoutValidatesBeforeThePrelude asserts a logout that fails its own
// validation replays nothing: an unknown server name, or a store variable
// that disagrees with the directory's declaration, leaves the change's
// work for a command that goes on.
func TestLogoutValidatesBeforeThePrelude(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		in   cmdtest.Input
	}{
		{name: "an unknown server", in: cmdtest.Input{Pos: []string{"typo"}}},
		{name: "a store variable against the declaration", env: "keychain", in: cmdtest.Input{Pos: []string{"prod"}}},
		{name: "--all, a store variable against the declaration", env: "keychain", in: cmdtest.Input{Flag: map[string]bool{"all": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cmdtest.PreludeDir(t)
			cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
			t.Setenv(config.EnvCredentialStore, tc.env)
			_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), tc.in, ui.Discard())
			require.Error(t, err)
			assert.FileExists(t, config.TransitionPath(dir), "the change is still there")
			assert.Empty(t, config.ReadPin(dir), "and none of it ran")
		})
	}
}

// TestLogoutWithNoChangeRecordTakesNoTransitionLock asserts what the
// prelude costs a logout on a directory with no change: one stat of the
// record's path and no lock beyond the ones the logout already took.
func TestLogoutWithNoChangeRecordTakesNoTransitionLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		all  bool
		want map[string]int
	}{
		{"by name", false, map[string]int{"configload": 1, "credentialsread": 2, "dirstat": 1, "manifeststat": 2}},
		{"--all", true, map[string]int{"configload": 1, "credentialsread": 2, "dirstat": 2, "manifeststat": 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := cmdtest.PreludeDir(t)
			c := cmdtest.CountOperations(t, func() {
				_, err := logoutRun(tc.all)(t, dir)
				require.NoError(t, err)
			})
			assert.Zero(t, c.TransitionLocks)
			assert.Equal(t, tc.want, c.Ops)
			assert.Zero(t, c.Keychain)
		})
	}
}
