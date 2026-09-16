package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// The pin file records what `auto` resolved to on THIS machine. It is
// machine-local probe state, deliberately separate from config.yaml (a
// portable user declaration): copying config.yaml to another machine must
// not carry a keychain pin, and pinning into config would erase the record
// that the user chose auto. Single value, atomic write, last-writer-wins -
// concurrent pinners compute the same probe result, so no lock is needed.
const pinFileName = "credstore-pin"

// NoticeFirstPinKeychain is the user-visible message returned when the auto
// credential-store backend is pinned to the keychain for the first time on
// this machine. The real emitter is openStoreResolved (credstore.go): it runs
// before RepinForLogin on the production login path and handles the fresh-machine
// first-pin case. RepinForLogin's else branch also references it, but that
// branch is dead on the production path (see its own comment). Sharing the
// const means a wording change applies uniformly and tests can reference it
// without repeating the literal.
const NoticeFirstPinKeychain = "pinned credential store: keychain"

func PinPath(dir string) string { return filepath.Join(dir, pinFileName) }

// PlatformKeychainLabel names the OS-native keychain auto tries first on
// this machine's platform, purely from runtime.GOOS - never a probe. `config
// status` uses it to answer "what will auto pick?" before any pin exists,
// without the side effects (or latency) of actually trying the keychain.
func PlatformKeychainLabel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "windows":
		return "Windows Credential Manager"
	case "linux":
		return "Secret Service keyring (D-Bus)"
	default:
		return "OS keychain"
	}
}

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
func EffectivePolicy(cfgPolicy string) (string, error) {
	p, _, err := EffectivePolicyWithSource(cfgPolicy)
	return p, err
}

// EffectivePolicyWithSource returns the effective credential-store policy
// and where it came from: "env" (RANCHER_CLI_CREDENTIAL_STORE), "config"
// (config.yaml's credentialStore), or "default" (nothing set - auto).
// `config status` reports this pair; EffectivePolicy is the value-only form
// every resolution path uses, so the two can never disagree.
//
// A directory that declares a store and a variable that asks for a
// different one is a conflict, not a precedence question: two processes on
// one directory would otherwise read different backends, and when both
// hold a credential each would silently use whichever its environment
// selected. The declared value is returned beside the error so a caller
// that only displays the pair can show what the directory says. The same
// value in both is no conflict, and a variable over a directory that has
// not declared yet applies and is recorded by the next login, which is
// what the variable is for on a fresh directory and in CI.
func EffectivePolicyWithSource(cfgPolicy string) (policy, source string, err error) {
	env := os.Getenv(EnvCredentialStore)
	switch {
	case env != "" && cfgPolicy != "" && env != cfgPolicy:
		return cfgPolicy, "config", storeConflictError(cfgPolicy, env)
	case env != "":
		return env, "env", nil
	case cfgPolicy != "":
		return cfgPolicy, "config", nil
	}
	return "auto", "default", nil
}

// storeConflictError reports the declaration and the variable naming
// different stores. VALIDATION: the command was asked for two stores and
// choosing either one risks serving a credential the caller did not mean.
func storeConflictError(declared, asked string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"this config directory stores credentials in %q (config.yaml credentialStore); %s asks for %q; unset the variable, or use a different --config",
		declared, EnvCredentialStore, asked)).
		WithHint("unset " + EnvCredentialStore)
}

// RepinForLogin re-probes at login time (only under auto): login is rare,
// interactive, and the moment a new credential is written - the one place a
// machine pinned "file" by an accident of first run (e.g. over SSH) can
// upgrade to the keychain. It is upgrade-only and reads the pin FIRST: a
// keychain pin is never re-probed, so a transient keychain hiccup can never
// downgrade a machine to plaintext (a genuinely broken keychain fails loudly at
// SetToken with the pinned-unusable hint). The read path never re-probes (CS-3).
// The second return value carries any user-visible notice so callers can route
// it through the warnings pipeline instead of writing to a logging framework.
// Empty string means no action was taken (already keychain-pinned, explicit
// policy, or unusable keychain).
func RepinForLogin(dir, cfgPolicy string, current CredStore) (CredStore, string, error) {
	policy, perr := EffectivePolicy(cfgPolicy)
	if perr != nil {
		return nil, "", perr
	}
	if policy != "auto" {
		return current, "", nil
	}
	if ReadPin(dir) == "keychain" {
		// Already on the keychain - never probe, never downgrade.
		return current, "", nil
	}
	// Pin is "file" or absent: this is the only place we probe, and only to
	// upgrade. If the keychain is unusable, keep the file backend as-is - no
	// rewrite, no duplicate plaintext notice. keyringUsableOnce shares its
	// memoized result with OpenStore's own auto-probe above, so a single
	// `rancher login` process never pays the (bounded) probe cost twice.
	if !keyringUsableOnce() {
		return current, "", nil
	}
	pinned := ReadPin(dir)
	if err := WritePin(dir, "keychain"); err != nil {
		return nil, "", fmt.Errorf("writing credential store pin: %w", err)
	}
	var msg string
	if pinned != "" {
		msg = fmt.Sprintf("credential store re-pinned: %s → keychain", pinned)
	} else {
		// pinned == "": reachable only when RepinForLogin is called directly on
		// a never-pinned dir without running openStoreResolved first (e.g. in
		// unit tests or direct callers). Within loginCmd.Execute, openStoreResolved
		// always runs before RepinForLogin and writes a pin, so this branch is
		// dead on the production login path.
		//
		// Call-order coupling: if login.go ever reorders the two calls or replaces
		// OpenStoreWithNotice with a bare OpenStore, RepinForLogin would see an
		// empty pin here and produce a second NoticeFirstPinKeychain in res.Warnings
		// alongside the one from openStoreResolved. The coupling is documented at
		// the call site in login.go.
		msg = NoticeFirstPinKeychain
	}
	return newPinnedKeyringStore(dir, true), msg, nil
}
