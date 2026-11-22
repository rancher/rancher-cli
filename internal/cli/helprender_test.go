package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
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
			if df, ok := f.(ucli.DocGenerationFlag); !ok || len(df.GetEnvVars()) == 0 {
				continue
			}
			checked++
			assert.Equal(t, "--config string, -c string\tpath to the config directory [$RANCHER_CLI_CONFIG_DIR]", helpFlagRow(f), "PSHOME %q", pshome)
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

// TestTheHelpPrinterWritesNothingForATemplateItDoesNotKnow asserts that the
// help printer writes nothing for a template that is none of the three help
// templates and keeps an error for the run of the root.
func TestTheHelpPrinterWritesNothingForATemplateItDoesNotKnow(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	printStyledHelp(&out, "NAME: {{.Name}}", root)
	assert.Empty(t, out.String())
	require.ErrorContains(t, takeHelpWriteError(root), "internal error: the help template is not one of the three the CLI renders; no help was written")
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

// TestEveryCommandAndFlagFitsTheHelpRenderer asserts that no command of the
// tree sets what the help templates leave out (UsageText, Category, Authors,
// Copyright, Arguments, CustomHelpTemplate or ExtraInfo),
// that no flag sets a category or shows a default value, and that every flag
// is a bool, string or string slice flag, the only kinds the flag row
// handles. The walk reads the tree before any run: urfave adds its own
// --help and --version flags when a command runs, and the walk does not see
// them.
func TestEveryCommandAndFlagFitsTheHelpRenderer(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	commands, flags := 0, 0
	seen := map[ucli.Flag]bool{}
	var walk func(cmd *ucli.Command)
	walk = func(cmd *ucli.Command) {
		commands++
		assert.Empty(t, cmd.UsageText, "%s sets UsageText", cmd.FullName())
		assert.Empty(t, cmd.Category, "%s sets Category", cmd.FullName())
		assert.Empty(t, cmd.Authors, "%s sets Authors", cmd.FullName())
		assert.Empty(t, cmd.Copyright, "%s sets Copyright", cmd.FullName())
		assert.Empty(t, cmd.Arguments, "%s sets Arguments", cmd.FullName())
		assert.Empty(t, cmd.CustomHelpTemplate, "%s sets CustomHelpTemplate", cmd.FullName())
		assert.Nil(t, cmd.ExtraInfo, "%s sets ExtraInfo", cmd.FullName())
		for _, f := range append(append([]ucli.Flag{}, cmd.Flags...), leafFlags(cmd)...) {
			if seen[f] {
				continue
			}
			seen[f] = true
			flags++
			name := cmd.FullName() + " --" + f.Names()[0]
			switch f.(type) {
			case *ucli.BoolFlag, *ucli.StringFlag, *ucli.StringSliceFlag:
			default:
				assert.Failf(t, "unexpected flag type", "%s is a %T", name, f)
			}
			if cf, ok := f.(ucli.CategorizableFlag); ok {
				assert.Empty(t, cf.GetCategory(), "%s sets a category", name)
			}
			if df, ok := f.(ucli.DocGenerationFlag); ok {
				assert.False(t, df.IsDefaultVisible() && (df.GetDefaultText() != "" || (df.TakesValue() && df.GetValue() != "")), "%s shows a default value", name)
			}
		}
		for _, sub := range cmd.Commands {
			walk(sub)
		}
	}
	walk(root)
	assert.Positive(t, commands, "the walk found no command")
	assert.Positive(t, flags, "the walk found no flag")
	t.Logf("checked %d commands and %d flags", commands, flags)
}
