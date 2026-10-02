package config

import (
	"errors"
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
// that the user chose auto. Single value, atomic write. Only a login writes
// it, under the login's config lock; a read never does.
const pinFileName = "credstore-pin"

const (
	// noticeFirstPinPrefix opens the message for the moment this machine
	// decides where an `auto` directory keeps its credentials.
	noticeFirstPinPrefix = "pinned credential store: "
	// NoticeFirstPinKeychain is that message for the keychain, as a login's
	// own pin step gives it after the credential commits. A constant, so
	// tests can reference it without repeating the literal.
	NoticeFirstPinKeychain = noticeFirstPinPrefix + "keychain"
)

// noticePinned is what the moment reads like for any pin write: a first
// pin names the backend it chose, a later one names both, so an operator
// reading two runs can tell "this machine decided" from "this machine
// changed its mind".
func noticePinned(from, to string) string {
	if from == "" {
		return noticeFirstPinPrefix + to
	}
	return fmt.Sprintf("credential store re-pinned: %s → %s", from, to)
}

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
// (an invalid value reads as no pin rather than an error: what the
// directory has stored then answers, as it does for a missing one).
func ReadPin(dir string) string {
	observeIdentity("pinread")
	b, err := readFile(PinPath(dir))
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

// ErrCredentialStoreConflict is the cause of the refusal when the
// directory's declared store and RANCHER_CLI_CREDENTIAL_STORE disagree.
var ErrCredentialStoreConflict = errors.New("the declared credential store and " + EnvCredentialStore + " disagree")

// storeConflictError reports the declaration and the variable naming
// different stores. VALIDATION: the command was asked for two stores and
// choosing either one risks serving a credential the caller did not mean.
func storeConflictError(declared, asked string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"this config directory stores credentials in %q (config.yaml credentialStore); %s asks for %q; unset the variable, or use a different --config",
		declared, EnvCredentialStore, asked)).
		WithHint("unset " + EnvCredentialStore).
		WithCause(ErrCredentialStoreConflict)
}

// storeFlagConflictError is the same refusal for a login that names one
// store on the command line while the variable asks for another. The
// subject is the login, not the directory: naming config.yaml for a
// disagreement config.yaml has no part in would send the operator to the
// wrong file.
func storeFlagConflictError(requested, asked string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"this login asks for credentials in %q (--credential-store); %s asks for %q; unset the variable, or drop the flag",
		requested, EnvCredentialStore, asked)).
		WithHint("unset " + EnvCredentialStore)
}

// PolicySourceFlag is what LoginPolicy reports for a store named on the
// command line. It is the one source that may settle a directory whose
// declaration and variable disagree, because naming the store is what
// resolves the disagreement.
const PolicySourceFlag = "flag"

// LoginPolicy resolves the credential-store policy one login uses, and
// where it came from.
//
// A flag names the store outright: that is what it is for, so it is not
// weighed against the directory's declaration - changing that declaration
// is the point - and it is checked only against the variable, which must
// agree. Without a flag the ordinary rules apply, the config-versus-
// variable conflict included.
func LoginPolicy(requested, declared string) (policy, source string, err error) {
	if requested == "" {
		return EffectivePolicyWithSource(declared)
	}
	if cerr := CheckStorePolicy(requested); cerr != nil {
		return "", "", clierr.New(clierr.CodeValidation, cerr.Error())
	}
	if env := os.Getenv(EnvCredentialStore); env != "" && env != requested {
		return "", "", storeFlagConflictError(requested, env)
	}
	return requested, PolicySourceFlag, nil
}
