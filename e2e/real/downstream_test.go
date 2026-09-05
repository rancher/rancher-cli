//go:build e2e_real

package real

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
// E2E_REAL_DOWNSTREAM. It skips entirely when that variable is unset: the
// pinned Docker oracle (scripts/e2e-real) has no downstream cluster, so this
// suite never runs there and TestOracle never covers these probes.
func TestDownstream(t *testing.T) {
	downstream := os.Getenv("E2E_REAL_DOWNSTREAM")
	if downstream == "" {
		t.Skip("E2E_REAL_DOWNSTREAM is unset; no downstream cluster to probe")
	}
	tgt, err := fidelity.NewRealTargetFromEnv()
	require.NoError(t, err)

	image, err := recordedImage(t.Context(), tgt)
	require.NoError(t, err)

	dir := filepath.Join("..", "..", "internal", "testrancher", "fidelity", "testdata", "downstream")
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
			require.NoError(t, err, "no downstream golden for %s: run make e2e-real-downstream-update", p.Name)
			require.Equal(t, image, want.OracleImage,
				"golden was recorded against a different server; bump and re-record deliberately")
			assert.Empty(t, fidelity.Diff(want, got), "the downstream server drifted from its recorded golden")
		})
	}
}

// recordedImage names what a downstream golden was recorded against:
// E2E_REAL_ORACLE_IMAGE when the caller set one, else the target's own
// /rancherversion Version prefixed "external:" - an external target has no
// digest-pinned image, so the server's reported version is the provenance a
// re-record against a different server must fail against.
func recordedImage(ctx context.Context, tgt *fidelity.RealTarget) (string, error) {
	if img := os.Getenv("E2E_REAL_ORACLE_IMAGE"); img != "" {
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
