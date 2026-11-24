package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheConfigHelpRowNamesTheVariableTheSameWayOnEveryOS asserts that the
// row of each flag of the root that reads an environment variable names the
// variable as $NAME, whether or not PSHOME is set. Not parallel: it sets the
// process environment.
func TestTheConfigHelpRowNamesTheVariableTheSameWayOnEveryOS(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	checked := 0
	for _, pshome := range []string{"", `C:\Program Files\PowerShell\7`} {
		t.Setenv("PSHOME", pshome)
		for _, f := range root.Flags {
			if f.EnvVar == "" {
				continue
			}
			checked++
			assert.Equal(t, "--config string, -c string\tpath to the config directory [$RANCHER_CLI_CONFIG_DIR]", f.helpRow(), "PSHOME %q", pshome)
		}
	}
	assert.Positive(t, checked, "no flag of the root reads an environment variable")
}

// TestAHelpPageReportsAWriteErrorOfItsLastLines asserts that a write error of
// the last lines of a page, which the page's tab writer holds until the end,
// is returned. The page is one that ends in a flag row: the template writes a
// tab in a flag row, so the writer holds the row until the end of the page and
// the last write happens in the flush. A page that ended in a line with no tab
// would be written before the flush and would not test the claim.
func TestAHelpPageReportsAWriteErrorOfItsLastLines(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	leaf := root.Command("access").Command("list")
	require.NotNil(t, leaf)

	var counting scriptedErrWriter
	require.NoError(t, renderHelpPage(&counting, helpPageLeaf, leaf, style.Style{}))
	require.Positive(t, counting.calls)
	lines := strings.Split(strings.TrimSuffix(counting.buf.String(), "\n"), "\n")
	require.True(t, strings.HasPrefix(lines[len(lines)-1], "   --"), "the last line of the page is not a flag row: %q", lines[len(lines)-1])

	last := &scriptedErrWriter{failOnly: counting.calls}
	require.ErrorIs(t, renderHelpPage(last, helpPageLeaf, leaf, style.Style{}), errScriptedWrite)
}

// TestARenderOfAnUnknownPageWritesNothingAndReturnsAnError asserts that
// renderHelpPage writes nothing for a page value that is not the root, a group
// or a leaf, and returns an error.
func TestARenderOfAnUnknownPageWritesNothingAndReturnsAnError(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	err = renderHelpPage(&out, helpPage(0), root, style.Style{})
	require.ErrorContains(t, err, "internal error: the help page is not for the root, a group or a single command; no help was written")
	assert.Empty(t, out.String())
}
