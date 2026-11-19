// Package keyringcall is a fixture for the keychain bus guard: it calls the
// keychain package directly, past the seam of internal/config.
package keyringcall

import "github.com/zalando/go-keyring"

// Read is a direct keychain read.
func Read() (string, error) { return keyring.Get("service", "user") }
