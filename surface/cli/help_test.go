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

// renderHelp runs `rancher <path...> --help` through the real root and
// returns stdout. color, when non-empty, is passed as --color; env values
// are set via t.Setenv so callers can exercise NO_COLOR/CLICOLOR_FORCE.
func renderHelp(t *testing.T, color string, path ...string) string {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	argv := append([]string{"rancher"}, path...)
	if color != "" {
		argv = append(argv, "--color", color)
	}
	argv = append(argv, "--help")
	require.NoError(t, root.Run(t.Context(), argv))
	return out.String()
}

// TestStyledHelpGroupCardBoldsHeadersAndAlignsCommands renders the "auth"
// group card (SubcommandHelpTemplate: a command with visible subcommands)
// and asserts gh's shape: bold section headers, plain colon-aligned command
// rows, no bolding anywhere else.
func TestStyledHelpGroupCardBoldsHeadersAndAlignsCommands(t *testing.T) {
	t.Parallel()
	styled := renderHelp(t, "always", "auth")

	for _, header := range []string{"NAME:", "USAGE:", "COMMANDS:", "OPTIONS:", "GLOBAL OPTIONS:"} {
		assert.Contains(t, styled, style.Style{Enabled: true}.Bold(header), "header %q must be bold", header)
	}
	assert.Contains(t, styled, "   login:", "command rows are colon-aligned, not comma-joined")
	assert.NotContains(t, styled, "login, ", "the stock comma-joined command listing must not appear")

	loginLine := lineContaining(t, styled, "login:")
	assert.NotContains(t, loginLine, "\x1b[", "a command row's name and usage stay plain")
}

// TestStyledHelpLeafCardBoldsHeaders renders the "auth login" leaf card
// (CommandHelpTemplate: no visible subcommands) and asserts the same header
// bolding with no COMMANDS section (a leaf has none).
func TestStyledHelpLeafCardBoldsHeaders(t *testing.T) {
	t.Parallel()
	styled := renderHelp(t, "always", "auth", "login")

	for _, header := range []string{"NAME:", "USAGE:", "OPTIONS:", "GLOBAL OPTIONS:"} {
		assert.Contains(t, styled, style.Style{Enabled: true}.Bold(header), "header %q must be bold", header)
	}
	assert.NotContains(t, styled, "COMMANDS:", "a leaf card has no COMMANDS section")

	urlLine := lineContaining(t, styled, "--url")
	assert.NotContains(t, urlLine, "\x1b[", "a flag row's name and usage stay plain")
}

// TestStyledHelpRootCardBoldsHeaders renders the bare `rancher --help` card
// (RootCommandHelpTemplate: the one shape with VERSION and a colon-aligned
// COMMANDS list at the top level) and asserts the same header bolding the
// group and leaf cards get.
func TestStyledHelpRootCardBoldsHeaders(t *testing.T) {
	t.Parallel()
	styled := renderHelp(t, "always")

	for _, header := range []string{"NAME:", "USAGE:", "VERSION:", "COMMANDS:", "GLOBAL OPTIONS:"} {
		assert.Contains(t, styled, style.Style{Enabled: true}.Bold(header), "header %q must be bold", header)
	}
	nameLine := lineContaining(t, styled, "rancher - Rancher CLI")
	assert.NotContains(t, nameLine, "\x1b[", "the NAME line's content stays plain")

	versionLine := lineContaining(t, styled, "v-test")
	assert.NotContains(t, versionLine, "\x1b[", "the VERSION line's content stays plain")

	assert.Contains(t, styled, "   auth:", "top-level command rows are colon-aligned")
	authLine := lineContaining(t, styled, "auth:")
	assert.NotContains(t, authLine, "\x1b[", "a command row's name and usage stay plain")

	globalOptLine := lineContaining(t, styled, "--color")
	assert.Contains(t, globalOptLine, "auto|always|never", "the global options list includes --color's own row")
}

// TestStyledHelpDescriptionPrecedesUsage asserts the layout change: when a
// command carries a Description (the Prime marker is the one case that sets
// one today), it renders above USAGE, not between VERSION and AUTHOR.
func TestStyledHelpDescriptionPrecedesUsage(t *testing.T) {
	t.Parallel()
	plain := renderHelp(t, "", "get", "oidcclient")

	descAt := indexOf(t, plain, "DESCRIPTION:")
	usageAt := indexOf(t, plain, "USAGE:")
	assert.Less(t, descAt, usageAt, "DESCRIPTION: must render before USAGE:")
}

// TestStyledHelpStripInvariant asserts the content-invariant for a rendered
// help card: strip(styled) == plain byte-for-byte, and the styled render
// actually differs from plain (proving the wrap ran). Not parallel-safe: it
// sets the color env vars to a known-clean state for the plain baseline,
// which would race a sibling that also mutates them.
func TestStyledHelpStripInvariant(t *testing.T) {
	setNoColorEnv(t)
	plain := renderHelp(t, "", "auth")
	styled := renderHelp(t, "always", "auth")

	assert.NotEqual(t, plain, styled, "styled render must differ from plain")
	assert.Equal(t, plain, style.Strip(styled))
}

// TestStyledHelpNoColorWinsOverForceVar asserts NO_COLOR's precedence over a
// force variable under --color=auto (the same rule ActivateColor asserts
// directly): CLICOLOR_FORCE alone styles help output, but adding NO_COLOR
// suppresses it back to the byte-identical plain render. Not parallel-safe:
// it mutates the color env vars directly.
func TestStyledHelpNoColorWinsOverForceVar(t *testing.T) {
	setNoColorEnv(t)
	plain := renderHelp(t, "", "auth")

	t.Setenv("CLICOLOR_FORCE", "1")
	forced := renderHelp(t, "", "auth")
	assert.NotEqual(t, plain, forced, "CLICOLOR_FORCE alone must style the render")

	t.Setenv("NO_COLOR", "1")
	suppressed := renderHelp(t, "", "auth")
	assert.Equal(t, plain, suppressed, "NO_COLOR must win over CLICOLOR_FORCE and restore the plain render")
}

// TestColonOffsetCommandsAlignsOnLongestJoinedNames asserts the alignment
// arithmetic the COMMANDS block depends on: the returned width tracks the
// longest comma-joined "name, aliases" (plus a colon), so a command with
// aliases widens the column exactly as far as its full joined-names string
// requires.
func TestColonOffsetCommandsAlignsOnLongestJoinedNames(t *testing.T) {
	t.Parallel()
	cmds := []*ucli.Command{
		{Name: "a", Aliases: []string{"aaaaaaaaaa"}},
		{Name: "bb"},
	}
	assert.Equal(t, len("a, aaaaaaaaaa:")+5, colonOffsetCommands(cmds, 5), "the longest joined-names string must set the column width")
}

// TestStyledHelpCommandRowShowsAliases renders `rancher get --help` (a
// subcommand card whose leaves include kinds with declared aliases) and
// asserts that a leaf's full name list — canonical name plus every alias,
// comma-joined — renders before the colon, matching urfave's own stock
// comma-joined listing rather than the canonical name alone.
func TestStyledHelpCommandRowShowsAliases(t *testing.T) {
	t.Parallel()
	styled := renderHelp(t, "always", "get")

	assert.Contains(t, styled, "pod, pods, po:", "a leaf's declared aliases must render comma-joined before the colon")
}

// setNoColorEnv sets all three color-activating env vars to empty — "unset"
// per ActivateColor's own contract — so a "plain" (--color not passed)
// render is actually plain, regardless of what the test runner's ambient
// shell has set.
func setNoColorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("FORCE_COLOR", "")
}

func lineContaining(t *testing.T, text, substr string) string {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, text)
	return ""
}

func indexOf(t *testing.T, text, substr string) int {
	t.Helper()
	i := bytes.Index([]byte(text), []byte(substr))
	require.GreaterOrEqual(t, i, 0, "expected %q to appear in:\n%s", substr, text)
	return i
}
