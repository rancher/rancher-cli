// Package dbus is a fixture for the keychain bus guard: it imports the D-Bus
// library under another name and holds a launching entry point without
// calling it.
package dbus

import bus "github.com/godbus/dbus/v5"

// Connect is a reference to an entry point that starts a session bus.
var Connect = bus.SessionBus
