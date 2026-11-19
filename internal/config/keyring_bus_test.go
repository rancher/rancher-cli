package config

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

var errTestNoBus = errors.New("no D-Bus session bus: test cause")

// stubSessionBusCheck makes the check run inside the test binary with the
// given answer, counts its calls, starts from an unset address, and restores
// everything at cleanup. The tests that use it must not run in parallel.
func stubSessionBusCheck(t *testing.T, ok bool, cause error) *int {
	t.Helper()
	t.Setenv(sessionBusEnv, "")
	prev := sessionBusAddressFound
	calls := 0
	sessionBusAddressFound = func() (bool, error) {
		calls++
		return ok, cause
	}
	restore := SetSessionBusCheckForTest()
	resetKeyringBusCause()
	t.Cleanup(func() {
		restore()
		sessionBusAddressFound = prev
		resetKeyringBusCause()
	})
	return &calls
}

// TestGoKeyringDisablesTheSessionBusWhenNoneExists asserts that with no
// session bus the first Set, Get or Delete sets the address to a value the
// D-Bus library rejects at once, runs the check once for the process, and
// gives a failure of go-keyring the cause as its text while errors.Is still
// reaches the cause and go-keyring's error.
func TestGoKeyringDisablesTheSessionBusWhenNoneExists(t *testing.T) {
	calls := stubSessionBusCheck(t, false, errTestNoBus)
	mockErr := errors.New("keyring transport failed")
	keyring.MockInitWithError(mockErr)

	err := goKeyring{}.Set("svc", "user", "secret")
	require.Error(t, err)
	assert.Equal(t, sessionBusDisabled, os.Getenv(sessionBusEnv))
	require.ErrorIs(t, err, errTestNoBus)
	require.ErrorIs(t, err, mockErr)
	assert.True(t, keyringUnreachable(err))
	require.EqualError(t, err, errTestNoBus.Error())

	_, err = goKeyring{}.Get("svc", "user")
	require.ErrorIs(t, err, errTestNoBus)
	err = goKeyring{}.Delete("svc", "user")
	require.ErrorIs(t, err, errTestNoBus)
	assert.Equal(t, 1, *calls)
}

// TestGoKeyringKeepsAMissAndAnOversizedValueUnwrapped asserts that with no
// session bus a miss and an oversized value reach the caller as go-keyring
// returned them, because neither says the keychain is unreachable.
func TestGoKeyringKeepsAMissAndAnOversizedValueUnwrapped(t *testing.T) {
	stubSessionBusCheck(t, false, errTestNoBus)
	keyring.MockInit()
	_, err := goKeyring{}.Get("svc", "missing")
	assert.Equal(t, keyring.ErrNotFound, err)

	keyring.MockInitWithError(keyring.ErrSetDataTooBig)
	err = goKeyring{}.Set("svc", "user", "secret")
	assert.Equal(t, keyring.ErrSetDataTooBig, err)
}

// TestGoKeyringLeavesTheSessionBusAloneWhenOneExists asserts that when the
// check finds a session bus the address is untouched and a go-keyring
// failure keeps its own text.
func TestGoKeyringLeavesTheSessionBusAloneWhenOneExists(t *testing.T) {
	calls := stubSessionBusCheck(t, true, nil)
	t.Setenv(sessionBusEnv, "unix:path=/run/user/1/bus")
	mockErr := errors.New("keyring transport failed")
	keyring.MockInitWithError(mockErr)

	err := goKeyring{}.Set("svc", "user", "secret")
	assert.Equal(t, mockErr, err)
	assert.Equal(t, "unix:path=/run/user/1/bus", os.Getenv(sessionBusEnv))
	assert.Equal(t, 1, *calls)
}

// TestGoKeyringSkipsTheSessionBusCheckInATestBinary asserts that a test
// binary does not run the check unless the test turned it on.
func TestGoKeyringSkipsTheSessionBusCheckInATestBinary(t *testing.T) {
	prev := sessionBusAddressFound
	t.Cleanup(func() {
		sessionBusAddressFound = prev
		resetKeyringBusCause()
	})
	sessionBusAddressFound = func() (bool, error) {
		t.Error("the check ran in a test binary")
		return false, errTestNoBus
	}
	resetKeyringBusCause()
	keyring.MockInit()
	require.NoError(t, goKeyring{}.Set("svc", "user", "secret"))
}

// TestGoKeyringRunsTheSessionBusCheckInProduction asserts that a process that
// is not a test binary runs the check on its own, with no opt-in, and sets
// the address to a value the D-Bus library rejects at once when no address
// is found.
func TestGoKeyringRunsTheSessionBusCheckInProduction(t *testing.T) {
	calls := stubSessionBusCheck(t, false, errTestNoBus)
	prev := busCheckUnderTest
	t.Cleanup(func() { busCheckUnderTest = prev })
	busCheckUnderTest = func() bool { return false }
	busCheckInTest.Store(false)
	resetKeyringBusCause()
	mockErr := errors.New("keyring transport failed")
	keyring.MockInitWithError(mockErr)

	err := goKeyring{}.Set("svc", "user", "secret")
	require.ErrorIs(t, err, errTestNoBus)
	assert.Equal(t, 1, *calls)
	assert.Equal(t, sessionBusDisabled, os.Getenv(sessionBusEnv))
}

// TestKeychainRemedyForNoSessionBus asserts that the remedy for a missing
// session bus is only a session with one, the remedy for a user that could
// not be looked up is the address or the lookup, and the other failures
// keep the remedy of the operating system.
func TestKeychainRemedyForNoSessionBus(t *testing.T) {
	cause := fmt.Errorf("%w: test", errNoSessionBus)
	wrapped := withBusCause(errors.New("transport failed"), cause)
	assert.Equal(t, "run in a session with a D-Bus session bus", keychainRemedyFor(wrapped))
	lookup := withBusCause(errors.New("transport failed"), &userLookupCause{text: "lookup failed", lookup: errors.New("no user")})
	assert.Equal(t, "set DBUS_SESSION_BUS_ADDRESS to the address of the session bus, or set USER and HOME", keychainRemedyFor(lookup))
	assert.Equal(t, keychainRemedy(), keychainRemedyFor(errors.New("transport failed")))
}

// TestKeychainMessagesCarryTheRemedyOfTheCause asserts that each of the three
// builders of a keychain message gives a missing session bus its own remedy
// and not the keychain's, and does the same for a user that could not be
// looked up.
func TestKeychainMessagesCarryTheRemedyOfTheCause(t *testing.T) {
	withKeychainOS(t, "linux")
	transport := errors.New("transport failed")
	builders := map[string]func(error) error{
		"keychainError": func(err error) error { return keychainError("get", "prod", err, "") },
		"keychainUnusableAtFirstLogin": func(err error) error {
			return keychainUnusableAtFirstLogin("prod", err, "")
		},
		"keychainUnusableForKeychainDirectory": func(err error) error {
			return keychainUnusableForKeychainDirectory("keychain", "/c/credentials.yaml", err, "")
		},
	}
	for _, tc := range []struct {
		name   string
		cause  error
		remedy string
	}{
		{"no session bus", fmt.Errorf("%w: test", errNoSessionBus), "; run in a session with a D-Bus session bus, then run this command again"},
		{"user lookup", &userLookupCause{text: "lookup failed", lookup: errors.New("no user")}, "; set DBUS_SESSION_BUS_ADDRESS to the address of the session bus, or set USER and HOME, then run this command again"},
	} {
		for name, build := range builders {
			t.Run(tc.name+" "+name, func(t *testing.T) {
				err := build(withBusCause(transport, tc.cause))
				require.ErrorContains(t, err, tc.remedy)
				assert.NotContains(t, err.Error(), "unlock the keychain")
			})
		}
	}
}
