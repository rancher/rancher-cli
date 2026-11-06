//go:build e2e_fidelity

package fidelity_test

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
)

// runProbeOrSkip runs p against tgt for TestOracle, TestDownstream and the
// drift run. It is the only function in this package that runs a probe. When
// the server's minor is newer than the probe's MaxMinor, a drift run records
// the probe under suite with the drift collector, then the test is skipped as
// not checked. The probe's result is not saved, compared or counted as a
// failure. Any other error is returned for the caller to handle. opts is the
// zero value outside a drift run.
func runProbeOrSkip(t *testing.T, suite string, tgt fidelity.Target, p fidelity.Probe, opts fidelity.RunOptions) ([]fidelity.ObservationResult, error) {
	t.Helper()
	var drift *fidelity.DriftCollector
	if driftConfig != nil {
		drift = driftCollector
	}
	return fidelity.RunProbeOrSkip(t.Context(), t, tgt, p, opts, drift, suite)
}
