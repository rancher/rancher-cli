// Package token is a fixture for the import guard: it imports the token
// package, which a package that reads no credential must not.
package token

import "github.com/rancher/rancher-cli/internal/token"

// Use is a use of the import.
var Use = token.AcquireLoginCredential
