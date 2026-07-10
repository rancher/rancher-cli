package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigratingCoversLegacyInventory keeps MIGRATING.md's reconciliation
// honest on every clean clone: legacy-commands.txt is a tracked transcription
// of the gitignored legacy command inventory (docs/design/legacy-command-inventory.md),
// so this test never reads that file directly.
func TestMigratingCoversLegacyInventory(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must resolve this file's path")
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	migrating, err := os.ReadFile(filepath.Join(repoRoot, "MIGRATING.md"))
	require.NoError(t, err)

	legacy, err := os.ReadFile(filepath.Join(repoRoot, "internal", "commands", "testdata", "legacy-commands.txt"))
	require.NoError(t, err)

	for _, line := range strings.Split(strings.TrimSpace(string(legacy)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		require.Contains(t, string(migrating), line, "legacy command %q must appear verbatim in MIGRATING.md", line)
	}
}
