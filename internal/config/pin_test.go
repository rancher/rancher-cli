package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, ReadPin(dir), "ReadPin on empty dir")
	require.NoError(t, WritePin(dir, "keychain"))
	assert.Equal(t, "keychain", ReadPin(dir))
	fi, err := os.Stat(filepath.Join(dir, "credstore-pin"))
	if assert.NoError(t, err, "pin file mode/err") && runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (hardenACL is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
}

func TestReadPinIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credstore-pin"), []byte("keyChain\n"), 0o600))
	assert.Empty(t, ReadPin(dir), "garbage pin should re-probe")
}

func TestRepinForLoginUpgradesFileToKeychain(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate from the operator's shell (env outranks the "" policy under test)
	dir := t.TempDir()
	require.NoError(t, WritePin(dir, "file"))
	orig := keyringUsable
	keyringUsable = func() bool { return true } // keychain now works
	resetKeyringUsableCache()                   // fresh probe, not a result cached by an earlier test
	defer func() { keyringUsable = orig; resetKeyringUsableCache() }()

	cur := NewFileStore(dir)
	s, _, err := RepinForLogin(dir, "", cur)
	require.NoError(t, err)
	assert.Equal(t, "keychain", ReadPin(dir), "pin after login re-probe")
	assert.IsType(t, &keyringStore{}, s, "store after upgrade")
}

// Reviewer repro: a fresh unpinned dir with an unusable keychain previously
// paid the (bounded) probe cost twice in one login - once via OpenStore's
// auto-probe (which pins "file"), once more via RepinForLogin re-probing
// since the pin is now "file" not "keychain". Both calls must share one
// memoized probe.
func TestOpenStoreThenRepinForLoginShareOneProbe(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	dir := t.TempDir()
	probes := 0
	orig := keyringUsable
	keyringUsable = func() bool { probes++; return false } // keychain unusable
	resetKeyringUsableCache()
	defer func() { keyringUsable = orig; resetKeyringUsableCache() }()

	store, err := OpenStore("auto", dir)
	require.NoError(t, err)
	require.Equal(t, "file", ReadPin(dir), "pin after OpenStore")
	_, _, err = RepinForLogin(dir, "", store)
	require.NoError(t, err)
	assert.Equal(t, 1, probes, "RepinForLogin must reuse OpenStore's memoized probe")
}

func TestRepinForLoginNoopWhenExplicitPolicy(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate: env would otherwise outrank the explicit "file" policy under test
	dir := t.TempDir()
	cur := NewFileStore(dir)
	s, _, err := RepinForLogin(dir, "file", cur)
	require.NoError(t, err, "explicit policy must not re-probe")
	require.Equal(t, cur, s, "explicit policy must not re-probe")
}

func TestRepinForLoginKeychainPinNeverDowngrades(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate from the operator's shell (env outranks the "" policy under test)
	dir := t.TempDir()
	require.NoError(t, WritePin(dir, "keychain"))
	probes := 0
	orig := keyringUsable
	keyringUsable = func() bool { probes++; return false } // keychain "broken" this run
	defer func() { keyringUsable = orig }()

	cur := NewFileStore(dir)
	s, _, err := RepinForLogin(dir, "", cur)
	require.NoError(t, err)
	assert.Equal(t, 0, probes, "keychain-pinned login must not probe")
	assert.Equal(t, "keychain", ReadPin(dir), "pin downgraded; a transient keychain hiccup must not flip to plaintext")
	assert.Equal(t, cur, s, "expected the injected store returned unchanged")
}

// TestRepinForLoginFreshPinNotices asserts the direct-call contract: calling
// RepinForLogin on a never-pinned dir (no prior openStoreResolved) returns a
// first-pin notice. This covers RepinForLogin's else branch in isolation.
// The production loginCmd.Execute path is covered by
// TestLoginFirstPinNoticeReachesWarnings in internal/commands/auth/login_test.go,
// where OpenStoreWithNotice always runs before RepinForLogin and pins the dir
// first - so the else branch is dead on that path.
func TestRepinForLoginFreshPinNotices(t *testing.T) {
	dir := t.TempDir()
	resetKeyringUsableCache()
	origUsable := keyringUsable
	keyringUsable = func() bool { return true }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	current := NewFileStore(dir)
	store, warning, err := RepinForLogin(dir, "", current)
	require.NoError(t, err, "RepinForLogin")
	require.NotEqual(t, current, store, "expected an upgraded keyring store, got the input store")
	assert.Equal(t, "keychain", ReadPin(dir))
	require.NotEmpty(t, warning, "expected first-pin notice")
	assert.Equal(t, NoticeFirstPinKeychain, warning, "missing first-pin notice")
	assert.NotContains(t, warning, "re-pinned", "fresh pin must not use the re-pin message")
}

// TestEffectivePolicyWithSourceConflict asserts what the variable and the
// declaration do to each other. A directory that declares a store owns
// that choice: a variable naming a different one is reported rather than
// silently winning, because two processes on one directory would
// otherwise read different backends and, when both hold a credential,
// each would serve whichever its own environment selected. The declared
// value still comes back beside the error so a caller that only displays
// the pair can show what the directory says.
func TestEffectivePolicyWithSourceConflict(t *testing.T) {
	for _, tc := range []struct {
		name           string
		env, declared  string
		policy, source string
		conflict       bool
	}{
		{name: "nothing set", policy: "auto", source: "default"},
		{name: "declaration only", declared: "file", policy: "file", source: "config"},
		{name: "variable only", env: "file", policy: "file", source: "env"},
		{name: "both agree", env: "file", declared: "file", policy: "file", source: "env"},
		{name: "both disagree", env: "file", declared: "keychain", policy: "keychain", source: "config", conflict: true},
		{name: "variable disagrees with auto", env: "file", declared: "auto", policy: "auto", source: "config", conflict: true},
		{name: "helper against keychain", env: "exec:vault", declared: "keychain", policy: "keychain", source: "config", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, tc.env)
			policy, source, err := EffectivePolicyWithSource(tc.declared)
			assert.Equal(t, tc.policy, policy)
			assert.Equal(t, tc.source, source)
			if !tc.conflict {
				require.NoError(t, err)
				valueOnly, verr := EffectivePolicy(tc.declared)
				require.NoError(t, verr)
				assert.Equal(t, tc.policy, valueOnly, "the value-only form cannot disagree")
				return
			}
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, strconv.Quote(tc.declared))
			require.ErrorContains(t, err, strconv.Quote(tc.env))
			require.ErrorContains(t, err, EnvCredentialStore)
			assert.Equal(t, "unset "+EnvCredentialStore, ce.Hint, "the remedy is one command a caller can run")

			_, verr := EffectivePolicy(tc.declared)
			require.Error(t, verr, "the value-only form carries the conflict too")
		})
	}
}

// TestRepinForLoginRefusesAConflict asserts the repin does not probe or
// write a pin for a directory whose declaration the variable disagrees
// with: the login that reaches it is already wrong about which store it
// is writing to.
func TestRepinForLoginRefusesAConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCredentialStore, "file")
	store, notice, err := RepinForLogin(dir, "keychain", NewFileStore(dir))
	require.Error(t, err)
	assert.Nil(t, store)
	assert.Empty(t, notice)
	assert.Empty(t, ReadPin(dir), "no pin is written on the way out")
}
