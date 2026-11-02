// Package keyring is a fixture for the import guard: its closure holds the
// keychain package.
package keyring

import "github.com/zalando/go-keyring"

// Use is a use of the import.
var Use = keyring.Get
