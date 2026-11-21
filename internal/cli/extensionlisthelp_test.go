package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtensionListShowHelpHasNoPlaceholder asserts the rendered help of
// extension list prints the --show line with the flag's type and its text
// that names the three values and the default, and not a backticked word in
// the text turned into the flag's value placeholder.
func TestExtensionListShowHelpHasNoPlaceholder(t *testing.T) {
	stdout, stderr, code := runBounded(t, newRoot(t), []string{"extension", "list", "--help"})
	require.Zero(t, code, stderr)
	var lines []string
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.Contains(line, "--show") {
			lines = append(lines, strings.Join(strings.Fields(line), " "))
		}
	}
	require.Equal(t, []string{"--show string which extensions to list: all, installed or available (default all)"}, lines)
	assert.NotContains(t, stdout, "--show all")
	assert.NotContains(t, stdout, "`")
}
