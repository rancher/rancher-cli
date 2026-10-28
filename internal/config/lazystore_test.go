package config

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestLazyStoreResolvesBackendExactlyOnce asserts the sync.Once contract: many
// calls across many goroutines resolve the backend a single time, and every
// call observes the same backend.
func TestLazyStoreResolvesBackendExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod"}, "tok"))

	var calls int
	l := &lazyStore{resolve: func() (CredStore, error) {
		calls++
		return fs, nil
	}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			tok, err := l.GetToken(ServerRef{Name: "prod"})
			assert.NoError(t, err) //nolint:testifylint // runs inside wg.Go; require's FailNow must only run on the test's own goroutine
			assert.Equal(t, "tok", tok)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, calls)
}

// TestLazyStoreDrainNoticesRacesFirstResolution races DrainNotices against
// the credential call that triggers first resolution: the "resolved"
// atomic.Bool exists specifically so DrainNotices can read the resolved
// backend without forcing resolution itself and without racing the write of
// l.store/l.err inside once.Do. Run under -race (make validate always does)
// to catch a torn read; the many iterations also give DrainNotices a real
// chance to land on both sides of the resolution window, not just win a
// scheduling fluke once. Whichever side it lands on, the result must be
// exactly one of two valid states - the pre-resolution empty read, or the
// fully-resolved backend's own notice - never anything else (a partial
// slice, a length mismatch, or the backend's notice with a resolution that
// somehow never finished).
func TestLazyStoreDrainNoticesRacesFirstResolution(t *testing.T) {
	for i := range 200 {
		dir := t.TempDir()
		backendNotices := &noticeBuffer{}
		backendNotices.record("backend notice")
		fs := newFileStore(dir, backendNotices)

		l := &lazyStore{resolve: func() (CredStore, error) {
			return fs, nil
		}}

		var wg sync.WaitGroup
		var drained []string
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = l.GetToken(ServerRef{Name: "x"}) // triggers first resolution
		}()
		go func() {
			defer wg.Done()
			drained = l.DrainNotices()
		}()
		wg.Wait()

		switch len(drained) {
		case 0:
			// DrainNotices ran (or the resolved check landed) before
			// resolution completed - the valid pre-resolution empty read.
		case 1:
			assert.Equal(t, []string{"backend notice"}, drained,
				"iteration %d: a non-empty drain must be exactly the fully-resolved backend's own notice", i)
		default:
			t.Fatalf("iteration %d: DrainNotices returned an impossible torn read: %v", i, drained)
		}
	}
}

// TestLazyStoreDeferredErrorIsStable: a failed resolution surfaces the SAME
// raw error from the first and every later call (returned as-is;
// callers add their own context).
func TestLazyStoreDeferredErrorIsStable(t *testing.T) {
	boom := errors.New("writing credential store pin: disk full")
	l := &lazyStore{resolve: func() (CredStore, error) { return nil, boom }}

	_, err1 := l.GetToken(ServerRef{Name: "prod"})
	err2 := l.SetToken(ServerRef{Name: "prod"}, "tok")
	require.ErrorIs(t, err1, boom)
	require.ErrorIs(t, err2, boom)
}

// TestNewLazyStoreValidatesPolicyEagerly: an unknown effective policy fails
// at CONSTRUCTION naming its source, and no filesystem state appears. A
// command that takes no secrets sees the value; one that takes secrets never
// does.
func TestNewLazyStoreValidatesPolicyEagerly(t *testing.T) {
	t.Setenv(EnvCredentialStore, "wat")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore(&Config{}, dir, false)
	require.Error(t, err)
	require.ErrorContains(t, err, `RANCHER_CLI_CREDENTIAL_STORE: unknown credential store "wat" (valid: keychain, file, auto, exec:<name>)`)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr))

	_, err = NewLazyStore(&Config{}, dir, true)
	require.Error(t, err)
	require.ErrorContains(t, err, "RANCHER_CLI_CREDENTIAL_STORE: unknown credential store (valid: keychain, file, auto, exec:<name>)")
	assert.NotContains(t, err.Error(), "wat")
	_, statErr = os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr))
}

// TestNewLazyStoreNamesTheSourceOfAnInvalidValue asserts that the refusal
// names the variable or config.yaml, and keeps the value out of the text of a
// command that takes secrets, for a name and a helper name alike.
func TestNewLazyStoreNamesTheSourceOfAnInvalidValue(t *testing.T) {
	for _, tc := range []struct {
		name, variable, declared, source string
	}{
		{"the variable", "wat", "", "RANCHER_CLI_CREDENTIAL_STORE"},
		{"config.yaml", "", "wat", "config.yaml credentialStore"},
		{"a helper name in the variable", "exec:Not_Valid", "", "RANCHER_CLI_CREDENTIAL_STORE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, tc.variable)
			for _, takesSecrets := range []bool{false, true} {
				_, err := NewLazyStore(&Config{CredentialStore: tc.declared}, t.TempDir(), takesSecrets)
				require.ErrorContains(t, err, tc.source+": ")
				value := cmp.Or(tc.variable, tc.declared)
				if takesSecrets {
					assert.NotContains(t, err.Error(), "wat")
					assert.NotContains(t, err.Error(), "Not_Valid")
				} else {
					require.ErrorContains(t, err, value)
				}
			}
		})
	}
}

// TestNewLazyStoreCapturesEffectivePolicy: the policy resolved
// at construction is the policy opened, even if env changes before first
// use.
func TestNewLazyStoreCapturesEffectivePolicy(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()
	l, err := NewLazyStore(&Config{}, dir, false)
	require.NoError(t, err)

	// Env flips to an INVALID policy after construction; the captured
	// "file" policy must still be what resolves.
	t.Setenv(EnvCredentialStore, "wat")
	require.NoError(t, l.SetToken(ServerRef{Name: "prod"}, "tok"))
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}

// TestNewLazyStoreNoSideEffectsBeforeFirstUse asserts that constructing
// the store for a fresh dir creates NOTHING until a credential call happens.
func TestNewLazyStoreNoSideEffectsBeforeFirstUse(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore(&Config{}, dir, false)
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr), "construction must not create the config dir or pin")
}

// TestLoginSequenceFreshAutoUsableKeychain drives a direct-call sequence
// (NewLazyStore → the login's own backend resolution → SetToken) on a
// fresh auto profile with a
// usable keychain: exactly one probe, the credential in the keychain, no
// plaintext file - and NO pin, because a login writes its pin as the last
// step of the change it records, after the credential commits. The lazy
// store never resolves: the login opens the backend it resolved.
func TestLoginSequenceFreshAutoUsableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() error { probes++; return nil }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	b, err := loginBackendsOf(dir, &Config{}, "prod", "", "auto")
	require.NoError(t, err)
	assert.Equal(t, backendKeychain, b.CommitsTo)
	assert.True(t, b.WritesPin(), "a directory that has never pinned ends the change with one")
	store, notice, err := OpenResolvedStore(b.CommitsTo, dir)
	require.NoError(t, err)
	assert.Empty(t, notice)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	assert.Equal(t, 1, probes)
	assert.NoFileExists(t, PinPath(dir), "the pin is the change's last step, not the store open")
	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	require.NotNil(t, rec, "a keychain write records the entry's identity")
	tok, err := keyring.Get(keyringService, rec.Account())
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	_, bare := keyring.Get(keyringService, "prod")
	require.Error(t, bare, "nothing is stored under the bare entry name")
	_, statErr := os.Stat(filepath.Join(dir, "credentials.yaml"))
	assert.True(t, os.IsNotExist(statErr), "usable keychain must not produce a plaintext file")

	out := replayPin(dir, transitionStep{Kind: transitionPin, Target: b.CommitsTo})
	assert.Equal(t, "keychain", ReadPin(dir))
	assert.Equal(t, []string{NoticeFirstPinKeychain}, out.notices, "the pin step is what says the machine decided")
}

// TestLoginSequenceFreshAutoUnusableKeychain: the same sequence with an
// unusable keychain. The backend resolves to the file store with the
// plaintext disclosure, the credential lands in credentials.yaml, and the
// pin is still the change's last step. The single probe count proves
// keyringUsableOnce's memo is shared with the store open.
func TestLoginSequenceFreshAutoUnusableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() error { probes++; return errNoKeychainSession }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	b, err := loginBackendsOf(dir, &Config{}, "prod", "", "auto")
	require.NoError(t, err)
	assert.Equal(t, backendFile, b.CommitsTo)
	assert.Contains(t, b.Notice, "keychain unavailable; credentials will be stored in plaintext")
	assert.NotContains(t, b.Notice, "pinned credential store: keychain")
	store, notice, err := OpenResolvedStore(b.CommitsTo, dir)
	require.NoError(t, err)
	assert.Empty(t, notice, "the disclosure belongs to the login's own decision, not to opening a named store")
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	assert.Equal(t, 1, probes)
	assert.NoFileExists(t, PinPath(dir), "the pin is the change's last step")
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}

// TestLazyStoreDefersAStoreConflict asserts that a declaration the
// variable disagrees with is carried into the deferred resolution rather
// than failing construction. Every command builds this store through
// LoadAndStore, so an eager failure would stop help, schema, completion,
// `config path` and a RANCHER_CLI_TOKEN run on a conflict none of them
// would ever have acted on. The first credential call is where it lands.
func TestLazyStoreDefersAStoreConflict(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()

	store, err := NewLazyStore(&Config{CredentialStore: "keychain"}, dir, false)
	require.NoError(t, err, "construction is not where a conflict surfaces")
	require.NotNil(t, store)
	assert.Empty(t, store.(NoticeSource).DrainNotices(), "a store nobody resolved produces nothing")

	_, gerr := store.GetToken(ServerRef{Name: "prod", URL: "https://r.ex"})
	require.Error(t, gerr)
	ce, ok := errors.AsType[*clierr.Error](gerr)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, gerr, EnvCredentialStore)

	_, serr := store.SetTokenWithOutcome(ServerRef{Name: "prod", URL: "https://r.ex"}, "id:secret", PriorCredential{})
	require.ErrorContains(t, serr, EnvCredentialStore, "every credential call reports it")

	assert.NoFileExists(t, PinPath(dir), "a refused resolution probes nothing and pins nothing")
}

// TestLazyStoreStillFailsFastOnAnUnknownPolicy asserts the one resolution
// failure that stays eager: a value no backend answers to is broken for
// every command, whether or not one opens the store.
func TestLazyStoreStillFailsFastOnAnUnknownPolicy(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	_, err := NewLazyStore(&Config{CredentialStore: "nonsense"}, t.TempDir(), false)
	require.Error(t, err)
}
