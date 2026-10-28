package cmdtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// PreludeDir is the directory every credential command's prelude rows
// start from: the file store declared, one entry, prod, holding the
// file token "prod-tok", and prod as the current server. The command
// under test acts on prod. Steps name "gone", an entry config.yaml no
// longer has - a server deleted while its change still had work - so
// nothing the command does to prod can be mistaken for the replay.
func PreludeDir(t *testing.T) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod",
		Servers: map[string]config.Server{"prod": {URL: "https://prod.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: "https://prod.ex"}, "prod-tok"))
	return dir
}

// UnreachableKeyring is a keychain no call reaches: a locked keychain, a
// headless machine with no D-Bus.
type UnreachableKeyring struct{}

var errKeychainUnreachable = errors.New("the keychain is not reachable")

func (UnreachableKeyring) Set(string, string, string) error   { return errKeychainUnreachable }
func (UnreachableKeyring) Get(string, string) (string, error) { return "", errKeychainUnreachable }
func (UnreachableKeyring) Delete(string, string) error        { return errKeychainUnreachable }

// RunPreludeRows drives one credential command through every state of the
// directory's change record, from PreludeDir: run executes the command on
// prod and returns its warnings. Each row asserts what the prelude did
// before the command's own work, and that the command's own work - prod's
// token gone - happened or, for a record that cannot be read, did not.
func RunPreludeRows(t *testing.T, run func(t *testing.T, dir string) ([]string, error)) {
	t.Helper()
	gone := config.ServerRef{Name: "gone", URL: "https://gone.ex"}
	fileToken := func(t *testing.T, dir string, ref config.ServerRef) string {
		t.Helper()
		tok, err := config.NewFileStore(dir).GetToken(ref)
		if errors.Is(err, config.ErrNoToken) {
			return ""
		}
		require.NoError(t, err)
		return tok
	}
	// prodAccount is the keychain account of prod's old copy, for the row
	// that writes one, and recordAtFirstWrite says whether prod's identity
	// record was still there when the command made its first write; the
	// row's setup sets them and its check reads them.
	var (
		prodAccount        string
		recordAtFirstWrite bool
	)
	for _, tc := range []struct {
		name string
		// setup writes the record and whatever its steps work on.
		setup func(t *testing.T, dir string)
		// refused is a record the prelude refuses on.
		refused bool
		// crash ends the command's first run right after the prelude
		// removes the record, before any write of its own; the command is
		// then run again.
		crash bool
		// keychain is a row whose step is in the keychain; every other
		// row makes no keychain call, so whether the keychain can be
		// reached does not change it.
		keychain bool
		// fails is a row the command stops on after the prelude did some
		// work: failed names the error's cause, and the work's notices
		// ride on the error.
		fails, notice string
		// check asserts what the prelude left.
		check func(t *testing.T, dir string, warnings []string)
	}{
		{name: "nothing recorded", check: func(t *testing.T, dir string, _ []string) {
			t.Helper()
			assert.NoFileExists(t, config.TransitionPath(dir))
		}},
		{name: "a delete step whose credential is still there is retried",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, config.NewFileStore(dir).SetToken(gone, "gone-old"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "file", Token: "gone-old"})
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.Empty(t, fileToken(t, dir, gone), "the old copy is removed")
				assert.NoFileExists(t, config.TransitionPath(dir), "the change is finished")
				assert.Empty(t, namedChange(t, dir), "and its name is forgotten by the command's own write")
			}},
		{name: "a delete step whose credential was replaced is dropped",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, config.NewFileStore(dir).SetToken(gone, "gone-new"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "file", Token: "gone-old"})
			},
			check: func(t *testing.T, dir string, warnings []string) {
				t.Helper()
				assert.Equal(t, "gone-new", fileToken(t, dir, gone), "a credential the step has no digest of is left alone")
				assert.NoFileExists(t, config.TransitionPath(dir))
				assert.Contains(t, strings.Join(warnings, "\n"), "was replaced since the change started; it was left alone")
			}},
		{name: "a keychain step the keychain cannot reach stays", keychain: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				t.Cleanup(config.SetKeyringBackendForTest(UnreachableKeyring{}))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "keychain", Token: "gone-old",
					SourceAccountID: "0123456789abcdef0123456789abcdef"})
			},
			check: func(t *testing.T, dir string, warnings []string) {
				t.Helper()
				assertStepStays(t, dir, warnings, errKeychainUnreachable.Error())
			}},
		{name: "a helper step the helper cannot erase stays",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				state := HelperHolding(t, "https://gone.ex", "gone", "gone-old")
				require.NoError(t, os.Chmod(state, 0o400))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "exec:fake", Token: "gone-old",
					CanonicalURL: "https://gone.ex"})
			},
			check: func(t *testing.T, dir string, warnings []string) {
				t.Helper()
				assertStepStays(t, dir, warnings, "could not finish for gone")
			}},
		{name: "a pin step that cannot be written stays and does not stop the command",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.MkdirAll(filepath.Join(config.PinPath(dir), "in-the-way"), 0o700))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "pin", Target: "file"})
			},
			check: func(t *testing.T, dir string, warnings []string) {
				t.Helper()
				assertStepStays(t, dir, warnings, "a credential store change is unfinished for this directory (pin)")
			}},
		{name: "the command ends right after the prelude finishes the change", crash: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, config.NewFileStore(dir).SetToken(gone, "gone-old"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "file", Token: "gone-old"})
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.Empty(t, fileToken(t, dir, gone))
				assert.Empty(t, namedChange(t, dir), "the run after the crash forgets the finished change")
			}},
		{name: "a keychain delete of the command's own entry is retried, and the command's own delete comes after", keychain: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				prodAccount = KeychainCopy(t, dir, "prod", "prod-old")
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "prod", Source: "keychain", Token: "prod-old",
					SourceAccountID: prodAccount})
				recordAtFirstWrite = true
				t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
					if step == "cachebinding" {
						_, err := os.Stat(identityRecordPath(dir, "prod"))
						recordAtFirstWrite = err == nil
					}
					return nil
				}))
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.NoFileExists(t, config.TransitionPath(dir), "the change is finished")
				assert.False(t, recordAtFirstWrite, "the replay retired prod's identity record before the command's first write")
				assert.NoFileExists(t, identityRecordPath(dir, "prod"), "and it is gone")
				_, err := keyring.Get("cli.rancher.io", prodAccount+":prod")
				assert.ErrorIs(t, err, keyring.ErrNotFound, "and so is the old copy")
			}},
		{name: "a pin step that moves readers is written, and the command's last write resumes the pauses it held",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				cfg.CredentialStore = "auto"
				cfg.Servers["other"] = config.Server{URL: "https://other.ex", CacheWritesPaused: config.PauseLocked}
				require.NoError(t, cfg.Write())
				require.NoError(t, config.WritePin(dir, "file"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "pin", Target: "keychain"})
				cfg, err = config.Load(config.FilePath(dir))
				require.NoError(t, err)
				require.Equal(t, []config.PausedEntry{{Entry: "other", Reason: config.PauseReasonPinStep}}, config.PausedEntries(dir, cfg),
					"the pin step holds the pause before the command")
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.Equal(t, "keychain", config.ReadPin(dir))
				assert.NoFileExists(t, config.TransitionPath(dir))
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				assert.Empty(t, config.PausedEntries(dir, cfg), "no step is left to hold a pause")
			}},
		{name: "a record the prelude cannot remove after its step stops the command",
			fails: "directory not empty", notice: "credential store re-pinned: keychain",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, config.WritePin(dir, "keychain"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "pin", Target: "file"})
				// Once the pin is written, the record's path becomes
				// something no removal can take away.
				t.Cleanup(config.SetAfterWriteHookForTest(func(what string) {
					if what != "credstore-pin" {
						return
					}
					path := config.TransitionPath(dir)
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.MkdirAll(filepath.Join(path, "in-the-way"), 0o700))
				}))
			}},
		{name: "the command's first write fails after the prelude ran",
			fails: "the config directory is full", notice: "was replaced since the change started; it was left alone",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, config.NewFileStore(dir).SetToken(gone, "gone-new"))
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "delete", Entry: "gone", Source: "file", Token: "gone-old"})
				t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
					if step == "cachebinding" {
						return errors.New("the config directory is full")
					}
					return nil
				}))
			}},
		{name: "a pin step is written and does not stop the command",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				WriteChangeRecord(t, dir, true, ChangeStep{Kind: "pin", Target: "file"})
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.Equal(t, "file", config.ReadPin(dir))
				assert.NoFileExists(t, config.TransitionPath(dir))
			}},
		{name: "a change that never started is removed",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				WriteChangeRecord(t, dir, false, ChangeStep{Kind: "pin", Target: "keychain"})
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.NoFileExists(t, config.TransitionPath(dir))
				assert.Empty(t, config.ReadPin(dir), "none of its work is done")
			}},
		{name: "a finished change's name is forgotten",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				NameChangeRecord(t, dir, "0123456789abcdef0123456789abcdef")
			},
			check: func(t *testing.T, dir string, _ []string) {
				t.Helper()
				assert.Empty(t, namedChange(t, dir))
			}},
		{name: "a named record that cannot be read refuses", refused: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				WriteDamagedChangeRecord(t, dir, true)
			}},
		{name: "a record nothing names that cannot be read refuses", refused: true,
			setup: func(t *testing.T, dir string) {
				t.Helper()
				WriteDamagedChangeRecord(t, dir, false)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := PreludeDir(t)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			before, err := os.ReadFile(config.FilePath(dir))
			require.NoError(t, err)

			if tc.crash {
				crashAfterThePrelude(t, dir, run)
				assert.Empty(t, fileToken(t, dir, gone), "the prelude's work is done")
				assert.NoFileExists(t, config.TransitionPath(dir))
				after, rerr := os.ReadFile(config.FilePath(dir))
				require.NoError(t, rerr)
				assert.Equal(t, string(before), string(after), "and nothing of the command's own was written")
				assert.NotEmpty(t, namedChange(t, dir), "so config.yaml still names the finished change")
			}
			var warnings []string
			counts := CountOperations(t, func() { warnings, err = run(t, dir) })
			if !tc.keychain {
				assert.Zero(t, counts.Keychain, "no keychain call; its reachability changes nothing")
			}
			if tc.fails != "" {
				require.ErrorContains(t, err, tc.fails)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Contains(t, strings.Join(ce.Warnings(), "\n"), tc.notice, "the prelude's work rides on the error")
				assert.Equal(t, "prod-tok", fileToken(t, dir, config.ServerRef{Name: "prod", URL: "https://prod.ex"}), "the command did none of its own work")
				return
			}
			if tc.refused {
				require.Error(t, err)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.Equal(t, clierr.CodeValidation, ce.Code)
				assert.Equal(t, clierr.Rancher+" config status", ce.Hint)
				require.ErrorContains(t, err, config.TransitionPath(dir))
				require.ErrorContains(t, err, "no credential was read or written")
				assert.Equal(t, "prod-tok", fileToken(t, dir, config.ServerRef{Name: "prod", URL: "https://prod.ex"}), "nothing is deleted")
				after, rerr := os.ReadFile(config.FilePath(dir))
				require.NoError(t, rerr)
				assert.Equal(t, string(before), string(after), "and config.yaml is untouched")
				assert.FileExists(t, config.TransitionPath(dir), "the record is left for the operator")
				return
			}
			require.NoError(t, err)
			assert.Empty(t, fileToken(t, dir, config.ServerRef{Name: "prod", URL: "https://prod.ex"}), "the command did its own work")
			tc.check(t, dir, warnings)
		})
	}
}

// KeychainCopy stores token for entry in the (mocked) keychain the way a
// keychain login does - an identity record and the slot it names - and
// returns the record's account id, which a delete step of that copy
// carries.
func KeychainCopy(t *testing.T, dir, entry, token string) string {
	t.Helper()
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, ks.SetToken(config.ServerRef{Name: entry, URL: "https://" + entry + ".ex"}, token))
	b, err := os.ReadFile(identityRecordPath(dir, entry))
	require.NoError(t, err)
	var rec struct {
		ID string `json:"id"`
	}
	require.NoError(t, yaml.Unmarshal(b, &rec))
	require.NotEmpty(t, rec.ID)
	return rec.ID
}

// identityRecordPath is where an entry's keychain identity record lives:
// credstore-id/, under the SHA-256 of the entry name.
func identityRecordPath(dir, entry string) string {
	sum := sha256.Sum256([]byte(entry))
	return filepath.Join(dir, "credstore-id", hex.EncodeToString(sum[:]))
}

// errCrash stands for the process ending: a write hook panics with it and
// the test recovers it, so nothing after that write runs.
var errCrash = errors.New("the process ended here")

// crashAfterThePrelude runs the command and ends it the moment the prelude
// removes the change record it finished.
func crashAfterThePrelude(t *testing.T, dir string, run func(t *testing.T, dir string) ([]string, error)) {
	t.Helper()
	restore := config.SetAfterWriteHookForTest(func(what string) {
		if what == "credential-transition.yaml removed" {
			panic(errCrash)
		}
	})
	defer restore()
	defer func() { assert.Equal(t, errCrash, recover()) }()
	_, _ = run(t, dir)
}

// assertStepStays asserts a step that could not finish: the record
// keeps it with its cause, the command names the report that shows it,
// and the change is still named.
func assertStepStays(t *testing.T, dir string, warnings []string, cause string) {
	t.Helper()
	require.FileExists(t, config.TransitionPath(dir))
	b, err := os.ReadFile(config.TransitionPath(dir))
	require.NoError(t, err)
	assert.Contains(t, string(b), "lastError:", "the step records why it stopped")
	joined := strings.Join(warnings, "\n")
	assert.Contains(t, joined, cause)
	assert.Contains(t, joined, "hint: "+clierr.Rancher+" config status")
	assert.NotEmpty(t, namedChange(t, dir), "the change is still in flight")
}

// HelperHolding puts the fake credential helper on PATH holding token for
// name at url, written straight into its state file so no helper record
// names it, and returns the state file's path.
func HelperHolding(t *testing.T, url, name, token string) string {
	t.Helper()
	t.Setenv("PATH", BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	state := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", state)
	b, err := json.Marshal(map[string]any{"creds": map[string]string{url + "\n" + name: token}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(state, b, 0o600))
	return state
}

// HelperLog is every call the fake helper recorded, as "verb url name".
func HelperLog(t *testing.T, statePath string) []string {
	t.Helper()
	b, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var st struct {
		Log []string `json:"log"`
	}
	require.NoError(t, json.Unmarshal(b, &st))
	return st.Log
}

// namedChange is the change id dir's config.yaml names.
func namedChange(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	return cfg.CredentialTransitionID
}
