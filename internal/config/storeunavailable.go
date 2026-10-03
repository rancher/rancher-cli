package config

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// keychainOS is the operating system whose keychain the remedies name.
// A package variable so a test can check the remedy of each one.
var keychainOS = runtime.GOOS

// securityCancelledStatus is the exit status /usr/bin/security reports
// when the keychain is locked and its unlock prompt is cancelled.
// go-keyring keeps only that status, not the command's output, so the
// message says what the status shows and no more.
const securityCancelledStatus = 128

// orAnotherConfig is the tail of a keychain error on a path a logout
// cannot clear either - a store change, the removal of an orphan - where
// the only other way out is a directory that does not use this keychain.
const orAnotherConfig = ", or use a different --config"

// keychainError turns a failed keychain call into the error its caller
// returns. A miss and an oversized value are not the store being
// unavailable and keep the plain form; every other failure is
// CREDENTIAL_STORE_UNAVAILABLE, naming the entry, the cause as far as the
// error shows it, and the remedy for this operating system's keychain.
// op is the go-keyring call ("get", "set" or "delete"); tail is appended
// to the message as it is, empty or starting with a space.
func keychainError(op, entry string, err error, tail string) error {
	if !keyringUnreachable(err) {
		return fmt.Errorf("keyring %s %q: %w%s", op, entry, err, tail)
	}
	verb := map[string]string{"get": "read", "set": "store", "delete": "delete"}[op]
	return clierr.Retryable(clierr.CodeCredentialStoreUnavailable, fmt.Sprintf(
		"the keychain could not %s the credential for %s: %s; %s, then run this command again%s",
		verb, clierr.ShellQuote(entry), keychainCause(err), keychainRemedy(), tail)).
		WithCause(err)
}

// asStoreUnavailable returns err's CREDENTIAL_STORE_UNAVAILABLE error, or
// nil when err is classified otherwise or not at all. A caller that adds
// its own context keeps the code, the retry flag and the remedy.
func asStoreUnavailable(err error) *clierr.Error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeCredentialStoreUnavailable {
		return ce
	}
	return nil
}

// keychainLocked reports whether err is macOS's security exit status for a
// locked keychain whose prompt was cancelled. Only that status says the
// keychain exists and is locked; every other failure, and any status on
// another operating system, says nothing about it.
func keychainLocked(err error) bool {
	ee, ok := errors.AsType[*exec.ExitError](err)
	return ok && keychainOS == "darwin" && ee.ExitCode() == securityCancelledStatus
}

// keychainAwaitsUnlock reports whether a failed keychain probe is what a
// locked macOS keychain looks like: security's status for a cancelled
// prompt, or no answer within the probe's limit while its prompt waits.
// Elsewhere neither says the keychain exists and is locked.
func keychainAwaitsUnlock(err error) bool {
	return keychainOS == "darwin" && (keychainLocked(err) || errors.Is(err, errKeyringProbeTimedOut))
}

// keychainUnusableAtFirstLogin refuses the first login of an `auto`
// directory whose keychain probe found the keychain locked or waiting on
// its prompt: no credential is stored and nothing is pinned. alt is the
// plaintext file store offered by name, or empty when the caller has no
// command that chooses it.
func keychainUnusableAtFirstLogin(entry string, err error, alt string) error {
	return clierr.Retryable(clierr.CodeCredentialStoreUnavailable, fmt.Sprintf(
		"the keychain could not store the credential for %s: %s; %s, then run this command again%s",
		clierr.ShellQuote(entry), keychainCause(err), keychainRemedy(), alt)).
		WithCause(err)
}

// keychainUnusableForKeychainDirectory refuses an `auto` login on a
// directory whose credentials are in the keychain or a credential helper
// when the probe says the keychain cannot be used now: falling back to
// the plaintext file would move them out on a probe.
func keychainUnusableForKeychainDirectory(readsNow, credentialsFile string, err error, alt string) error {
	return clierr.Retryable(clierr.CodeCredentialStoreUnavailable, fmt.Sprintf(
		"the keychain cannot be used right now (%s), and a login that asks for auto never moves this directory's credentials out of %s into the plaintext file %s; %s, then run this command again%s",
		keychainCause(err), readsNow, credentialsFile, keychainRemedy(), alt)).
		WithCause(err)
}

// fileStoreAlternative is the command that keeps entry's credential in
// the plaintext file store on purpose, as the tail of a refusal, or empty
// when there is none to offer: auth login refuses the exec plugin's own
// entry names, and an entry that is not registered yet needs a URL this
// code does not have.
func fileStoreAlternative(cfg *Config, entry string) string {
	if IsExecLoginEntryName(entry) {
		return ""
	}
	if _, ok := cfg.Servers[entry]; !ok {
		return ""
	}
	return fmt.Sprintf(", or run %s auth login %s --credential-store file to keep its credential in a plaintext file on purpose",
		clierr.Rancher, clierr.ShellQuote(entry))
}

// keychainCause says what a failed keychain call shows about why. On
// macOS go-keyring runs /usr/bin/security and returns only its exit
// status; status 128 is the one whose meaning is known.
func keychainCause(err error) string {
	if errors.Is(err, errKeyringProbeTimedOut) {
		if keychainOS == "darwin" {
			return fmt.Sprintf("the keychain did not answer within %s, as when its unlock prompt is not answered in time", keyringProbeTimeout)
		}
		return fmt.Sprintf("the keychain did not answer within %s", keyringProbeTimeout)
	}
	ee, ok := errors.AsType[*exec.ExitError](err)
	if !ok || keychainOS != "darwin" {
		return err.Error()
	}
	if ee.ExitCode() == securityCancelledStatus {
		return fmt.Sprintf("the keychain is locked or its prompt was cancelled (security exited with status %d)", securityCancelledStatus)
	}
	return fmt.Sprintf("security exited with status %d", ee.ExitCode())
}

// keychainRemedy is what makes this operating system's keychain usable
// again: each names the tool or step its own documentation does.
func keychainRemedy() string {
	switch keychainOS {
	case "darwin":
		return "unlock the keychain with security unlock-keychain, or approve the macOS prompt"
	case "windows":
		return "make Windows Credential Manager reachable for this user"
	default:
		return "unlock the Secret Service keyring, or run in a session with a D-Bus session bus"
	}
}

// helperUnavailable is CREDENTIAL_STORE_UNAVAILABLE for a credential
// helper that is not installed, did not answer in time, or failed: the
// helper, the operation, the entry, and the cause with its remedy. For a
// helper that failed, the cause is the helper's own message, which is
// where its remedy is.
func helperUnavailable(binary, verb, entry, cause string) error {
	op := map[string]string{"get": "read", "store": "store", "erase": "erase"}[verb]
	return clierr.Retryable(clierr.CodeCredentialStoreUnavailable, fmt.Sprintf(
		"credential helper %s could not %s the credential for %s: %s", binary, op, clierr.ShellQuote(entry), cause))
}
