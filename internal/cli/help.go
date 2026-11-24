package cli

import (
	"io"
	"os"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// firstErrorWriter passes every write to w and keeps the first error.
type firstErrorWriter struct {
	w   io.Writer
	err error
}

func (f *firstErrorWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

// primeMark returns the plain (uncolored) "[Prime] " prefix for a Prime
// command's usage text, or "" for an ordinary one. It stays uncolored so the
// COMMANDS block's wrap computes column widths off plain text length, same
// as any other row - coloring happens afterward, in colorPrimeMark.
func primeMark(cmd *Command) string {
	if commandIsPrime(cmd) {
		return runtime.PrimeMarker + " "
	}
	return ""
}

// colorPrimeMark colors the leading "[Prime]" token green in a row already
// wrapped by primeMark+wrap, leaving every other row byte-identical. Usage
// text never carries the literal token (an invariant enforced elsewhere), so
// the first occurrence in a Prime row's wrapped text is always the marker.
func colorPrimeMark(s style.Style, cmd *Command, wrapped string) string {
	if !commandIsPrime(cmd) {
		return wrapped
	}
	return runtime.ColorPrimeMarker(wrapped, s)
}

// colorPrimeDescription colors a Prime command's DESCRIPTION block entirely
// green, under the same activation rule as every other styled render; an
// ordinary command's Description is always empty, so wrapped is only ever
// non-empty here for a Prime command, but the check stays explicit rather
// than relying on that.
func colorPrimeDescription(s style.Style, cmd *Command, wrapped string) string {
	if !commandIsPrime(cmd) {
		return wrapped
	}
	return s.Green(wrapped)
}

// helpStyle mirrors internal/runtime's stylesFor for the OUT stream, without
// its format gate: a help render is always plain text, never routed through
// the output/json/yaml pipeline, so no format check applies here.
func helpStyle(cmd *Command, in *Input) style.Style {
	root := cmd.Root()
	return style.Style{Enabled: runtime.ActivateColor(
		in.String("color"),
		os.Getenv("NO_COLOR"),
		os.Getenv("CLICOLOR_FORCE"),
		os.Getenv("FORCE_COLOR"),
		fileIsTTY(root.Writer),
	)}
}

// errStyle is helpStyle's twin for the ERR stream, styling errors the
// adapter renders itself - before or outside the runtime pipeline, which
// resolves its own styles. colorValue is passed in rather than read off cmd
// because two callers fire before this invocation's flags have parsed and
// must scan raw argv for it. No format gate here: RenderErrorStyled already
// zeroes the style for non-text formats.
func errStyle(cmd *Command, colorValue string) style.Style {
	return style.Style{Enabled: runtime.ActivateColor(
		colorValue,
		os.Getenv("NO_COLOR"),
		os.Getenv("CLICOLOR_FORCE"),
		os.Getenv("FORCE_COLOR"),
		fileIsTTY(cmd.Root().ErrWriter),
	)}
}

// colonOffsetCommands returns the left-column width for a colon-aligned
// COMMANDS list: the longest comma-joined "names:" plus fixed. It sizes the
// column on every comma-joined alias rather than the canonical name alone.
func colonOffsetCommands(cmds []*Command, fixed int) int {
	longest := 0
	for _, c := range cmds {
		if n := len(strings.Join(c.Names(), ", ")) + 1; n > longest {
			longest = n
		}
	}
	return longest + fixed
}

// sortedCommands returns the commands ordered by canonical name. Help lists
// commands alphabetically so a reader can scan for one; the command tree
// itself, and the schema manifest that mirrors it, keep registration
// order, so this sorts a copy and never the tree.
func sortedCommands(cmds []*Command) []*Command {
	out := slices.Clone(cmds)
	slices.SortFunc(out, func(a, b *Command) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// sortedFlags returns the flags ordered by their first (long) name, so
// OPTIONS and GLOBAL OPTIONS read alphabetically. A copy: the command's
// own flag slice keeps its declaration order.
func sortedFlags(flags []*Flag) []*Flag {
	out := slices.Clone(flags)
	slices.SortFunc(out, func(a, b *Flag) int { return strings.Compare(a.Names()[0], b.Names()[0]) })
	return out
}

// leafFlags is a leaf's OPTIONS: its own flags, plus --project with what it
// does for this command when the command takes it. --project is parsed as
// the global flag, so it is never one of the leaf's own flags.
func leafFlags(cmd *Command) []*Flag {
	flags := cmd.VisibleFlags()
	if desc, ok := commandProject(cmd); ok {
		flags = append(flags, &Flag{Name: runtime.FlagProject, Kind: stringFlag, Usage: desc})
	}
	return flags
}

// leafGlobalFlags is a command's GLOBAL OPTIONS: the global flags the command
// accepts, without --project (a command that takes it lists it under
// OPTIONS, and every other command refuses it).
func leafGlobalFlags(cmd *Command) []*Flag {
	return slices.DeleteFunc(slices.Clone(cmd.VisiblePersistentFlags()), func(f *Flag) bool {
		name := f.Names()[0]
		return name == runtime.FlagProject || !commandAcceptsGlobalFlag(cmd, name)
	})
}

// groupGlobalFlags is a command group's GLOBAL OPTIONS: the global flags a
// command under the group accepts, --project included when one of them
// takes it.
func groupGlobalFlags(cmd *Command) []*Flag {
	return slices.DeleteFunc(slices.Clone(cmd.VisiblePersistentFlags()), func(f *Flag) bool {
		return !commandAcceptsGlobalFlag(cmd, f.Names()[0])
	})
}

// commandsBlock is the colon-aligned, alphabetical COMMANDS list shared by
// rootCommandHelpTemplate and subcommandHelpTemplate - the two shapes that
// ever show one.
const commandsBlock = `{{ $cmds := sortedCommands .VisibleCommands }}{{ $cv := colonOffsetCommands $cmds 5 }}{{range $cmds}}
   {{$s := printf "%s:" (join .Names ", ")}}{{$s}}{{ $sp := subtract $cv (offset $s 3) }}{{ indent $sp ""}}{{colorPrimeMark . (wrap (print (primeMark .) .Usage) $cv)}}{{end}}`

// flagsBlock is the list of the visible flags of a command in alphabetical
// order, one row each.
const flagsBlock = `{{range $i, $e := sortedFlags .VisibleFlags}}
   {{wrap (flagRow $e) 6}}{{end}}`

const leafFlagsBlock = `{{range $i, $e := sortedFlags (leafFlags .)}}
   {{wrap (flagRow $e) 6}}{{end}}`

const leafGlobalFlagsBlock = `{{range $i, $e := sortedFlags (leafGlobalFlags .)}}
   {{wrap (flagRow $e) 6}}{{end}}`

const groupGlobalFlagsBlock = `{{range $i, $e := sortedFlags (groupGlobalFlags .)}}
   {{wrap (flagRow $e) 6}}{{end}}`

// rootCommandHelpTemplate is the page of the root command: bold section
// headers, the DESCRIPTION block above USAGE, and a colon-aligned COMMANDS
// list.
var rootCommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{.FullName}} {{if .VisibleFlags}}[global options]{{end}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{end}}{{if .Version}}

{{bold "VERSION:"}}
   {{.Version}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlags}}

{{bold "GLOBAL OPTIONS:"}}` + flagsBlock + `{{end}}
`

// commandHelpTemplate is the page of a leaf (a command with no visible
// subcommands): bold section headers, the DESCRIPTION block above USAGE, and
// --project listed under OPTIONS by the commands that take it and nowhere by
// the rest.
var commandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{template "usageTemplate" .}}{{if leafFlags .}}

{{bold "OPTIONS:"}}` + leafFlagsBlock + `{{end}}{{if leafGlobalFlags .}}

{{bold "GLOBAL OPTIONS:"}}` + leafGlobalFlagsBlock + `{{end}}
`

// subcommandHelpTemplate is the page of a group (a command with visible
// subcommands): bold section headers, the DESCRIPTION block above USAGE, and a
// colon-aligned COMMANDS list.
var subcommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{.FullName}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlags}}

{{bold "OPTIONS:"}}` + flagsBlock + `{{end}}{{if groupGlobalFlags .}}

{{bold "GLOBAL OPTIONS:"}}` + groupGlobalFlagsBlock + `{{end}}
`
