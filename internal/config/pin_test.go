package config

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := ReadPin(dir); got != "" {
		t.Fatalf("ReadPin on empty dir = %q, want empty", got)
	}
	if err := WritePin(dir, "keychain"); err != nil {
		t.Fatal(err)
	}
	if got := ReadPin(dir); got != "keychain" {
		t.Errorf("ReadPin = %q, want keychain", got)
	}
	fi, err := os.Stat(filepath.Join(dir, "credstore-pin"))
	if err != nil {
		t.Errorf("pin file mode/err: %v %v", fi, err)
	} else if runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (hardenACL is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("pin file mode/err: %v %v", fi, err)
		}
	}
}

func TestReadPinIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "credstore-pin"), []byte("keyChain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadPin(dir); got != "" {
		t.Errorf("garbage pin = %q, want empty (re-probe)", got)
	}
}

func TestRepinForLoginUpgradesFileToKeychain(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate from the operator's shell (env outranks the "" policy under test)
	dir := t.TempDir()
	if err := WritePin(dir, "file"); err != nil {
		t.Fatal(err)
	}
	orig := keyringUsable
	keyringUsable = func() bool { return true } // keychain now works
	resetKeyringUsableCache()                   // fresh probe, not a result cached by an earlier test
	defer func() { keyringUsable = orig; resetKeyringUsableCache() }()

	cur := NewFileStore(dir)
	s, err := RepinForLogin(dir, "", cur)
	if err != nil {
		t.Fatal(err)
	}
	if got := ReadPin(dir); got != "keychain" {
		t.Errorf("pin after login re-probe = %q, want keychain", got)
	}
	if _, ok := s.(*keyringStore); !ok {
		t.Errorf("store after upgrade = %T, want keyringStore", s)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if got := ReadPin(dir); got != "file" {
		t.Fatalf("pin after OpenStore = %q, want file", got)
	}
	if _, err := RepinForLogin(dir, "", store); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (RepinForLogin must reuse OpenStore's memoized probe)", probes)
	}
}

func TestRepinForLoginNoopWhenExplicitPolicy(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate: env would otherwise outrank the explicit "file" policy under test
	dir := t.TempDir()
	cur := NewFileStore(dir)
	s, err := RepinForLogin(dir, "file", cur)
	if err != nil || s != cur {
		t.Fatalf("explicit policy must not re-probe: %v %v", s, err)
	}
}

func TestRepinForLoginKeychainPinNeverDowngrades(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "") // isolate from the operator's shell (env outranks the "" policy under test)
	dir := t.TempDir()
	if err := WritePin(dir, "keychain"); err != nil {
		t.Fatal(err)
	}
	probes := 0
	orig := keyringUsable
	keyringUsable = func() bool { probes++; return false } // keychain "broken" this run
	defer func() { keyringUsable = orig }()

	cur := NewFileStore(dir)
	s, err := RepinForLogin(dir, "", cur)
	if err != nil {
		t.Fatal(err)
	}
	if probes != 0 {
		t.Errorf("keychain-pinned login must not probe, got %d probes", probes)
	}
	if got := ReadPin(dir); got != "keychain" {
		t.Errorf("pin downgraded to %q; a transient keychain hiccup must not flip to plaintext", got)
	}
	if s != cur {
		t.Errorf("expected the injected store returned unchanged, got %T", s)
	}
}

// TestRepinForLoginFreshPinNotices pins the R1-1 contract: the FIRST pin
// written by a fresh-auto login emits the same notice eager OpenStore used
// to emit before login ran — without it, lazy resolution silently loses
// the e2e-asserted "pinned credential store: keychain" line.
func TestRepinForLoginFreshPinNotices(t *testing.T) {
	dir := t.TempDir()
	resetKeyringUsableCache()
	origUsable := keyringUsable
	keyringUsable = func() bool { return true }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	var buf bytes.Buffer
	origOut := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(origOut)

	current := NewFileStore(dir)
	store, err := RepinForLogin(dir, "", current)
	if err != nil {
		t.Fatalf("RepinForLogin: %v", err)
	}
	if store == current {
		t.Fatalf("expected an upgraded keyring store, got the input store")
	}
	if got := ReadPin(dir); got != "keychain" {
		t.Errorf("pin = %q, want keychain", got)
	}
	if !strings.Contains(buf.String(), "pinned credential store: keychain") {
		t.Errorf("missing first-pin notice; stderr log: %q", buf.String())
	}
	if strings.Contains(buf.String(), "re-pinned") {
		t.Errorf("fresh pin must not use the re-pin message; got %q", buf.String())
	}
}
