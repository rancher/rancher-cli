package config

import "github.com/zalando/go-keyring"

// keyringOps is the seam over the operating system keyring. go-keyring
// exposes no provider injection, and its in-memory mock cannot model a
// write that commits and then reports an error, which is the case this
// package must classify; tests swap the seam.
type keyringOps interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type goKeyring struct{}

func (goKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (goKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (goKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

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
