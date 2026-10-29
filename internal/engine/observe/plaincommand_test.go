package observe

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// holdsMarkerRune reports whether s holds one of the five characters a
// remedy marker is made of, U+2060 to U+2064.
func holdsMarkerRune(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' })
}

// TestAManifestStatusWritesItsCommandPlain asserts the two texts the support
// bundle writes into its manifest, the status of a failed collector and the
// status of a target whose container logs were not all collected, hold the
// command a certificate failure names as the plain command and no character
// of the remedy marker: the manifest is in an archive that no invocation
// expands. Mutation: drop ExpandRemedies in either function.
func TestAManifestStatusWritesItsCommandPlain(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]string{
		"a failed collector": errorStatus(certificateFailure()),
		"a failed log":       logOutcome{containers: 2, failed: []string{certificateFailure().Error()}}.status(),
	} {
		assert.Contains(t, status, plainRemedy, name)
		assert.False(t, clierr.HasRemedy(status), name)
		assert.False(t, holdsMarkerRune(status), "%s: the status holds a character of the remedy marker: %q", name, status)
	}
}
