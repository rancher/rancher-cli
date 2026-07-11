package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
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
	if env := os.Getenv("RANCHER_CLI_CREDENTIAL_STORE"); env != "" {
		return env
	}
	if cfgPolicy != "" {
		return cfgPolicy
	}
	return "auto"
}

// RepinForLogin re-probes at login time (only under auto): login is rare,
// interactive, and the moment a new credential is written — the one place a
// machine pinned "file" by an accident of first run (e.g. over SSH) can
// upgrade to the keychain. It is upgrade-only and reads the pin FIRST: a
// keychain pin is never re-probed, so a transient keychain hiccup can never
// downgrade a machine to plaintext (a genuinely broken keychain fails loudly at
// SetToken with the pinned-unusable hint). The read path never re-probes (CS-3).
func RepinForLogin(dir, cfgPolicy string, current CredStore) (CredStore, error) {
	if EffectivePolicy(cfgPolicy) != "auto" {
		return current, nil
	}
	if ReadPin(dir) == "keychain" {
		// Already on the keychain — never probe, never downgrade.
		return current, nil
	}
	// Pin is "file" or absent: this is the only place we probe, and only to
	// upgrade. If the keychain is unusable, keep the file backend as-is — no
	// rewrite, no duplicate plaintext notice. keyringUsableOnce shares its
	// memoized result with OpenStore's own auto-probe above, so a single
	// `rancher login` process never pays the (bounded) probe cost twice.
	if !keyringUsableOnce() {
		return current, nil
	}
	pinned := ReadPin(dir)
	if err := WritePin(dir, "keychain"); err != nil {
		return nil, fmt.Errorf("writing credential store pin: %w", err)
	}
	if pinned != "" {
		logrus.Warnf("credential store re-pinned: %s → keychain", pinned)
	}
	return newPinnedKeyringStore(dir, true), nil
}
