//go:build e2e_fidelity

package fidelity_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
)

// recordDriftTest records a test or subtest, and whether it failed, with the
// drift collector when it ends. It is the first statement of every test that
// takes part in a drift run; a drift run lists a failed test as a harness
// failure. It does nothing outside drift mode.
func recordDriftTest(t *testing.T) {
	t.Helper()
	if driftConfig == nil {
		return
	}
	t.Cleanup(func() { driftCollector.Test(t.Name(), t.Failed()) })
}

// runDriftProbe runs one probe in drift mode and hands its differences from
// the golden under dir, and its error, to the collector. Nothing is asserted
// about the answer: a difference or a probe error is data for the report. A
// missing golden, or one recorded against another image than the community
// goldens', fails the test.
func runDriftProbe(t *testing.T, suite string, tgt fidelity.Target, p fidelity.Probe, dir string) {
	t.Helper()
	want, err := fidelity.LoadGolden(fidelity.GoldenPath(dir, p.Name))
	require.NoError(t, err, "no golden for %s", p.Name)
	require.Equal(t, driftConfig.GoldenImage, want.OracleImage,
		"golden was recorded against a different image than the community goldens' image")
	results, runErr := fidelity.RunProbeWith(t.Context(), tgt, p, fidelity.RunOptions{KeepLastAnswer: true})
	got := fidelity.Golden{OracleImage: driftConfig.OracleImage, Observations: results}
	diffs := fidelity.Differences(p.Name, want, got, runErr == nil)
	driftCollector.Probe(suite, p.Name, p.Downstream, diffs, runErr)
	if runErr != nil {
		t.Logf("probe error: %v", runErr)
	}
	for _, d := range diffs {
		t.Logf("difference %s", d)
	}
}
