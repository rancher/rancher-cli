// Package aliased is a fixture for the telemetry command scan: an alias hides
// the package name of a call the scan refuses.
package aliased

import c "github.com/rancher/rancher-cli/internal/config"

// Open loads the config and the credential store through an alias.
func Open(dir string) error {
	_, _, err := c.LoadAndStore(dir, false)
	return err
}
