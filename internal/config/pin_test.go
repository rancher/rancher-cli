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
