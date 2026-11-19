package config

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zalando/go-keyring"
)

// keyringOps is the seam over the operating system keyring. go-keyring
// exposes no provider injection, and its in-memory mock cannot model a
// write that commits and then reports an error, which is the case this
// package must classify; tests swap the seam.
type keyringOps interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// goKeyring calls go-keyring. Before the first call of the process it
// makes sure go-keyring cannot start a D-Bus session bus (see
// keyringBusCause); a failure that follows from there being no bus carries
// that cause.
type goKeyring struct{}

func (goKeyring) Set(service, user, password string) error {
	cause := keyringBusCause()
	return withBusCause(keyring.Set(service, user, password), cause)
}

func (goKeyring) Get(service, user string) (string, error) {
	cause := keyringBusCause()
	v, err := keyring.Get(service, user)
	return v, withBusCause(err, cause)
}

func (goKeyring) Delete(service, user string) error {
	cause := keyringBusCause()
	return withBusCause(keyring.Delete(service, user), cause)
}

// sessionBusEnv is the variable that names the D-Bus session bus.
// sessionBusDisabled names no usable transport, so the D-Bus library returns
// an error at once for it and never starts a bus.
const (
	sessionBusEnv      = "DBUS_SESSION_BUS_ADDRESS"
	sessionBusDisabled = "disabled:"
)

// sessionBusAddressFound reports whether a session bus address exists. When it
// does not, cause says why in plain words. Platforms whose keychain does not
// go through the Secret Service report true; the others replace it (see
// keyring_bus_secretservice.go).
var sessionBusAddressFound = func() (ok bool, cause error) { return true, nil }

// busCheckUnderTest reports whether the process is a test binary. A test
// binary skips the check unless the test turned it on, so a host without a
// session bus does not change what the tests of every package see through
// go-keyring's in-memory mock.
var busCheckUnderTest = testing.Testing

var busCheckInTest atomic.Bool

// SetSessionBusCheckForTest lets the session bus check run inside a test
// binary until the returned function is called. The calling test must not
// use t.Parallel(): the setting is process-wide.
func SetSessionBusCheckForTest() (restore func()) {
	old := busCheckInTest.Swap(true)
	return func() { busCheckInTest.Store(old) }
}

var keyringBusCauseFn = sync.OnceValue(prepareSessionBus)

// keyringBusCause runs the session bus check once per process, before the
// first go-keyring call, and returns the cause when no session bus exists.
func keyringBusCause() error { return keyringBusCauseFn() }

// resetKeyringBusCause makes the next keyringBusCause run the check again.
// Tests that call it must not run in parallel.
func resetKeyringBusCause() { keyringBusCauseFn = sync.OnceValue(prepareSessionBus) }

// prepareSessionBus makes sure go-keyring cannot start a session bus. With
// no session bus address, the D-Bus library would start one through
// dbus-launch and the Secret Service lookup would then start
// gnome-keyring-daemon, and both keep running after the command; on such a
// host the keychain is unusable anyway. The address is set to a value the
// library rejects at once, and go-keyring is still called. Any address that
// exists is left alone: the library never starts a bus for one.
func prepareSessionBus() error {
	if busCheckUnderTest() && !busCheckInTest.Load() {
		return nil
	}
	ok, cause := sessionBusAddressFound()
	if ok {
		return nil
	}
	_ = os.Setenv(sessionBusEnv, sessionBusDisabled)
	return cause
}

// errNoSessionBus is the start of every cause of a missing session bus, so
// keychainRemedyFor can tell it from the other failures of the keychain.
var errNoSessionBus = errors.New("no D-Bus session bus")

// errUserLookup marks the cause that could not look for a session bus
// because the current user could not be looked up. The bus may exist there,
// so its remedy differs (keychainRemedyFor).
var errUserLookup = errors.New("the current user could not be looked up")

// userLookupCause is the cause of a session bus that was not looked for. It
// is a missing session bus (errNoSessionBus) for what the keychain does, and
// errUserLookup and the lookup error for what to tell the user.
type userLookupCause struct {
	text   string
	lookup error
}

func (e *userLookupCause) Error() string   { return e.text }
func (e *userLookupCause) Unwrap() []error { return []error{errNoSessionBus, errUserLookup, e.lookup} }

// busCauseError is a keychain failure on a host with no session bus. Its
// text is the cause alone: the error go-keyring returned after the address
// was disabled describes that override, not the host. Both stay reachable
// for errors.Is and errors.As.
type busCauseError struct {
	cause error
	err   error
}

func (e *busCauseError) Error() string   { return e.cause.Error() }
func (e *busCauseError) Unwrap() []error { return []error{e.cause, e.err} }

// withBusCause returns err with the no-session-bus cause when keyring
// failed as unreachable and a cause is set; a miss or an oversized value,
// and any other host, keep err as it is.
func withBusCause(err, cause error) error {
	if cause == nil || !keyringUnreachable(err) {
		return err
	}
	return &busCauseError{cause: cause, err: err}
}

var keyringBackend keyringOps = goKeyring{}

// KeyringOps is the exported name of the seam for tests in other packages.
type KeyringOps = keyringOps

// SetKeyringBackendForTest swaps the keyring seam and returns a restore
// function. Tests that call it must not run in parallel.
func SetKeyringBackendForTest(ops KeyringOps) (restore func()) {
	prev := keyringBackend
	keyringBackend = ops
	return func() { keyringBackend = prev }
}
