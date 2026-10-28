// Package loadandstore is a fixture for the telemetry command scan: it calls
// the loader that also opens the credential store.
package loadandstore

import "github.com/rancher/rancher-cli/internal/config"

// Open loads the config and the credential store.
func Open(dir string) error {
	_, _, err := config.LoadAndStore(dir, false)
	return err
}
