package config

import (
	"os"
	"path/filepath"
	"strings"
)

// The pin file records what `auto` resolved to on THIS machine. It is
// machine-local probe state, deliberately separate from config.yaml (a
// portable user declaration): copying config.yaml to another machine must
// not carry a keychain pin, and pinning into config would erase the record
// that the user chose auto. Single value, atomic write, last-writer-wins —
// concurrent pinners compute the same probe result, so no lock is needed.
const pinFileName = "credstore-pin"

func PinPath(dir string) string { return filepath.Join(dir, pinFileName) }

// ReadPin returns the pinned backend, or "" when absent/unreadable/invalid
// (invalid values re-probe rather than error: the pin is a cache, not config).
func ReadPin(dir string) string {
	b, err := os.ReadFile(PinPath(dir))
	if err != nil {
		return ""
	}
	switch v := strings.TrimSpace(string(b)); v {
	case "keychain", "file":
		return v
	}
	return ""
}

func WritePin(dir, backend string) error {
	return atomicWriteFile(PinPath(dir), []byte(backend+"\n"), 0o600)
}

// EffectivePolicy resolves the declared policy: env > config > auto.
func EffectivePolicy(cfgPolicy string) string {
	if env := os.Getenv("RANCHER_CREDENTIAL_STORE"); env != "" {
		return env
	}
	if cfgPolicy != "" {
		return cfgPolicy
	}
	return "auto"
}
