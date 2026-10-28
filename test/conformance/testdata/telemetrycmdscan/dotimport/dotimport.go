// Package dotimport is a fixture for the telemetry command scan: a dot import
// hides every selector.
package dotimport

import . "github.com/rancher/rancher-cli/internal/config"

// Open loads the config and the credential store.
func Open(dir string) error {
	_, _, err := LoadAndStore(dir, false)
	return err
}
