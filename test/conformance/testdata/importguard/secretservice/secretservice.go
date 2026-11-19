// Package secretservice is a fixture for the keychain bus guard: it imports
// the Secret Service package of go-keyring, which connects to the session bus
// without the check of internal/config.
package secretservice

import "github.com/zalando/go-keyring/secret_service"

// Connect is a reference to the connecting constructor.
var Connect = secret_service.NewSecretService
