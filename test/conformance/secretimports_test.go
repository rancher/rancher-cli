package conformance

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// secretDir is the package of the type of a secret value.
const secretDir = "internal/secret"

// secretImportAllowlist is every import path the package's production files
// may name: standard library packages. The package is a leaf that every layer
// can import; it reaches no other package of the module.
var secretImportAllowlist = map[string]bool{
	"fmt": true, "io": true, "log/slog": true, "strings": true,
}

// TestSecretImportsOnlyTheStandardLibrary asserts the production files of
// internal/secret import only the allowlisted standard library packages and
// nothing of the module: every layer can import it without a cycle.
func TestSecretImportsOnlyTheStandardLibrary(t *testing.T) {
	t.Parallel()
	assert.Empty(t, scanImports(t, secretDir, secretImportAllowlist))
}
