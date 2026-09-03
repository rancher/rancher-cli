package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
// paid the (bounded) probe cost twice in one login — once via OpenStore's
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
// TestLoginFirstPinNoticeReachesWarnings in internal/commands/login_test.go,
// where OpenStoreWithNotice always runs before RepinForLogin and pins the dir
// first — so the else branch is dead on that path.
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
