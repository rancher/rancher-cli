//go:build (dragonfly && cgo) || (freebsd && cgo) || linux || netbsd || openbsd

package config

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
)

func init() {
	sessionBusAddressFound = func() (bool, error) {
		return findSessionBus(os.Getenv(sessionBusEnv), os.Getuid(), currentUID, pathExists, readFile)
	}
}

func currentUID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Uid, nil
}

// pathExists is false only for a path that does not exist, as the D-Bus
// library reads it: a path it cannot stat counts as there.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// findSessionBus reports whether the D-Bus library finds a session bus
// address without starting a bus, by the rules of getSessionBusAddress in
// github.com/godbus/dbus/v5 conn.go and tryDiscoverDbusSessionBusAddress in
// conn_other.go. The library finds an address, and starts no bus, in these
// cases; in every other case it starts one through dbus-launch:
//   - DBUS_SESSION_BUS_ADDRESS is set to anything but empty or exactly
//     "autolaunch:";
//   - or the user is known and /run/user/<uid>/bus exists;
//   - or the user is known and /run/user/<uid>/dbus-session exists, can be
//     read, and starts with "DBUS_SESSION_BUS_ADDRESS=" followed by an
//     address.
//
// A user that cannot be looked up gives no discovery; processUID, the
// process's own user ID, only fills the cause's text. The check does not
// connect.
func findSessionBus(address string, processUID int, uid func() (string, error), exists func(string) bool, read func(string) ([]byte, error)) (bool, error) {
	if address != "" && address != "autolaunch:" {
		return true, nil
	}
	id, err := uid()
	if err != nil {
		return false, &userLookupCause{
			text: fmt.Sprintf("no D-Bus session bus found: %s gives no address, and /run/user/%d/bus and /run/user/%d/dbus-session were not checked because user ID %d could not be looked up: %s",
				sessionBusEnv, processUID, processUID, processUID, err),
			lookup: err,
		}
	}
	bus := "/run/user/" + id + "/bus"
	session := "/run/user/" + id + "/dbus-session"
	if exists(bus) {
		return true, nil
	}
	if exists(session) {
		if data, err := read(session); err == nil {
			if addr, ok := bytes.CutPrefix(data, []byte("DBUS_SESSION_BUS_ADDRESS=")); ok && len(bytes.TrimRight(addr, "\n\r")) > 0 {
				return true, nil
			}
		}
	}
	return false, fmt.Errorf("%w: %s gives no address, %s does not exist, and %s names no address",
		errNoSessionBus, sessionBusEnv, bus, session)
}
