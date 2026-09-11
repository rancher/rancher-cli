//go:build e2e_fidelity

package fidelity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
)

// TestDownstream records or verifies the goldens for every probe marked
// Downstream: true, against the downstream cluster named by
// E2E_FIDELITY_DOWNSTREAM. scripts/e2e-fidelity imports a pinned k3s cluster into
// the oracle and sets the variable, so the goldens carry the oracle image
// pin; an external Rancher with its own downstream can be probed by hand
// through scripts/e2e-fidelity-downstream. Unset, the suite skips entirely, and
// TestOracle never covers these probes.
func TestDownstream(t *testing.T) {
	downstream := os.Getenv("E2E_FIDELITY_DOWNSTREAM")
	if downstream == "" {
		t.Skip("E2E_FIDELITY_DOWNSTREAM is unset; no downstream cluster to probe")
	}
	tgt, err := fidelity.NewOracleTargetFromEnv()
	require.NoError(t, err)
	// The member identity provisions a user and bindings on the oracle;
	// leave the server as it was found, on every exit path.
	t.Cleanup(func() {
		if err := tgt.Cleanup(context.Background()); err != nil {
			t.Errorf("cleaning up the fidelity identities: %v", err)
		}
	})

	image, err := recordedImage(t.Context(), tgt)
	require.NoError(t, err)

	dir := filepath.Join("..", "..", "..", "internal", "testrancher", "fidelity", "testdata", "downstream")
	for _, p := range fidelity.All() {
		if !p.Downstream {
			continue
		}
		t.Run(p.Name, func(t *testing.T) {
			results, err := fidelity.RunProbe(t.Context(), tgt, p)
			require.NoError(t, err)
			got := fidelity.Golden{OracleImage: image, Observations: results}
			path := fidelity.GoldenPath(dir, p.Name)

			if *update {
				require.NoError(t, fidelity.SaveGolden(path, got))
				return
			}
			want, err := fidelity.LoadGolden(path)
			require.NoError(t, err, "no downstream golden for %s: run make e2e-fidelity-downstream-update", p.Name)
			require.Equal(t, image, want.OracleImage,
				"golden was recorded against a different server; bump and re-record deliberately")
			assert.Empty(t, fidelity.Diff(want, got), "the downstream server drifted from its recorded golden")
		})
	}
}

// recordedImage names what a downstream golden was recorded against:
// E2E_FIDELITY_ORACLE_IMAGE when the caller set one, else the target's own
// /rancherversion Version prefixed "external:" - an external target has no
// digest-pinned image, so the server's reported version is the provenance a
// re-record against a different server must fail against.
func recordedImage(ctx context.Context, tgt *fidelity.OracleTarget) (string, error) {
	if img := os.Getenv("E2E_FIDELITY_ORACLE_IMAGE"); img != "" {
		return img, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.BaseURL()+"/rancherversion", nil)
	if err != nil {
		return "", fmt.Errorf("building /rancherversion request: %w", err)
	}
	resp, err := tgt.Client().Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching /rancherversion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading /rancherversion response: %w", err)
	}
	var body struct {
		Version string
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("decoding /rancherversion response: %w", err)
	}
	if body.Version == "" {
		return "", fmt.Errorf("/rancherversion response has no Version field")
	}
	return "external:" + body.Version, nil
}
