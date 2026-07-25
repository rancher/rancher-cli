//go:build e2e_real

// Package real records and verifies the fidelity oracle goldens against a
// live Rancher booted by scripts/e2e-real. It never runs in make test or
// make e2e.
package real

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher/fidelity"
)

var update = flag.Bool("update", false, "record oracle goldens from the real server")

func TestOracle(t *testing.T) {
	tgt, err := fidelity.NewRealTargetFromEnv()
	require.NoError(t, err)
	oracleImage := os.Getenv("E2E_REAL_ORACLE_IMAGE")
	require.NotEmpty(t, oracleImage, "E2E_REAL_ORACLE_IMAGE must be set (run via scripts/e2e-real)")

	dir := filepath.Join("..", "..", "internal", "testrancher", "fidelity", "testdata", "oracle")
	for _, p := range fidelity.All() {
		t.Run(p.Name, func(t *testing.T) {
			results, err := fidelity.RunProbe(context.Background(), tgt, p)
			require.NoError(t, err)
			got := fidelity.Golden{OracleImage: oracleImage, Observations: results}
			path := fidelity.GoldenPath(dir, p.Name)

			if *update {
				require.NoError(t, fidelity.SaveGolden(path, got))
				return
			}
			want, err := fidelity.LoadGolden(path)
			require.NoError(t, err, "no oracle golden for %s: run make e2e-real-update", p.Name)
			require.Equal(t, oracleImage, want.OracleImage,
				"golden was recorded against a different oracle image; bump and re-record deliberately")
			assert.Empty(t, fidelity.Diff(want, got), "the real server drifted from its recorded golden")
		})
	}
}
