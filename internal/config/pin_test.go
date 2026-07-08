package config

import (
	"os"
	"path/filepath"
	"testing"
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
	if fi, err := os.Stat(filepath.Join(dir, "credstore-pin")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("pin file mode/err: %v %v", fi, err)
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
	dir := t.TempDir()
	if err := WritePin(dir, "file"); err != nil {
		t.Fatal(err)
	}
	orig := keyringUsable
	keyringUsable = func() bool { return true } // keychain now works
	defer func() { keyringUsable = orig }()

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

func TestRepinForLoginNoopWhenExplicitPolicy(t *testing.T) {
	dir := t.TempDir()
	cur := NewFileStore(dir)
	s, err := RepinForLogin(dir, "file", cur)
	if err != nil || s != cur {
		t.Fatalf("explicit policy must not re-probe: %v %v", s, err)
	}
}

func TestRepinForLoginKeychainPinNeverDowngrades(t *testing.T) {
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
