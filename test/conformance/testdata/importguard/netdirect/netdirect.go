// Package netdirect is a fixture for the import guard: it imports a
// network package of the standard library directly.
package netdirect

import "net"

// Loopback is a use of the import.
func Loopback() bool { return net.ParseIP("127.0.0.1").IsLoopback() }
