// Package telemetry is a fixture for the import guard: it imports the
// telemetry package, which only some packages may.
package telemetry

import "github.com/rancher/rancher-cli/internal/telemetry"

// Version is a use of the import.
func Version() string { return telemetry.ContractVersion }
