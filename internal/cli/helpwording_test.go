package cli

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flagRowNames returns the names of a flag as its help row writes them: -x for
// a name of one character and --name for the others, separated by ", ".
func flagRowNames(f *Flag) string {
	var names []string
	for _, name := range f.Names() {
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) == 1 {
			names = append(names, "-"+name)
		} else {
			names = append(names, "--"+name)
		}
	}
	return strings.Join(names, ", ")
}

// boolRowUsage returns the usage a bool flag's help row prints: the usage as
// written, followed by " [$NAME]" when the flag reads an environment variable.
func boolRowUsage(f *Flag) string {
	usage := strings.TrimSpace(f.Usage)
	if f.EnvVar != "" {
		usage += " [$" + f.EnvVar + "]"
	}
	return usage
}

// TestABoolFlagHelpRowShowsNoPlaceholder asserts that the help row of every bool
// flag of every command, the root's global flags and the built-in help and
// version flags included, shows its names alone before the tab, on every name,
// and prints its usage as written, backticks included, followed by the
// environment variable when the flag reads one; that a hand-built bool flag
// with a backtick pair in its usage keeps the pair in the row; that a
// hand-built bool flag with an environment variable names it after its usage;
// and that a
// slice flag keeps its placeholder and its names repeated in brackets.
func TestABoolFlagHelpRowShowsNoPlaceholder(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	bools := 0
	var walk func(c *Command)
	walk = func(c *Command) {
		for _, f := range c.Flags {
			if f.Kind != boolFlag {
				continue
			}
			bools++
			names, usage, found := strings.Cut(f.helpRow(), "\t")
			require.True(t, found, "the row of --%s has a tab", f.Name)
			assert.Equal(t, flagRowNames(f), names, "the row of --%s of %q", f.Name, c.FullName())
			assert.Equal(t, boolRowUsage(f), usage, "the usage of --%s of %q prints as written", f.Name, c.FullName())
		}
		for _, sub := range c.Commands {
			walk(sub)
		}
	}
	walk(root)
	assert.Positive(t, bools)

	aliased := &Flag{Name: "x", Aliases: []string{"yes"}, Kind: boolFlag, Usage: "also `rancher get x` works"}
	assert.Equal(t, "-x, --yes\talso `rancher get x` works", aliased.helpRow())
	withEnv := &Flag{Name: "quiet", Kind: boolFlag, EnvVar: "RANCHER_QUIET", Usage: "print less"}
	assert.Equal(t, "--quiet\tprint less [$RANCHER_QUIET]", withEnv.helpRow())
	assert.Equal(t, boolRowUsage(withEnv), "print less [$RANCHER_QUIET]")
	slice := &Flag{Name: "label", Kind: sliceFlag, Usage: "a `key=value` label"}
	assert.Equal(t, "--label key=value [ --label key=value ]\ta key=value label", slice.helpRow())
	text := &Flag{Name: "name", Kind: stringFlag, Usage: "the name"}
	assert.Equal(t, "--name string\tthe name", text.helpRow())
}

var (
	flagRowLine    = regexp.MustCompile(`^   -`)
	sectionHeading = regexp.MustCompile(`^[A-Z][A-Z ]*:$`)
)

// TestHelpPagesSayFlags asserts that no help page has a line OPTIONS: or
// GLOBAL OPTIONS: or the word options in its USAGE line, that every row of a
// flag sits under a FLAGS: or GLOBAL FLAGS: header, that every page lists
// flags, and that the root page has the GLOBAL FLAGS: header. Not parallel: it
// sets the process environment and the captured command line.
func TestHelpPagesSayFlags(t *testing.T) {
	home, work := goldenEnvironment(t)
	commands := goldenCommands(t)
	pages := recordHelp(t, commands)
	require.Len(t, pages, len(commands))

	for name, page := range pages {
		lines := strings.Split(page, "\n")
		section, rows := "", 0
		for i, line := range lines {
			assert.NotContains(t, []string{"OPTIONS:", "GLOBAL OPTIONS:"}, line, "the help of %s has the header %q", name, line)
			if sectionHeading.MatchString(line) {
				section = line
				if line == "USAGE:" {
					require.Less(t, i+1, len(lines), name)
					assert.NotContains(t, strings.ToLower(lines[i+1]), "options", "the USAGE line of the help of %s", name)
				}
			}
			if flagRowLine.MatchString(line) {
				rows++
				assert.Contains(t, []string{"FLAGS:", "GLOBAL FLAGS:"}, section, "the flag row %q of the help of %s", line, name)
			}
		}
		assert.Positive(t, rows, "the help of %s lists flags", name)
	}
	assert.Contains(t, pages[goldenName(nil)], "\nGLOBAL FLAGS:\n", "the root page has GLOBAL FLAGS:")
	assertUntouched(t, home, work)
}
