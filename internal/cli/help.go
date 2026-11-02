package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// init wires the styled help templates once, package-wide: every help
// render path in urfave/cli/v3 (root, group, and leaf) funnels through the
// single HelpPrinter var, so overriding it here covers all three shapes.
func init() {
	ucli.HelpPrinter = printStyledHelp
	ucli.VersionPrinter = printVersion
	ucli.RootCommandHelpTemplate = rootCommandHelpTemplate
	ucli.CommandHelpTemplate = commandHelpTemplate
	ucli.SubcommandHelpTemplate = subcommandHelpTemplate
	ucli.ShowCommandHelp = showCommandHelp
}

// visibleSubcommand returns the visible subcommand of node named word
// (by name or alias), or nil. It is the one lookup behind every level of
// the `--help <words>` walk and behind the hint for a mistyped `help`.
func visibleSubcommand(node *ucli.Command, word string) *ucli.Command {
	for _, sub := range node.VisibleCommands() {
		if sub.HasName(word) {
			return sub
		}
	}
	return nil
}

// showCommandHelp is urfave's seam for "show help for the subcommand
// named name under cmd", reached by `<cmd> --help [words]`, by
// `<cmd> <sub> --help` (cmd is the parent, name the command run), by
// `<leaf> <arg> --help` and, through groupCommandAction, by a root or group
// run with an unknown first word (`rancher nosuch`, `rancher get help`).
// Three cases:
//   - cmd has no subcommand of that name and no CommandNotFound seam: name
//     is an argument of cmd, not a topic, so cmd shows its own card.
//     urfave's stock implementation fails here with "No help topic" and
//     exit 3 (the AUTH exit code) without printing anything.
//   - name is a visible subcommand: the words that follow it in cmd's
//     arguments name a path, and the card is that of the deepest command
//     named, so `rancher --help get pod` shows `get pod`. The path ends at
//     a flag or at a command with no visible subcommands (whose next words
//     are its arguments). Below a command that has visible subcommands, a
//     hidden subcommand shows its own card and any other unknown word is an
//     unknown command, reported with the words typed after it.
//   - anything else (a hidden subcommand, an unknown name under a root,
//     group or hidden command that owns a CommandNotFound seam) keeps
//     urfave's behavior.
func showCommandHelp(ctx context.Context, cmd *ucli.Command, name string) error {
	if cmd.Command(name) == nil && cmd.CommandNotFound == nil {
		showHelpCard(cmd)
		return nil
	}
	node := visibleSubcommand(cmd, name)
	if node == nil {
		return ucli.DefaultShowCommandHelp(ctx, cmd, name)
	}
	words := cmd.Args().Tail()
	for i, word := range words {
		if strings.HasPrefix(word, "-") || len(node.VisibleCommands()) == 0 {
			break
		}
		next := visibleSubcommand(node, word)
		if next == nil {
			if node.Command(word) != nil {
				return ucli.DefaultShowCommandHelp(ctx, node, word)
			}
			reportUnknownCommand(node, word, words[i+1:])
			return nil
		}
		node = next
	}
	showHelpCard(node)
	return nil
}

// showHelpCard prints cmd's own help: the group card for a command with
// visible subcommands, the leaf card otherwise.
func showHelpCard(cmd *ucli.Command) {
	tmpl := cmd.CustomHelpTemplate
	if tmpl == "" {
		tmpl = ucli.CommandHelpTemplate
		if len(cmd.VisibleCommands()) > 0 {
			tmpl = ucli.SubcommandHelpTemplate
		}
	}
	ucli.HelpPrinter(cmd.Root().Writer, tmpl, cmd)
}

// printStyledHelp renders templ through urfave's own template engine,
// adding a "bold" function resolved from this invocation's --color value
// and the target writer's TTY-ness - the same activation rule every other
// styled render site uses. data is always the *ucli.Command whose help is
// being shown (cmd.Root() for root help); a data of a different shape (only
// possible on the FishCompletionTemplate path, which never calls this
// printer) leaves styling disabled.
func printStyledHelp(w io.Writer, templ string, data any) {
	kept := &firstErrorWriter{w: w}
	w = kept
	var s style.Style
	if cmd, ok := data.(*ucli.Command); ok {
		s = helpStyle(cmd)
		defer func() { noteHelpWriteError(cmd.Root(), kept.err) }()
	}
	ucli.HelpPrinterCustom(w, templ, data, map[string]any{
		"bold":                s.Bold,
		"colonOffsetCommands": colonOffsetCommands,
		"sortedCommands":      sortedCommands,
		"sortedFlags":         sortedFlags,
		"leafFlags":           leafFlags,
		"leafGlobalFlags":     leafGlobalFlags,
		"groupGlobalFlags":    groupGlobalFlags,
		"primeMark":           primeMark,
		"colorPrimeMark":      func(cmd *ucli.Command, wrapped string) string { return colorPrimeMark(s, cmd, wrapped) },
		"colorPrimeDescription": func(cmd *ucli.Command, wrapped string) string {
			return colorPrimeDescription(s, cmd, wrapped)
		},
	})
}

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

// helpWriteErrors holds, for each root command, the first error of a failed
// write of a help card or the version in the run of that root. urfave's
// printers return nil whether or not the text was written; the run reads
// the error from here when it ends. A root has its own entry: runs of two
// roots never share an error.
var helpWriteErrors sync.Map

// noteHelpWriteError keeps err as the first write error of root's run.
func noteHelpWriteError(root *ucli.Command, err error) {
	if err != nil {
		helpWriteErrors.LoadOrStore(root, err)
	}
}

// takeHelpWriteError returns the error kept for root, if any, and clears it.
func takeHelpWriteError(root *ucli.Command) error {
	if v, ok := helpWriteErrors.LoadAndDelete(root); ok {
		return v.(error)
	}
	return nil
}

// printVersion writes the version line urfave's own printer writes, and keeps
// the write error for the run of the root.
func printVersion(cmd *ucli.Command) {
	if _, err := fmt.Fprintf(cmd.Root().Writer, "%v version %v\n", cmd.Name, cmd.Version); err != nil {
		noteHelpWriteError(cmd.Root(), err)
	}
}

// primeMark returns the plain (uncolored) "[Prime] " prefix for a Prime
// command's usage text, or "" for an ordinary one. It stays uncolored so the
// COMMANDS block's wrap computes column widths off plain text length, same
// as any other row - coloring happens afterward, in colorPrimeMark.
func primeMark(cmd *ucli.Command) string {
	if commandIsPrime(cmd) {
		return runtime.PrimeMarker + " "
	}
	return ""
}

// colorPrimeMark colors the leading "[Prime]" token green in a row already
// wrapped by primeMark+wrap, leaving every other row byte-identical. Usage
// text never carries the literal token (an invariant enforced elsewhere), so
// the first occurrence in a Prime row's wrapped text is always the marker.
func colorPrimeMark(s style.Style, cmd *ucli.Command, wrapped string) string {
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
func colorPrimeDescription(s style.Style, cmd *ucli.Command, wrapped string) string {
	if !commandIsPrime(cmd) {
		return wrapped
	}
	return s.Green(wrapped)
}

// helpStyle mirrors internal/runtime's stylesFor for the OUT stream, without
// its format gate: a help render is always plain text, never routed through
// the output/json/yaml pipeline, so no format check applies here.
func helpStyle(cmd *ucli.Command) style.Style {
	root := cmd.Root()
	return style.Style{Enabled: runtime.ActivateColor(
		cmd.String("color"),
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
func errStyle(cmd *ucli.Command, colorValue string) style.Style {
	return style.Style{Enabled: runtime.ActivateColor(
		colorValue,
		os.Getenv("NO_COLOR"),
		os.Getenv("CLICOLOR_FORCE"),
		os.Getenv("FORCE_COLOR"),
		fileIsTTY(cmd.Root().ErrWriter),
	)}
}

// colonOffsetCommands returns the left-column width for a colon-aligned
// COMMANDS list: the longest comma-joined "names:" plus fixed, mirroring
// urfave's own offsetCommands, which sizes the same column on every
// comma-joined alias rather than the canonical name alone.
func colonOffsetCommands(cmds []*ucli.Command, fixed int) int {
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
func sortedCommands(cmds []*ucli.Command) []*ucli.Command {
	out := slices.Clone(cmds)
	slices.SortFunc(out, func(a, b *ucli.Command) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// sortedFlags returns the flags ordered by their first (long) name, so
// OPTIONS and GLOBAL OPTIONS read alphabetically. A copy: the command's
// own flag slice keeps its declaration order.
func sortedFlags(flags []ucli.Flag) []ucli.Flag {
	out := slices.Clone(flags)
	slices.SortFunc(out, func(a, b ucli.Flag) int { return strings.Compare(a.Names()[0], b.Names()[0]) })
	return out
}

// leafFlags is a leaf's OPTIONS: its own flags, plus --project with what it
// does for this command when the command takes it. --project is parsed as
// the global flag, so it is never one of the leaf's own urfave flags.
func leafFlags(cmd *ucli.Command) []ucli.Flag {
	flags := cmd.VisibleFlags()
	if desc, ok := commandProject(cmd); ok {
		flags = append(flags, &ucli.StringFlag{Name: runtime.FlagProject, Usage: desc})
	}
	return flags
}

// leafGlobalFlags is a command's GLOBAL OPTIONS: the global flags the command
// accepts, without --project (a command that takes it lists it under
// OPTIONS, and every other command refuses it).
func leafGlobalFlags(cmd *ucli.Command) []ucli.Flag {
	return slices.DeleteFunc(slices.Clone(cmd.VisiblePersistentFlags()), func(f ucli.Flag) bool {
		name := f.Names()[0]
		return name == runtime.FlagProject || !commandAcceptsGlobalFlag(cmd, name)
	})
}

// groupGlobalFlags is a command group's GLOBAL OPTIONS: the global flags a
// command under the group accepts, --project included when one of them
// takes it.
func groupGlobalFlags(cmd *ucli.Command) []ucli.Flag {
	return slices.DeleteFunc(slices.Clone(cmd.VisiblePersistentFlags()), func(f ucli.Flag) bool {
		return !commandAcceptsGlobalFlag(cmd, f.Names()[0])
	})
}

// commandsBlock is the colon-aligned, alphabetical COMMANDS list shared by
// rootCommandHelpTemplate and subcommandHelpTemplate - the two shapes that
// ever show one. It replaces urfave's own comma-joined, colon-less
// visibleCommandCategoryTemplate/visibleCommandTemplate, which cannot be
// overridden (they are unexported template names urfave always re-parses).
const commandsBlock = `{{ $cmds := sortedCommands .VisibleCommands }}{{ $cv := colonOffsetCommands $cmds 5 }}{{range $cmds}}
   {{$s := printf "%s:" (join .Names ", ")}}{{$s}}{{ $sp := subtract $cv (offset $s 3) }}{{ indent $sp ""}}{{colorPrimeMark . (wrap (print (primeMark .) .Usage) $cv)}}{{end}}`

// flagsBlock is urfave's visibleFlagTemplate over an alphabetical copy of
// the flags; like the command templates, urfave's own is unexported and
// cannot be overridden in place.
const flagsBlock = `{{range $i, $e := sortedFlags .VisibleFlags}}
   {{wrap $e.String 6}}{{end}}`

const leafFlagsBlock = `{{range $i, $e := sortedFlags (leafFlags .)}}
   {{wrap $e.String 6}}{{end}}`

const leafGlobalFlagsBlock = `{{range $i, $e := sortedFlags (leafGlobalFlags .)}}
   {{wrap $e.String 6}}{{end}}`

const groupGlobalFlagsBlock = `{{range $i, $e := sortedFlags (groupGlobalFlags .)}}
   {{wrap $e.String 6}}{{end}}`

// rootCommandHelpTemplate is urfave's RootCommandHelpTemplate with bold
// section headers, the DESCRIPTION block moved above USAGE, and a
// colon-aligned COMMANDS list in place of the stock comma-joined rows.
var rootCommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}} {{if .VisibleFlags}}[global options]{{end}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Version}}{{if not .HideVersion}}

{{bold "VERSION:"}}
   {{.Version}}{{end}}{{end}}
{{- if len .Authors}}

{{bold "AUTHOR"}}{{template "authorsTemplate" .}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlagCategories}}

{{bold "GLOBAL OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{bold "GLOBAL OPTIONS:"}}` + flagsBlock + `{{end}}{{if .Copyright}}

{{bold "COPYRIGHT:"}}
   {{template "copyrightTemplate" .}}{{end}}
`

// commandHelpTemplate is urfave's CommandHelpTemplate (a leaf with no
// visible subcommands) with bold section headers, the DESCRIPTION block
// moved above USAGE, and --project listed under OPTIONS by the commands
// that take it and nowhere by the rest.
var commandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{template "usageTemplate" .}}{{if .Category}}

{{bold "CATEGORY:"}}
   {{.Category}}{{end}}{{if .VisibleFlagCategories}}

{{bold "OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if leafFlags .}}

{{bold "OPTIONS:"}}` + leafFlagsBlock + `{{end}}{{if leafGlobalFlags .}}

{{bold "GLOBAL OPTIONS:"}}` + leafGlobalFlagsBlock + `{{end}}
`

// subcommandHelpTemplate is urfave's SubcommandHelpTemplate (a group - has
// visible subcommands) with bold section headers, the DESCRIPTION block
// moved above USAGE, and a colon-aligned COMMANDS list in place of the
// stock comma-joined rows.
var subcommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{colorPrimeDescription . (wrap .Description 3)}}{{end}}

{{bold "USAGE:"}}
   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Category}}

{{bold "CATEGORY:"}}
   {{.Category}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlagCategories}}

{{bold "OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{bold "OPTIONS:"}}` + flagsBlock + `{{end}}{{if groupGlobalFlags .}}

{{bold "GLOBAL OPTIONS:"}}` + groupGlobalFlagsBlock + `{{end}}
`
