package fidelity

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseScriptImagePin extracts a NAME="..." assignment from scripts/e2e-fidelity.
// Anchoring on line start and tolerating surrounding whitespace around "="
// keeps the match from drifting onto the prose comments above the two image
// pins, while still catching a reformatted literal.
func parseScriptImagePin(t *testing.T, script, name string) string {
	t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`(?m)^\s*%s\s*=\s*"([^"]*)"\s*$`, regexp.QuoteMeta(name)))
	m := re.FindStringSubmatch(script)
	require.NotEmptyf(t, m, "scripts/e2e-fidelity: no %s=\"...\" assignment found - the pin literal moved or was removed", name)
	return m[1]
}

// TestOracleImagePinsMatchGoldens asserts scripts/e2e-fidelity's community
// ORACLE_IMAGE pin equals the oracleImage recorded in every golden that
// carries one, and that PRIME_ORACLE_IMAGE stays distinct from it and
// pointed at the Prime registry. A community pin bump with no matching
// golden re-record fails this test instead of shipping silently on a PR.
func TestOracleImagePinsMatchGoldens(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "..", "scripts", "e2e-fidelity")
	raw, err := os.ReadFile(scriptPath)
	require.NoError(t, err)
	script := string(raw)

	oracleImage := parseScriptImagePin(t, script, "ORACLE_IMAGE")
	primeImage := parseScriptImagePin(t, script, "PRIME_ORACLE_IMAGE")

	assert.NotEqual(t, oracleImage, primeImage, "PRIME_ORACLE_IMAGE must differ from ORACLE_IMAGE")
	assert.True(t, strings.HasPrefix(primeImage, "registry.rancher.com/"),
		"PRIME_ORACLE_IMAGE %q must come from registry.rancher.com - no goldens exist yet to pin it against, so this is the whole check", primeImage)
	// The downstream the oracle imports is a second reviewed pin: the
	// access-downstream goldens hold for this k3s image only.
	k3sImage := parseScriptImagePin(t, script, "K3S_IMAGE")
	assert.True(t, strings.HasPrefix(k3sImage, "rancher/k3s:"), "K3S_IMAGE %q must be a rancher/k3s tag", k3sImage)
	assert.Contains(t, k3sImage, "@sha256:", "K3S_IMAGE %q must be digest-pinned like ORACLE_IMAGE", k3sImage)

	checked := 0
	err = filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}
		g, loadErr := LoadGolden(path)
		require.NoError(t, loadErr, path)
		if g.OracleImage == "" {
			// Goldens recorded against a synthetic target (e.g. the facade)
			// carry no oracle provenance.
			return nil
		}
		checked++
		assert.Equal(t, oracleImage, g.OracleImage, "%s: oracleImage does not match scripts/e2e-fidelity's ORACLE_IMAGE pin", path)
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, checked, "no golden under testdata carried a non-empty oracleImage - the walk or the field name drifted")
}
