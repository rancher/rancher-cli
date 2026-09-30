package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prodOnDisk is prod as config.yaml holds it now.
func prodOnDisk(t *testing.T, dir string) Server {
	t.Helper()
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	return cfg.Servers["prod"]
}

// TestALoginPausesInItsFirstWriteAndResumesInItsLast asserts where the
// login's pause sits among its writes: the first write advances the id
// and pauses before any of the entry's credential is written, every write
// between keeps the pause, and only the last clears it - for a login that
// changes no store and for one that changes it.
func TestALoginPausesInItsFirstWriteAndResumesInItsLast(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dir    func(t *testing.T) string
		in     PersistLoginInput
		steps  []string
		noLock bool
	}{
		{name: "a login that changes no store", dir: settledFixture, in: settledInput(), steps: []string{"entry", "resume"}},
		{name: "the auto repin", dir: repinFixture, in: repinInput(), steps: []string{"entry", "transition", "resume"}},
		{name: "a login made without the lock", dir: settledFixture, in: settledInput(), steps: []string{"entry", "resume"}, noLock: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir(t)
			pause := PauseLocked
			if tc.noLock {
				t.Setenv(EnvConfigNoLock, "1")
				pause = PauseNoLock
			}
			before := prodOnDisk(t, dir)
			type seen struct {
				id, pause, credential string
			}
			var steps []string
			at := map[string]seen{}
			committed := ""
			defer SetAfterWriteHookForTest(func(what string) {
				if committed == "" && (what == "credentials.yaml" || what == "keychain set") {
					committed = "after " + steps[len(steps)-1]
				}
			})()
			restore := SetPersistWriteHookForTest(func(step string) error {
				steps = append(steps, step)
				p := prodOnDisk(t, dir)
				at[step] = seen{p.CacheBindingID, p.CacheWritesPaused, committed}
				return nil
			})
			defer restore()

			out, err := PersistLoginCredential(t.Context(), dir, tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.steps, steps)
			assert.Equal(t, seen{before.CacheBindingID, "", ""}, at["entry"], "nothing is committed or paused before the first write")
			assert.Equal(t, "after entry", at[tc.steps[1]].credential, "the credential lands after the first write")
			for _, step := range tc.steps[1:] {
				assert.Equal(t, pause, at[step].pause, "paused through the %s write", step)
				assert.NotEqual(t, before.CacheBindingID, at[step].id, "under the advanced id")
			}
			after := prodOnDisk(t, dir)
			assert.False(t, after.Paused(), "the last write resumes, a pause made without the lock by its own change included")
			assert.Equal(t, at["resume"].id, after.CacheBindingID, "and moves no id")
			assert.False(t, out.CacheWritesPaused)
		})
	}
}

// TestALoginLeavesThePauseEachWayItCanEnd asserts, for each way a login can
// end, whether the entry's cache writes are left paused and who clears
// them: nothing stored leaves nothing paused; a stop after the first
// write leaves the pause for the exec plugin's next miss; a rollback, the
// warn-and-succeed store, the logout handle and a finished change record
// resume in their own write; a change record that cannot be read keeps
// it; a pin step left that would move readers keeps it for the next
// login, and one readers already resolve does not.
func TestALoginLeavesThePauseEachWayItCanEnd(t *testing.T) {
	failWrite := func(want string) func(string) error {
		return func(step string) error {
			if step == want {
				return errors.New("config directory is full")
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name   string
		dir    func(t *testing.T) string
		in     func(dir string) PersistLoginInput
		hook   func(dir string) func(string) error
		fails  bool
		paused bool
		// advanced is false only where nothing was written.
		advanced bool
		// warning is a warning the login gives, when the row names one.
		warning string
	}{
		{name: "the first write fails", dir: settledFixture, in: func(string) PersistLoginInput { return settledInput() },
			hook: func(string) func(string) error { return failWrite("entry") }, fails: true},
		{name: "the commit fails and the rollback resumes", dir: settledFixture,
			in: func(dir string) PersistLoginInput {
				in := settledInput()
				in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
				return in
			}, fails: true, advanced: true},
		{name: "the rollback write fails", dir: settledFixture,
			in: func(dir string) PersistLoginInput {
				in := settledInput()
				in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
				return in
			}, hook: func(string) func(string) error { return failWrite("rollback") }, fails: true, paused: true, advanced: true},
		{name: "the write that names the change fails", dir: repinFixture, in: func(string) PersistLoginInput { return repinInput() },
			hook: func(string) func(string) error { return failWrite("transition") }, fails: true, paused: true, advanced: true},
		{name: "the last write fails", dir: settledFixture, in: func(string) PersistLoginInput { return settledInput() },
			hook: func(string) func(string) error { return failWrite("resume") }, paused: true, advanced: true,
			warning: "cache writes for prod stay paused (config directory is full); the next exec-credential cache miss or credential change resumes them"},
		{name: "the change record is removed once it is named", dir: repinFixture, in: func(string) PersistLoginInput { return repinInput() },
			hook: func(dir string) func(string) error {
				return func(step string) error {
					if step == "transition" {
						return os.Remove(TransitionPath(dir))
					}
					return nil
				}
			}, fails: true, advanced: true},
		{name: "the change record is replaced once it is named", dir: repinFixture, in: func(string) PersistLoginInput { return repinInput() },
			hook: func(dir string) func(string) error {
				return func(step string) error {
					if step == "transition" {
						return os.WriteFile(TransitionPath(dir), []byte("not: [yaml"), 0o600)
					}
					return nil
				}
			}, fails: true, paused: true, advanced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir(t)
			before := prodOnDisk(t, dir)
			if tc.hook != nil {
				t.Cleanup(SetPersistWriteHookForTest(tc.hook(dir)))
			}
			out, err := PersistLoginCredential(t.Context(), dir, tc.in(dir))
			if tc.fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.paused, out.CacheWritesPaused)
			}
			after := prodOnDisk(t, dir)
			assert.Equal(t, tc.paused, after.Paused())
			assert.Equal(t, tc.advanced, after.CacheBindingID != before.CacheBindingID, "the id moved only if the first write landed")
			if tc.warning != "" {
				assert.Contains(t, joinWarnings(out.Warnings), tc.warning)
			}
		})
	}

	t.Run("a pin step left that would move readers keeps the pause", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("an unprivileged runner cannot create a symlink")
		}
		dir := repinFixture(t)
		defer SetAfterWriteHookForTest(func(what string) {
			if what == "credentials.yaml" {
				// Readers still read the file pin through the link; no
				// write replaces a symlink, so the pin step cannot run.
				target := filepath.Join(t.TempDir(), "pin")
				require.NoError(t, os.WriteFile(target, []byte(backendFile+"\n"), 0o600))
				require.NoError(t, os.Remove(PinPath(dir)))
				require.NoError(t, os.Symlink(target, PinPath(dir)))
			}
		})()
		out, err := PersistLoginCredential(t.Context(), dir, repinInput())
		require.NoError(t, err)
		assert.True(t, out.CacheWritesPaused)
		assert.Equal(t, PauseLocked, prodOnDisk(t, dir).CacheWritesPaused, "until a later login writes the pin")
	})

	t.Run("a pin step left that readers already resolve does not", func(t *testing.T) {
		dir := settledFixture(t)
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		cfg.CredentialStore = ""
		require.NoError(t, cfg.Write())
		require.NoError(t, os.Remove(CredentialsFilePath(dir)))
		orig := keyringUsable
		keyringUsable = func() bool { return true }
		resetKeyringUsableCache()
		t.Cleanup(func() { keyringUsable = orig; resetKeyringUsableCache() })
		require.NoError(t, os.Mkdir(PinPath(dir), 0o700), "no pin can be written")

		in := settledInput()
		in.Policy, in.ObservedDeclaration, in.Declaration = "auto", "", DeclarationFor("", "", "auto")
		out, err := PersistLoginCredential(t.Context(), dir, in)
		require.NoError(t, err)
		require.FileExists(t, TransitionPath(dir), "the pin step is left")
		assert.False(t, out.CacheWritesPaused, "readers already resolve the keychain its identity record shows")
		assert.False(t, prodOnDisk(t, dir).Paused())
	})

	t.Run("the warn-and-succeed store resumes in its write", func(t *testing.T) {
		dir, _ := persistFixture(t, derivedEntry())
		st := failStore{CredStore: NewFileStore(dir),
			out: CommitOutcome{Backend: clierr.BackendExec, State: CommitNotWritten, PriorSource: clierr.PriorUnknown},
			err: errors.Join(ErrTokenStoreUnsupported, errors.New("helper: read-only"))}
		_, err := PersistLoginCredential(t.Context(), dir, loginInput(dir, st))
		require.ErrorIs(t, err, ErrTokenStoreUnsupported)
		after := prodOnDisk(t, dir)
		assert.False(t, after.Paused())
		assert.NotEmpty(t, after.CacheBindingID, "under the id the first write gave it")
	})

	t.Run("without the lock, a rollback clears the pause it made", func(t *testing.T) {
		dir := settledFixture(t)
		t.Setenv(EnvConfigNoLock, "1")
		var paused string
		t.Cleanup(SetPersistWriteHookForTest(func(step string) error {
			if step == "rollback" {
				paused = prodOnDisk(t, dir).CacheWritesPaused
			}
			return nil
		}))
		in := settledInput()
		in.OpenStore = fileOpen(failStore{out: CommitOutcome{State: clierr.CredentialNotWritten}, err: errors.New("backend refused")})
		_, err := PersistLoginCredential(t.Context(), dir, in)
		require.Error(t, err)
		assert.Equal(t, PauseNoLock, paused, "the first write marked it as made without the lock")
		assert.False(t, prodOnDisk(t, dir).Paused(), "and the rollback, its own change's last write, clears it")
	})

	t.Run("without the lock, the warn-and-succeed store clears the pause it made", func(t *testing.T) {
		dir, _ := persistFixture(t, derivedEntry())
		t.Setenv(EnvConfigNoLock, "1")
		st := failStore{CredStore: NewFileStore(dir),
			out: CommitOutcome{Backend: clierr.BackendExec, State: CommitNotWritten, PriorSource: clierr.PriorUnknown},
			err: errors.Join(ErrTokenStoreUnsupported, errors.New("helper: read-only"))}
		_, err := PersistLoginCredential(t.Context(), dir, loginInput(dir, st))
		require.ErrorIs(t, err, ErrTokenStoreUnsupported)
		assert.False(t, prodOnDisk(t, dir).Paused())
	})

	t.Run("a credential that landed without its identity settled stays paused", func(t *testing.T) {
		dir := repinFixture(t)
		writes := 0
		t.Cleanup(SetIdentityWriteHookForTest(func() error {
			writes++
			if writes >= 2 {
				return errors.New("read-only file system")
			}
			return nil
		}))
		_, err := PersistLoginCredential(t.Context(), dir, repinInput())
		require.ErrorContains(t, err, "recording the credential identity")
		assert.Equal(t, PauseLocked, prodOnDisk(t, dir).CacheWritesPaused, "for the exec plugin's next miss to resume")
	})
}

// TestALoginThatCreatesItsEntryStartsItWithAnIDOfItsOwn asserts that no
// entry a login creates carries the empty id: a record an earlier entry of
// the same name left under that id is a miss for it, and so is a record
// made while the directory had no entry for the server.
func TestALoginThatCreatesItsEntryStartsItWithAnIDOfItsOwn(t *testing.T) {
	dir, _ := persistFixture(t, nil)
	canon := CanonicalDir(dir)
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	store := NewFileStore(dir)
	expiry := time.Now().Add(time.Hour)
	// An earlier entry of the same name, with no id, left this record.
	earlier := &Config{TokenReuse: cfg.TokenReuse, Servers: map[string]Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin},
	}}
	require.NoError(t, store.SetCachedCred("left", "earlier-entry", expiry, "exec-login-r.ex", "https://r.ex", "c", CacheBindingFor(canon, earlier, "exec-login-r.ex")))
	require.NoError(t, store.SetCachedCred("bare", "no-entry", expiry, "n", "https://r.ex", "c", CacheBindingForServer(canon, cfg, "https://r.ex", false)))
	_, _, err = store.GetCachedCred("bare", testAnchor(t, dir))
	require.NoError(t, err, "while the directory has no entry for the server, the record is its own")

	st := NewFileStore(dir)
	_, err = PersistLoginCredential(t.Context(), dir, pluginInput(dir, "exec-login-r.ex", st))
	require.NoError(t, err)
	after, err := Load(FilePath(dir))
	require.NoError(t, err)
	created := after.Servers["exec-login-r.ex"]
	assert.Len(t, created.CacheBindingID, randomIDLen)
	assert.False(t, created.Paused())
	for _, key := range []string{"left", "bare"} {
		_, _, gerr := store.GetCachedCred(key, testAnchor(t, dir))
		require.ErrorIs(t, gerr, ErrNoToken, "%s is a miss for the entry the login created", key)
	}
}
