//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFidelityDriftRefusesUpdate asserts that the e2e_fidelity test binary,
// started with the six drift variables and -update, stops with the refusal
// before it runs any test. It reaches no server: the run selects no test, the
// variables name no real server, and every inherited E2E_FIDELITY variable
// is dropped from the child's environment.
func TestFidelityDriftRefusesUpdate(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	dir := t.TempDir()

	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "E2E_FIDELITY_") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"E2E_FIDELITY_MINOR=2.16",
		"E2E_FIDELITY_DRIFT_REPORT="+filepath.Join(dir, "report.json"),
		"E2E_FIDELITY_DRIFT_KNOWN="+filepath.Join(dir, "v2.16.json"),
		"E2E_FIDELITY_DRIFT_STARTED=2026-10-20T19:00:00Z",
		"E2E_FIDELITY_GOLDEN_IMAGE=rancher/rancher:golden@sha256:aa",
		"E2E_FIDELITY_GOLDEN_K3S_IMAGE=rancher/k3s:golden@sha256:bb",
		"E2E_FIDELITY_DOWNSTREAM_IMAGE=rancher/k3s:next@sha256:cc",
	)
	cmd := exec.CommandContext(t.Context(), "go", "test", "-tags", "e2e_fidelity", "-run", "^$", "./test/e2e/fidelity/", "-update")
	cmd.Dir = root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the test binary must refuse to start: %s", out)
	assert.Contains(t, string(out), "drift mode compares only; it never records goldens (-update is set)")
}
