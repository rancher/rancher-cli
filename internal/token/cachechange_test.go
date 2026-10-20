package token

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// The tests in this file drive the exec plugin's real miss path against
// the real credential changes, one window at a time: a cache writer that
// is part way through its own work when a change of the entry's
// credential happens, and a reader afterwards that must never be served
// a token made from a credential the entry no longer resolves.

// changeDir is a directory with one entry, prod, at url: declared as
// decl, pinned to pin when pin is not empty, and holding nothing yet.
func changeDir(t *testing.T, url, decl, pin string) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: decl, Servers: map[string]config.Server{"prod": {URL: url}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	if pin != "" {
		require.NoError(t, config.WritePin(dir, pin))
	}
	return dir
}

// loginAs is `rancher auth login prod` as user, through the login
// primitive, with the store the directory reads unless change says
// otherwise.
func loginAs(t *testing.T, dir, url, user string, change func(*config.PersistLoginInput)) (config.PersistLoginOutcome, error) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	in := config.PersistLoginInput{
		Name: "prod", CanonicalURL: url, Bearer: user, Provenance: clierr.ProvenanceDerived,
		ObservedDeclaration: cfg.CredentialStore,
		OpenStore:           config.OpenResolvedStore,
	}
	if change != nil {
		change(&in)
	}
	return config.PersistLoginCredential(t.Context(), dir, in)
}

// byName is a login that names store: `--credential-store store`.
func byName(store string) func(*config.PersistLoginInput) {
	return func(in *config.PersistLoginInput) {
		in.Policy = store
		in.Declaration = config.DeclarationFor(in.ObservedDeclaration, store, store)
	}
}

// autoLogin is a login whose policy is `auto`, as the directory declares.
func autoLogin(in *config.PersistLoginInput) {
	in.Policy = "auto"
	in.Declaration = config.DeclarationFor(in.ObservedDeclaration, "", "auto")
}

// kubectl is one exec-plugin process for prod against url, built the way
// the CLI builds it: the root hook's stamp read and config load, then
// resolveDeps' load and lazy store. between, when set, runs once after
// the cache probe fixed the store the process reads and before the load
// its record is bound to. inputs are further flag and value pairs.
func kubectl(t *testing.T, dir, url string, p runtime.Prompter, between func(), inputs ...string) (string, error) {
	t.Helper()
	anchor := config.NewCacheAnchor(dir)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, store, err := config.LoadAndStore(dir)
	require.NoError(t, err)
	cmd := tokenCmd{dir: dir, store: store, anchor: anchor.WithConfig(cfg)}
	cmd.newClient = func(o httpx.Options) (*http.Client, error) {
		if between != nil {
			b := between
			between = nil
			b()
		}
		return httpx.New(o)
	}
	in := map[string]string{"server": url, "name": "prod", "cluster": "local"}
	for i := 0; i+1 < len(inputs); i += 2 {
		in[inputs[i]] = inputs[i+1]
	}
	res, err := cmd.Execute(t.Context(), tin{s: in}, p)
	if err != nil {
		return "", err
	}
	return res.Data.(*execCredential).Status.Token, nil
}

// served is what a reader of prod's cache is handed now, or "" for a
// miss: the plugin's own cache probe, with an anchor of its own.
func served(t *testing.T, dir, url string) string {
	t.Helper()
	anchor := config.NewCacheAnchor(dir)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, store, err := config.LoadAndStore(dir)
	require.NoError(t, err)
	cmd := tokenCmd{dir: dir, store: store, anchor: anchor.WithConfig(cfg)}
	_, _, r, hit := cmd.probeFlagCache("prod", url, "local", "", "", ui.Discard())
	if !hit {
		return ""
	}
	return r.Data.(*execCredential).Status.Token
}

// swapFile replaces path with a directory, which no read parses and no
// atomic rename can replace, and returns the undo that puts the file
// back as it was.
func swapFile(t *testing.T, path string) (restore func()) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	return func() {
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
}

// identityRecordPaths lists the credential identity records the directory
// holds.
func identityRecordPaths(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "credstore-id", "*"))
	require.NoError(t, err)
	var out []string
	for _, p := range paths {
		if fi, serr := os.Stat(p); serr == nil && fi.Mode().IsRegular() && !strings.HasPrefix(filepath.Base(p), ".") {
			out = append(out, p)
		}
	}
	return out
}

// TestAWriterWhoseStoreWasFixedBeforeAChangeCachesNothingAfterIt asserts
// the store check across the store changes that leave an old copy behind
// in the store a writer already chose: the writer's cache probe fixes its
// store, the whole change lands, and only then does the writer load the
// config its record is bound to and read the credential - from the store
// readers have left, which still holds the previous login's copy.
func TestAWriterWhoseStoreWasFixedBeforeAChangeCachesNothingAfterIt(t *testing.T) {
	t.Run("a declared change whose old copy could not be read", func(t *testing.T) {
		m := newMadeFrom(t)
		dir := changeDir(t, m.URL, "keychain", "")
		_, err := loginAs(t, dir, m.URL, "alice", nil)
		require.NoError(t, err)

		var notices bytes.Buffer
		p := ui.New(&notices, strings.NewReader(""), true, false)
		tok, err := kubectl(t, dir, m.URL, p, func() {
			// The keychain copy cannot be read while the change is planned:
			// no step names it, and the change records nothing.
			records := identityRecordPaths(t, dir)
			require.Len(t, records, 1)
			data, rerr := os.ReadFile(records[0])
			require.NoError(t, rerr)
			require.NoError(t, os.WriteFile(records[0], []byte("not a record"), 0o600))
			out, lerr := loginAs(t, dir, m.URL, "bob", byName("file"))
			require.NoError(t, lerr)
			assert.Contains(t, strings.Join(out.Warnings, "\n"), "could not be read and was left where it is")
			require.NoError(t, os.WriteFile(records[0], data, 0o600))
		})
		require.NoError(t, err)
		assert.Equal(t, testrancher.MadeFromToken("alice"), tok, "the writer read the copy the change left in the store it had fixed")
		assert.Contains(t, notices.String(),
			"not caching: the credential store for prod changed during this call (it was read from keychain, and the directory now reads file)",
			"the notice says which two stores")

		assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
			"a reader of prod, which resolves bob in the file store, is never served alice's token")
	})

	t.Run("a pinned change whose old copy could not be read", func(t *testing.T) {
		m := newMadeFrom(t)
		dir := changeDir(t, m.URL, "auto", "file")
		_, err := loginAs(t, dir, m.URL, "alice", autoLoginToFile)
		require.NoError(t, err)

		tok, err := kubectl(t, dir, m.URL, ui.Discard(), func() {
			// credentials.yaml cannot be read while the change is planned:
			// the plan holds only the pin, which runs early instead.
			restore := swapFile(t, config.CredentialsFilePath(dir))
			out, lerr := loginAs(t, dir, m.URL, "bob", autoLogin)
			require.NoError(t, lerr)
			assert.Contains(t, strings.Join(out.Warnings, "\n"), "could not be read and was left where it is")
			restore()
			require.Equal(t, "keychain", config.ReadPin(dir))
		})
		require.NoError(t, err)
		assert.Equal(t, testrancher.MadeFromToken("alice"), tok, "the writer read the copy the change left in the store it had fixed")

		assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
			"a reader of prod, which resolves bob in the keychain, is never served alice's token")
	})
}

// autoLoginToFile is an `auto` login on a directory pinned to the file
// store with the keychain unusable, so the credential stays in the file
// store: the state an `auto` directory starts from before it upgrades.
func autoLoginToFile(in *config.PersistLoginInput) {
	in.Policy = "file"
	in.Declaration = ""
}

// TestTheFallbackAfterAStoreChangeCachesNothing asserts the store check
// on the interactive fallback: a writer whose cache probe fixed the
// keychain finds nothing there once a declared change to the file store
// has finished, signs in interactively, and would write that sign-in's
// token under prod's binding while prod resolves the credential the
// change stored.
func TestTheFallbackAfterAStoreChangeCachesNothing(t *testing.T) {
	f, srv := newTokenFacade(t, testrancher.Options{})
	dir := changeDir(t, srv.URL, "keychain", "")
	_, err := loginAs(t, dir, srv.URL, "alice", nil)
	require.NoError(t, err)

	var notices bytes.Buffer
	p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
	tok, err := kubectl(t, dir, srv.URL, p, func() {
		_, lerr := loginAs(t, dir, srv.URL, "bob", byName("file"))
		require.NoError(t, lerr)
	}, "auth-provider", "localProvider")
	require.NoError(t, err)
	require.Positive(t, f.LoginCount(), "the writer found nothing in the keychain and signed in")

	assert.NotEqual(t, tok, served(t, dir, srv.URL),
		"a reader of prod, which resolves bob in the file store, is never served the sign-in's token")
}

// TestALoginStoppedAfterItsCommitLeavesNoRecordOfThePreviousLoginValid
// asserts the window a crash between a login's commit and its last write
// leaves: every config write after the credential lands fails, as it does
// for a process killed there, and a record the previous login's cache
// writer made must not be served while prod resolves the new credential.
func TestALoginStoppedAfterItsCommitLeavesNoRecordOfThePreviousLoginValid(t *testing.T) {
	m := newMadeFrom(t)
	dir := changeDir(t, m.URL, "file", "")
	_, err := loginAs(t, dir, m.URL, "alice", nil)
	require.NoError(t, err)
	tok, err := kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	require.Equal(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL), "alice's record is cached")
	require.Equal(t, testrancher.MadeFromToken("alice"), tok)

	var committed bool
	t.Cleanup(config.SetAfterWriteHookForTest(func(what string) {
		if what == "credentials.yaml" {
			committed = true
		}
	}))
	t.Cleanup(config.SetPersistWriteHookForTest(func(string) error {
		if committed {
			return errors.New("the process is gone")
		}
		return nil
	}))
	_, _ = loginAs(t, dir, m.URL, "bob", nil)
	require.True(t, committed)
	got, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "prod", URL: m.URL})
	require.NoError(t, gerr)
	require.Equal(t, "bob", got, "bob's credential is what prod resolves")

	assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
		"alice's record is not served while prod resolves bob")
}

// compensationFails is a file store whose commit lands and whose
// compensation cannot undo it.
type compensationFails struct{ config.CredStore }

func (s compensationFails) SetTokenWithOutcome(ref config.ServerRef, tok string, prior config.PriorCredential) (config.CommitOutcome, error) {
	out, err := s.CredStore.SetTokenWithOutcome(ref, tok, prior)
	if err == nil {
		out.Compensate = func() error { return errors.New("the store refused the undo") }
	}
	return out, err
}

// TestALoginWhoseCompensationFailsLeavesNoRecordOfThePreviousLoginValid
// asserts the rollback's side of the id: a login whose trust write fails
// after its credential committed, and whose compensation cannot undo the
// commit, rolls its entry back with the new credential still stored. A
// rollback that put the entry's earlier id back would make the previous
// login's records valid again beside the new credential.
func TestALoginWhoseCompensationFailsLeavesNoRecordOfThePreviousLoginValid(t *testing.T) {
	m := newMadeFrom(t)
	dir := changeDir(t, m.URL, "file", "")
	_, err := loginAs(t, dir, m.URL, "alice", nil)
	require.NoError(t, err)
	_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	require.Equal(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL))

	t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
		if step == "trust" {
			return errors.New("config directory is full")
		}
		return nil
	}))
	out, err := loginAs(t, dir, m.URL, "bob", func(in *config.PersistLoginInput) {
		in.PendingTrust = &config.PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}
		in.OpenStore = func(string, string) (config.CredStore, string, error) {
			return compensationFails{config.NewFileStore(dir)}, "", nil
		}
	})
	require.Error(t, err)
	require.Equal(t, clierr.CredentialCompensationFailed, out.CredentialState)
	got, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "prod", URL: m.URL})
	require.NoError(t, gerr)
	require.Equal(t, "bob", got, "the compensation failed, so bob's credential stays")

	assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
		"alice's record is not served while prod resolves bob")
}

// TestAPinnedChangeFinishedByAnotherCommandLeavesNoRecordOfThePreviousLoginValid
// asserts the window a pinned store change leaves when neither its old
// copy nor its pin can be written: the login stops with both steps left,
// a cache writer runs while readers still resolve the old copy, and
// another command's prelude later finishes both steps, which moves
// readers to the new credential without writing config.yaml.
func TestAPinnedChangeFinishedByAnotherCommandLeavesNoRecordOfThePreviousLoginValid(t *testing.T) {
	m := newMadeFrom(t)
	dir := changeDir(t, m.URL, "auto", "file")
	_, err := loginAs(t, dir, m.URL, "alice", autoLoginToFile)
	require.NoError(t, err)
	_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	require.Equal(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL))

	var restores []func()
	t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
		if step == "transition" {
			restores = append(restores, swapFile(t, config.CredentialsFilePath(dir)), swapFile(t, config.PinPath(dir)))
		}
		return nil
	}))
	_, err = loginAs(t, dir, m.URL, "bob", autoLogin)
	require.ErrorContains(t, err, "neither the old copy nor the credential store pin could be changed")
	for _, r := range restores {
		r()
	}
	config.SetPersistWriteHookForTest(nil)

	// Cache writers while readers still resolve alice's copy: the first
	// finds the pause, tries the lock and finds the pin step left; the
	// second would be the first to write a record once a resume let it.
	for range 2 {
		_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
		require.NoError(t, err)
	}

	// Another command's prelude finishes the change.
	require.NoError(t, config.WithLock(t.Context(), dir, func() error {
		cfg, lerr := config.Load(config.FilePath(dir))
		if lerr != nil {
			return lerr
		}
		_, perr := config.PrepareMutation(t.Context(), dir, cfg, config.MutationIntent{})
		return perr
	}))
	require.Equal(t, "keychain", config.ReadPin(dir), "the replay moved readers to the keychain")
	require.NoFileExists(t, config.TransitionPath(dir))

	assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
		"alice's record is not served while prod resolves bob")
}

// TestAChangeRecordRemovedAfterItWasNamedLeavesNoRecordOfThePreviousLoginValid
// asserts the state a change record removed right after the write that
// named it leaves: the declaration already moved readers, none of the
// change's steps ran, so the old copy is still in the store they left.
func TestAChangeRecordRemovedAfterItWasNamedLeavesNoRecordOfThePreviousLoginValid(t *testing.T) {
	// lostChange is bob's declared change to the file store, with its
	// record removed by a process that skipped the flock, in the one
	// window where that is visible.
	lostChange := func(t *testing.T, dir, url string) {
		t.Helper()
		restore := config.SetPersistWriteHookForTest(func(step string) error {
			if step == "declaration" {
				return os.Remove(config.TransitionPath(dir))
			}
			return nil
		})
		defer restore()
		out, err := loginAs(t, dir, url, "bob", byName("file"))
		require.ErrorContains(t, err, "no longer the one this login wrote")
		require.Equal(t, clierr.ConfigTransitionLost, out.ConfigState)
	}

	t.Run("a record made before the change", func(t *testing.T) {
		m := newMadeFrom(t)
		dir := changeDir(t, m.URL, "keychain", "")
		_, err := loginAs(t, dir, m.URL, "alice", nil)
		require.NoError(t, err)
		_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
		require.NoError(t, err)
		require.Equal(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL))

		lostChange(t, dir, m.URL)

		assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
			"alice's record is not served while prod resolves bob in the file store")
	})

	t.Run("a writer that fixed its store before the change", func(t *testing.T) {
		m := newMadeFrom(t)
		dir := changeDir(t, m.URL, "keychain", "")
		_, err := loginAs(t, dir, m.URL, "alice", nil)
		require.NoError(t, err)

		tok, err := kubectl(t, dir, m.URL, ui.Discard(), func() { lostChange(t, dir, m.URL) })
		require.NoError(t, err)
		assert.Equal(t, testrancher.MadeFromToken("alice"), tok, "the writer read the copy the change left in the keychain")

		assert.NotEqual(t, testrancher.MadeFromToken("alice"), served(t, dir, m.URL),
			"alice's record is not served while prod resolves bob in the file store")
	})
}

// TestAFirstLoginWhosePinWriteFailsStillCaches asserts that a first login
// on an `auto` directory with nothing stored and no pin, whose pin step
// fails, leaves cache writes working with no later command: its pin would
// only name the store readers already resolve, the keychain its identity
// record shows, so it moves nobody. A directory only the exec plugin
// uses runs no later login to finish it.
func TestAFirstLoginWhosePinWriteFailsStillCaches(t *testing.T) {
	m := newMadeFrom(t)
	dir := changeDir(t, m.URL, "", "")
	require.NoError(t, os.Mkdir(config.PinPath(dir), 0o700), "no pin can be written")

	out, err := loginAs(t, dir, m.URL, "alice", autoLogin)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(out.Warnings, "\n"), "a credential store change is unfinished for this directory (pin)",
		"the pin step that could not run says so")
	require.FileExists(t, config.TransitionPath(dir), "and stays for a later login")

	_, err = kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	before := m.Creates()
	tok, err := kubectl(t, dir, m.URL, ui.Discard(), nil)
	require.NoError(t, err)
	assert.Equal(t, testrancher.MadeFromToken("alice"), tok)
	assert.Equal(t, before, m.Creates(), "the second kubectl call is served from the cache")
}
