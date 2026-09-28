package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// logoutRaceDir is a file-store directory whose entry prod, at url, holds
// alice's credential.
func logoutRaceDir(t *testing.T, url string) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: url}, "alice"))
	return dir
}

// TestALogoutRacingACacheWriterLeavesNoRecordValid asserts the window a
// logout's first write opens: a kubectl call that loads the config right
// after that write and reads the credential before the delete makes its
// token over the network while the logout deletes and purges, and writes
// its record after both. That record must never be served: prod holds no
// credential at all once the logout is done.
func TestALogoutRacingACacheWriterLeavesNoRecordValid(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, dir string)
	}{
		{"auth logout prod", func(t *testing.T, dir string) {
			t.Helper()
			runLogout(t, dir, "prod")
		}},
		{"auth logout --all", func(t *testing.T, dir string) {
			t.Helper()
			_, err := (&logoutCmd{dir: dir}).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testrancher.NewMadeFrom()
			t.Cleanup(m.Close)
			dir := logoutRaceDir(t, m.URL)

			race := cmdtest.NewRace(t)
			m.During(race.Hold)
			var started atomic.Bool
			restore := config.SetAfterWriteHookForTest(func(what string) {
				if what != "config.yaml" || started.Swap(true) {
					return
				}
				race.Start(t, dir, m.URL, "prod")
				race.WaitHeld(t)
			})
			tc.run(t, dir)
			restore()
			race.Release()
			require.Equal(t, testrancher.MadeFromToken("alice"), race.Wait(t), "the writer read alice's credential before the delete")

			tok, _ := cmdtest.Kubectl(t, dir, m.URL, "prod")
			assert.NotEqual(t, testrancher.MadeFromToken("alice"), tok,
				"a later kubectl call is never served alice's token once prod is logged out")
		})
	}
}

// TestALogoutPausesInItsFirstWriteAndResumesInItsLast asserts the logout's
// writes one at a time: its first write advances each target's id and
// pauses its cache writes while the credential is still there; a stop
// after it leaves the pause for the exec plugin's next miss; the last
// write - the logout stamps, or a write of the pauses alone when nothing
// was stamped - resumes them, and a failure of it is a notice; for
// `--all` that write comes before the cache wipe, so a failed wipe
// leaves no pause.
func TestALogoutPausesInItsFirstWriteAndResumesInItsLast(t *testing.T) {
	fail := func(want string) func(string) error {
		return func(step string) error {
			if step == want {
				return errors.New("config directory is full")
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name     string
		tokenRef bool
		all      bool
		hook     func(string) error
		setup    func(t *testing.T, dir string)
		steps    []string
		fails    bool
		paused   bool
		advanced bool
		notice   string
		noLock   bool
	}{
		{name: "nothing stamped", steps: []string{"cachebinding", "resume"}, advanced: true},
		{name: "without the lock, the logout clears the pause it made", noLock: true, steps: []string{"cachebinding", "resume"}, advanced: true},
		{name: "stamped", tokenRef: true, steps: []string{"cachebinding", "logout"}, advanced: true},
		{name: "the first write fails", hook: fail("cachebinding"), steps: []string{"cachebinding"}, fails: true},
		{name: "the delete fails", steps: []string{"cachebinding"}, fails: true, paused: true, advanced: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Remove(config.CredentialsFilePath(dir)))
				require.NoError(t, os.Mkdir(config.CredentialsFilePath(dir), 0o700))
			}},
		{name: "the stamp write fails", tokenRef: true, hook: fail("logout"), steps: []string{"cachebinding", "logout"},
			paused: true, advanced: true, notice: "could not record the logout on the server entry"},
		{name: "the write that resumes fails", hook: fail("resume"), steps: []string{"cachebinding", "resume"},
			paused: true, advanced: true, notice: "cache writes for prod stay paused (config directory is full)"},
		{name: "--all, and the cache wipe fails after the last write", all: true, steps: []string{"cachebinding", "resume"},
			fails: true, advanced: true, setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "token-cache.yaml"), []byte("\t: not yaml ["), 0o600))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := logoutRaceDir(t, "https://r.ex")
			pause := config.PauseLocked
			if tc.noLock {
				t.Setenv(config.EnvConfigNoLock, "1")
				pause = config.PauseNoLock
			}
			if tc.tokenRef {
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				e := cfg.Servers["prod"]
				e.Provenance, e.TokenRef = clierr.ProvenanceDerived, &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}
				cfg.Servers["prod"] = e
				require.NoError(t, cfg.Write())
			}
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			before, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			type seen struct{ pause, credential string }
			at := map[string]seen{}
			var steps []string
			t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
				steps = append(steps, step)
				cfg, lerr := config.Load(config.FilePath(dir))
				require.NoError(t, lerr)
				tok, _ := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "prod", URL: "https://r.ex"})
				at[step] = seen{cfg.Servers["prod"].CacheWritesPaused, tok}
				if tc.hook != nil {
					return tc.hook(step)
				}
				return nil
			}))
			in := cmdtest.Input{Pos: []string{"prod"}}
			if tc.all {
				in = cmdtest.Input{Flag: map[string]bool{"all": true}}
			}
			res, err := (&logoutCmd{dir: dir}).Execute(t.Context(), in, ui.Discard())
			if tc.fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.steps, steps)
			if tc.setup == nil {
				assert.Equal(t, "alice", at["cachebinding"].credential, "the first write lands while the credential is still there")
			}
			if len(tc.steps) > 1 {
				assert.Equal(t, pause, at[tc.steps[1]].pause, "paused until the last write")
			}
			after, lerr := config.Load(config.FilePath(dir))
			require.NoError(t, lerr)
			assert.Equal(t, tc.paused, after.Servers["prod"].Paused())
			assert.Equal(t, tc.advanced, after.Servers["prod"].CacheBindingID != before.Servers["prod"].CacheBindingID)
			if tc.notice != "" {
				assert.Contains(t, strings.Join(res.Warnings, "\n"), tc.notice)
			}
		})
	}
}
