package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// assertStoreUnavailable asserts err is CREDENTIAL_STORE_UNAVAILABLE as an
// agent reads it from the JSON envelope - the code, exit 14, retryable, no
// hint - and returns the message.
func assertStoreUnavailable(t *testing.T, err error) string {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "not a structured error: %v", err)
	b, merr := json.Marshal(ce)
	require.NoError(t, merr)
	var got struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Hint      string `json:"hint"`
		Retryable bool   `json:"retryable"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
	assert.Equal(t, 14, ce.ExitCode())
	assert.True(t, got.Retryable, "retryable once the store is usable again")
	assert.Empty(t, got.Hint, "the remedy needs the person at the keyboard, so no command is offered to run")
	assert.NotEmpty(t, got.Message)
	return got.Message
}

// securityExit is the error go-keyring returns on macOS when
// /usr/bin/security exits with status.
func securityExit(t *testing.T, status int) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell to produce an exit status")
	}
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", status)).Run()
	_, ok := errors.AsType[*exec.ExitError](err)
	require.True(t, ok, "got %v", err)
	return err
}

// withKeychainOS runs the rest of the test as if on goos.
func withKeychainOS(t *testing.T, goos string) {
	t.Helper()
	prev := keychainOS
	keychainOS = goos
	t.Cleanup(func() { keychainOS = prev })
}

// TestKeychainErrorClassifies asserts the one classification every
// keychain error site goes through: a miss and an oversized value keep
// the plain form, and every other failure is CREDENTIAL_STORE_UNAVAILABLE
// naming the operation, the entry, the cause the error shows, the remedy,
// and the caller's tail, with the backend's error as the cause.
func TestKeychainErrorClassifies(t *testing.T) {
	withKeychainOS(t, "darwin")
	locked := securityExit(t, 128)

	for _, plain := range []error{keyring.ErrNotFound, keyring.ErrSetDataTooBig} {
		err := keychainError("set", "prod", plain, " (tail)")
		_, ok := errors.AsType[*clierr.Error](err)
		assert.False(t, ok, "%v is not the store being unavailable", plain)
		require.EqualError(t, err, fmt.Sprintf("keyring set %q: %v (tail)", "prod", plain))
		require.ErrorIs(t, err, plain)
	}

	for op, verb := range map[string]string{"get": "read", "set": "store", "delete": "delete"} {
		err := keychainError(op, "it's", locked, " (tail)")
		msg := assertStoreUnavailable(t, err)
		assert.Equal(t,
			"the keychain could not "+verb+" the credential for 'it'\\''s': the keychain is locked or its prompt was cancelled (security exited with status 128); unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again (tail)",
			msg)
		assert.ErrorIs(t, err, locked, "the backend's own error is the cause")
	}
}

// TestKeychainCauseSaysOnlyWhatTheErrorShows asserts the cause text:
// status 128 from macOS's security names the two things it can mean,
// any other status is reported as the status alone, and an error that is
// not an exit status - or one on another operating system - is quoted as
// it is.
func TestKeychainCauseSaysOnlyWhatTheErrorShows(t *testing.T) {
	locked, other := securityExit(t, 128), securityExit(t, 36)
	plain := errors.New("dbus: no session bus")

	withKeychainOS(t, "darwin")
	assert.Equal(t, "the keychain is locked or its prompt was cancelled (security exited with status 128)", keychainCause(locked))
	assert.Equal(t, "security exited with status 36", keychainCause(other))
	assert.Equal(t, "dbus: no session bus", keychainCause(plain))
	assert.Equal(t, "the keychain is locked or its prompt was cancelled (security exited with status 128)",
		keychainCause(fmt.Errorf("wrapped: %w", locked)), "a wrapped exit status is still read")

	withKeychainOS(t, "linux")
	assert.Equal(t, locked.Error(), keychainCause(locked), "an exit status means nothing known on another keychain")
	assert.Equal(t, "dbus: no session bus", keychainCause(plain))
}

// TestKeychainRemedyNamesEachSystemsOwnStep asserts the remedy for each
// operating system's keychain: a command or step that exists there.
func TestKeychainRemedyNamesEachSystemsOwnStep(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  "unlock the keychain with security unlock-keychain, or approve the macOS prompt",
		"windows": "make Windows Credential Manager reachable for this user",
		"linux":   "unlock the Secret Service keyring, or run in a session with a D-Bus session bus",
		"freebsd": "unlock the Secret Service keyring, or run in a session with a D-Bus session bus",
	} {
		withKeychainOS(t, goos)
		assert.Equal(t, want, keychainRemedy(), goos)
	}
}

// TestHelperUnavailableNamesHelperOperationAndEntry asserts the helper
// form: the helper, the operation in plain words, the quoted entry, and
// the cause as given.
func TestHelperUnavailableNamesHelperOperationAndEntry(t *testing.T) {
	for verb, op := range map[string]string{"get": "read", "store": "store", "erase": "erase"} {
		err := helperUnavailable("rancher-credential-fake", verb, "prod", "fakehelper: deliberate failure")
		msg := assertStoreUnavailable(t, err)
		assert.Equal(t, "credential helper rancher-credential-fake could not "+op+" the credential for 'prod': fakehelper: deliberate failure", msg)
	}
}

// TestAsStoreUnavailable asserts the lookup a caller adding context uses:
// it finds the classification through wrapping and joining, and nothing
// for another code or a plain error.
func TestAsStoreUnavailable(t *testing.T) {
	ce := clierr.Retryable(clierr.CodeCredentialStoreUnavailable, "x")
	assert.Same(t, ce, asStoreUnavailable(fmt.Errorf("w: %w", ce)))
	assert.Same(t, ce, asStoreUnavailable(errors.Join(errors.New("other"), ce)))
	assert.Nil(t, asStoreUnavailable(clierr.New(clierr.CodeValidation, "x")))
	assert.Nil(t, asStoreUnavailable(errors.New("plain")))
}

// TestHelperFailuresAreStoreUnavailable asserts the helper exec paths: a
// helper that exits non-zero is CREDENTIAL_STORE_UNAVAILABLE carrying its
// own message, on every verb; one that answers with something malformed
// is not, because it did answer.
func TestHelperFailuresAreStoreUnavailable(t *testing.T) {
	useFakeHelper(t, "fail")
	st, err := OpenStore("exec:fake", t.TempDir())
	require.NoError(t, err)
	_, err = st.GetToken(execRef())
	assert.Equal(t, "credential helper rancher-credential-fake could not read the credential for 'prod': fakehelper: deliberate failure",
		assertStoreUnavailable(t, err))
	err = st.SetToken(execRef(), "tok")
	assert.Contains(t, assertStoreUnavailable(t, err), "could not store the credential for 'prod': fakehelper: deliberate failure")
	err = st.DeleteToken(execRef())
	assert.Contains(t, assertStoreUnavailable(t, err), "could not erase the credential for 'prod': fakehelper: deliberate failure")

	useFakeHelper(t, "garbage")
	_, err = st.GetToken(execRef())
	require.ErrorContains(t, err, "malformed response")
	assert.Nil(t, asStoreUnavailable(err), "a helper that answered is not unavailable")
}

// failingKeychain is a keyring seam whose selected calls fail with err
// and whose other calls go to go-keyring's in-memory mock.
type failingKeychain struct {
	err                          error
	failGet, failSet, failDelete bool
}

func (k failingKeychain) Set(service, user, password string) error {
	if k.failSet {
		return k.err
	}
	return keyring.Set(service, user, password)
}

func (k failingKeychain) Get(service, user string) (string, error) {
	if k.failGet {
		return "", k.err
	}
	return keyring.Get(service, user)
}

func (k failingKeychain) Delete(service, user string) error {
	if k.failDelete {
		return k.err
	}
	return keyring.Delete(service, user)
}

// TestKeychainSitesFailAsStoreUnavailable asserts each keychain site that
// turns a failure into an error, one row per site, with the failure macOS
// reports for a locked keychain whose prompt was cancelled: the code, the
// operation and the entry in the message, the directory's own tail, and
// that nothing was given up on the way - the identity record stays and no
// plaintext file appears.
func TestKeychainSitesFailAsStoreUnavailable(t *testing.T) {
	ref := ServerRef{Name: "prod", URL: "https://prod.ex"}
	seedOld := func(t *testing.T, dir string) {
		t.Helper()
		seedKeychainCredential(t, dir, "prod", "old")
	}
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, dir string)
		backend failingKeychain
		run     func(t *testing.T, dir string) error
		verb    string
		tail    string
	}{
		{name: "a stored-login read",
			seed:    seedOld,
			backend: failingKeychain{failGet: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				_, err := newPinnedKeyringStore(dir, true).GetToken(ref)
				return err
			},
			verb: "read", tail: readerKeychainHint()},
		{name: "a login's read of the credential it replaces",
			seed:    seedOld,
			backend: failingKeychain{failGet: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				return newPinnedKeyringStore(dir, true).SetToken(ref, "new")
			},
			verb: "read", tail: readerKeychainHint()},
		{name: "a login's settling of a login that did not finish",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				id, err := newKeychainAccountID()
				require.NoError(t, err)
				require.NoError(t, writeIdentityRecord(dir, identityRecord{
					Version: identityVersion, Entry: "prod", State: identityPending,
					ID: id, Fingerprint: credentialFingerprint(id, "half"),
				}))
			},
			backend: failingKeychain{failGet: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				return newPinnedKeyringStore(dir, true).SetToken(ref, "new")
			},
			verb: "read", tail: readerKeychainHint()},
		{name: "a login's write",
			backend: failingKeychain{failSet: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				return newPinnedKeyringStore(dir, true).SetToken(ref, "new")
			},
			verb: "store", tail: readerKeychainHint()},
		{name: "a logout's read on a directory that uses the keychain",
			seed:    seedOld,
			backend: failingKeychain{failGet: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				_, _, err := DeleteLoginToken(t.Context(), dir, ref, backendKeychain)
				return err
			},
			verb: "read"},
		{name: "a logout's delete on a directory that uses the keychain",
			seed:    seedOld,
			backend: failingKeychain{failDelete: true},
			run: func(t *testing.T, dir string) error {
				t.Helper()
				_, _, err := DeleteLoginToken(t.Context(), dir, ref, backendKeychain)
				return err
			},
			verb: "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Cleanup(keyring.MockInit)
			withKeychainOS(t, "darwin")
			dir := t.TempDir()
			if tc.seed != nil {
				tc.seed(t, dir)
			}
			before, err := readIdentityRecord(dir, "prod")
			require.NoError(t, err)
			b := tc.backend
			b.err = securityExit(t, 128)
			t.Cleanup(SetKeyringBackendForTest(b))

			err = tc.run(t, dir)
			msg := assertStoreUnavailable(t, err)
			assert.Equal(t, "the keychain could not "+tc.verb+" the credential for 'prod': the keychain is locked or its prompt was cancelled (security exited with status 128); unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again"+tc.tail,
				msg)
			require.ErrorIs(t, err, b.err)
			after, rerr := readIdentityRecord(dir, "prod")
			require.NoError(t, rerr)
			assert.Equal(t, before, after, "the identity record is as the command found it")
			assert.NoFileExists(t, CredentialsFilePath(dir), "no other store was written instead")
		})
	}
}

// TestKeychainLogoutOutsideTheDirectorysStoreStaysAWarning asserts the
// site that stays resilient: a directory that does not keep its
// credentials in the keychain, but has a leftover identity record, logs
// out of the store it does use and only warns about the keychain.
func TestKeychainLogoutOutsideTheDirectorysStoreStaysAWarning(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	seedKeychainCredential(t, dir, "prod", "old")
	locked := securityExit(t, 128)
	t.Cleanup(SetKeyringBackendForTest(failingKeychain{err: locked, failGet: true}))

	_, notices, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, backendFile)
	require.NoError(t, err)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "keychain unreachable, the credential identity record was kept")
	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.NotNil(t, rec, "the record stays for the next attempt")
}

// errNoKeychainSession is a probe failure that says nothing about a
// locked keychain: a machine with no keychain session.
var errNoKeychainSession = errors.New("no keychain session")

// probeAnswer is the keychain probe's answer for a table row that says
// only whether the keychain is usable.
func probeAnswer(usable bool) error {
	if usable {
		return nil
	}
	return errNoKeychainSession
}

// TestAutoFirstLoginProbe asserts what the keychain probe of an `auto`
// directory with nothing stored decides, through the probe seam and the
// real timeout around it. On macOS a locked keychain - security's status
// 128, or no answer within the probe's limit while its prompt waits -
// refuses with CREDENTIAL_STORE_UNAVAILABLE and names `auth login
// --credential-store file` only where the caller can run it. Any other
// failure, and both answers on another operating system, choose the file
// store with the plaintext notice; a probe that succeeds chooses the
// keychain.
func TestAutoFirstLoginProbe(t *testing.T) {
	locked := securityExit(t, 128)
	const remedy = "; unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again"
	const alt = ", or run rancher auth login 'prod' --credential-store file to keep its credential in a plaintext file on purpose"
	registered := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	for _, tc := range []struct {
		name  string
		goos  string
		cfg   *Config
		entry string
		probe func() error
		want  string // the refusal's message, or the store chosen
	}{
		{name: "a locked keychain, a registered entry", goos: "darwin", cfg: registered, entry: "prod",
			probe: func() error { return locked },
			want:  "the keychain could not store the credential for 'prod': the keychain is locked or its prompt was cancelled (security exited with status 128)" + remedy + alt},
		{name: "a locked keychain, an entry not registered yet", goos: "darwin", cfg: &Config{}, entry: "prod",
			probe: func() error { return locked },
			want:  "the keychain could not store the credential for 'prod': the keychain is locked or its prompt was cancelled (security exited with status 128)" + remedy},
		{name: "a locked keychain, the exec plugin's own entry", goos: "darwin",
			cfg:   &Config{Servers: map[string]Server{"exec-login-r.ex": {URL: "https://r.ex"}}},
			entry: "exec-login-r.ex", probe: func() error { return locked },
			want: "the keychain could not store the credential for 'exec-login-r.ex': the keychain is locked or its prompt was cancelled (security exited with status 128)" + remedy},
		{name: "a probe that times out on macOS", goos: "darwin", cfg: registered, entry: "prod",
			probe: func() error { select {} },
			want:  "the keychain could not store the credential for 'prod': the keychain did not answer within 50ms, as when its unlock prompt is not answered in time" + remedy + alt},
		{name: "a probe that times out elsewhere", goos: "linux", cfg: registered, entry: "prod",
			probe: func() error { select {} }, want: backendFile},
		{name: "another failure", goos: "darwin", cfg: registered, entry: "prod",
			probe: func() error { return securityExit(t, 36) }, want: backendFile},
		{name: "status 128 on another operating system", goos: "linux", cfg: registered, entry: "prod",
			probe: func() error { return locked }, want: backendFile},
		{name: "a usable keychain", goos: "darwin", cfg: registered, entry: "prod",
			probe: func() error { return nil }, want: backendKeychain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withKeychainOS(t, tc.goos)
			origProbe, origTimeout := keyringProbe, keyringProbeTimeout
			keyringProbe, keyringProbeTimeout = tc.probe, 50*time.Millisecond
			resetKeyringUsableCache()
			t.Cleanup(func() {
				keyringProbe, keyringProbeTimeout = origProbe, origTimeout
				resetKeyringUsableCache()
			})
			dir := t.TempDir()

			b, err := loginBackendsOf(dir, tc.cfg, tc.entry, "", "auto")
			if tc.want != backendFile && tc.want != backendKeychain {
				assert.Equal(t, tc.want, clierr.ExpandRemedies(assertStoreUnavailable(t, err), ""))
				assert.NoFileExists(t, PinPath(dir))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, b.CommitsTo)
			if tc.want == backendFile {
				assert.Contains(t, b.Notice, "keychain unavailable; credentials will be stored in plaintext")
			} else {
				assert.Empty(t, b.Notice)
			}
		})
	}
}

// TestUnpinnedDirectoryWithCredentialsOnALockedKeychain asserts the probe
// where it does not decide the store, on an unpinned `auto` directory
// that already holds credentials. One whose credentials are in the file
// store keeps it, as a file pin does. One whose credentials are in the
// keychain is refused as CREDENTIAL_STORE_UNAVAILABLE with the probe's
// cause, offering `--credential-store file` only when no other entry
// holds a keychain credential, because that store change is refused
// while one does.
func TestUnpinnedDirectoryWithCredentialsOnALockedKeychain(t *testing.T) {
	withKeychainOS(t, "darwin")
	locked := securityExit(t, 128)
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, dir string)
		refuse  bool
		withAlt bool
	}{
		{name: "another entry in the file store", seed: func(t *testing.T, dir string) {
			t.Helper()
			require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "other", URL: "https://o.ex"}, "other-tok"))
		}},
		{name: "another entry in the keychain", refuse: true, seed: func(t *testing.T, dir string) {
			t.Helper()
			seedKeychainCredential(t, dir, "other", "other-tok")
		}},
		{name: "only this entry in the keychain", refuse: true, withAlt: true, seed: func(t *testing.T, dir string) {
			t.Helper()
			seedKeychainCredential(t, dir, "prod", "prod-tok")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			t.Cleanup(keyring.MockInit)
			dir := t.TempDir()
			cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex"}, "other": {URL: "https://o.ex"}}}
			tc.seed(t, dir)
			origProbe := keyringProbe
			keyringProbe = func() error { return locked }
			resetKeyringUsableCache()
			t.Cleanup(func() { keyringProbe = origProbe; resetKeyringUsableCache() })

			b, err := loginBackendsOf(dir, cfg, "prod", "", "auto")
			if !tc.refuse {
				require.NoError(t, err)
				assert.Equal(t, backendFile, b.CommitsTo)
				return
			}
			msg := clierr.ExpandRemedies(assertStoreUnavailable(t, err), "")
			assert.Contains(t, msg, "the keychain cannot be used right now (the keychain is locked or its prompt was cancelled (security exited with status 128))")
			assert.Contains(t, msg, "unlock the keychain with security unlock-keychain, or approve the macOS prompt, then run this command again")
			if tc.withAlt {
				assert.Contains(t, msg, ", or run rancher auth login 'prod' --credential-store file")
			} else {
				assert.NotContains(t, msg, "--credential-store file")
			}
			assert.NoFileExists(t, PinPath(dir))
		})
	}
}

// TestAutoFilePinnedLoginStaysOnALockedKeychain asserts the other probe a
// login runs: on a directory already pinned to the file store, the probe
// only asks whether to upgrade, so a locked keychain leaves it on the file
// store rather than refusing a login that pins nothing new.
func TestAutoFilePinnedLoginStaysOnALockedKeychain(t *testing.T) {
	withKeychainOS(t, "darwin")
	locked := securityExit(t, 128)
	origProbe := keyringProbe
	keyringProbe = func() error { return locked }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringProbe = origProbe; resetKeyringUsableCache() })
	dir := t.TempDir()
	require.NoError(t, WritePin(dir, backendFile))

	b, err := loginBackendsOf(dir, &Config{}, "prod", "", "auto")
	require.NoError(t, err)
	assert.Equal(t, backendFile, b.CommitsTo)
}

// TestHelperDirectoryRefusalOffersTheFileStoreOnlyAlone asserts the
// refusal of an `auto` login on a directory that declares a credential
// helper while the keychain cannot be used: `--credential-store file` is
// offered only when no other entry exists, since leaving a helper is
// refused while one does.
func TestHelperDirectoryRefusalOffersTheFileStoreOnlyAlone(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	origUsable := keyringUsable
	keyringUsable = func() error { return errNoKeychainSession }
	resetKeyringUsableCache()
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })
	for _, tc := range []struct {
		name    string
		servers map[string]Server
		withAlt bool
	}{
		{name: "the only entry", servers: map[string]Server{"prod": {URL: "https://r.ex"}}, withAlt: true},
		{name: "beside another entry", servers: map[string]Server{"prod": {URL: "https://r.ex"}, "other": {URL: "https://o.ex"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{CredentialStore: "exec:vault", Servers: tc.servers}
			_, err := loginBackendsOf(t.TempDir(), cfg, "prod", "auto", "auto")
			msg := clierr.ExpandRemedies(assertStoreUnavailable(t, err), "")
			assert.Contains(t, msg, "never moves this directory's credentials out of exec:vault")
			if tc.withAlt {
				assert.Contains(t, msg, ", or run rancher auth login 'prod' --credential-store file")
			} else {
				assert.NotContains(t, msg, "--credential-store file")
			}
		})
	}
}
