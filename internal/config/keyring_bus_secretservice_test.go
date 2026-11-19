//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

package config

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindSessionBus asserts the rule of the D-Bus library: only an empty
// address or exactly "autolaunch:" leaves the lookup to the files, a user
// that cannot be looked up gives no files, /run/user/<uid>/bus counts when it
// exists, and /run/user/<uid>/dbus-session counts only when it can be read
// and starts with the variable name and an address.
func TestFindSessionBus(t *testing.T) {
	const bus, session = "/run/user/1000/bus", "/run/user/1000/dbus-session"
	uid := func() (string, error) { return "1000", nil }
	noUser := func() (string, error) { return "", errors.New("user: unknown userid 1000") }
	files := func(contents map[string]string) (func(string) bool, func(string) ([]byte, error)) {
		exists := func(p string) bool { _, ok := contents[p]; return ok }
		read := func(p string) ([]byte, error) {
			c, ok := contents[p]
			if !ok {
				return nil, os.ErrNotExist
			}
			if c == "unreadable" {
				return nil, os.ErrPermission
			}
			return []byte(c), nil
		}
		return exists, read
	}
	for _, tc := range []struct {
		name    string
		address string
		uid     func() (string, error)
		files   map[string]string
		found   bool
	}{
		{"unset and no file", "", uid, nil, false},
		{"autolaunch and no file", "autolaunch:", uid, nil, false},
		{"autolaunch and the bus socket", "autolaunch:", uid, map[string]string{bus: ""}, true},
		{"unset and the session file with an address", "", uid, map[string]string{session: "DBUS_SESSION_BUS_ADDRESS=unix:abstract=/tmp/dbus-x\n"}, true},
		{"the session file with a CRLF line end", "", uid, map[string]string{session: "DBUS_SESSION_BUS_ADDRESS=unix:path=/x\r\n"}, true},
		{"the session file with an address and no line end", "", uid, map[string]string{session: "DBUS_SESSION_BUS_ADDRESS=unix:path=/x"}, true},
		{"the session file is empty", "", uid, map[string]string{session: ""}, false},
		{"the session file holds only the variable name", "", uid, map[string]string{session: "DBUS_SESSION_BUS_ADDRESS=\n"}, false},
		{"the session file has no variable name first", "", uid, map[string]string{session: "# comment\nDBUS_SESSION_BUS_ADDRESS=unix:path=/x\n"}, false},
		{"the session file is unreadable", "", uid, map[string]string{session: "unreadable"}, false},
		{"the bus socket and an empty session file", "", uid, map[string]string{bus: "", session: ""}, true},
		{"a socket of another user", "", uid, map[string]string{"/run/user/1001/bus": ""}, false},
		{"the user cannot be looked up", "", noUser, map[string]string{bus: ""}, false},
		{"the user cannot be looked up and autolaunch", "autolaunch:", noUser, map[string]string{bus: ""}, false},
		{"an address and a user that cannot be looked up", "unix:path=/x", noUser, nil, true},
		{"an address", "unix:path=/x", uid, nil, true},
		{"disabled", "disabled:", uid, nil, true},
		{"autolaunch with a parameter", "autolaunch:scope=*install-path", uid, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exists, read := files(tc.files)
			found, cause := findSessionBus(tc.address, 1000, tc.uid, exists, read)
			assert.Equal(t, tc.found, found)
			if tc.found {
				assert.NoError(t, cause)
				return
			}
			require.ErrorIs(t, cause, errNoSessionBus)
			require.ErrorContains(t, cause, "DBUS_SESSION_BUS_ADDRESS gives no address")
		})
	}
}

// TestFindSessionBusCauseText asserts the text of the cause for a host with
// a user and for a user that cannot be looked up.
func TestFindSessionBusCauseText(t *testing.T) {
	none := func(string) bool { return false }
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	_, cause := findSessionBus("", 1000, func() (string, error) { return "1000", nil }, none, read)
	require.EqualError(t, cause, "no D-Bus session bus: DBUS_SESSION_BUS_ADDRESS gives no address, /run/user/1000/bus does not exist, and /run/user/1000/dbus-session names no address")

	lookup := errors.New("user: Current requires cgo or $USER set in environment")
	_, cause = findSessionBus("", 1000, func() (string, error) { return "", lookup }, none, read)
	require.EqualError(t, cause, "no D-Bus session bus found: DBUS_SESSION_BUS_ADDRESS gives no address, and /run/user/1000/bus and /run/user/1000/dbus-session were not checked because user ID 1000 could not be looked up: user: Current requires cgo or $USER set in environment")
	require.ErrorIs(t, cause, lookup)
	require.ErrorIs(t, cause, errNoSessionBus)
	require.ErrorIs(t, cause, errUserLookup)
}
