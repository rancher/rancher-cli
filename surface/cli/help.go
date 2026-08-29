package cli

import (
	"io"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// init wires the styled help templates once, package-wide: every help
// render path in urfave/cli/v3 (root, group, and leaf) funnels through the
// single HelpPrinter var, so overriding it here covers all three shapes.
func init() {
	ucli.HelpPrinter = printStyledHelp
	ucli.RootCommandHelpTemplate = rootCommandHelpTemplate
	ucli.CommandHelpTemplate = commandHelpTemplate
	ucli.SubcommandHelpTemplate = subcommandHelpTemplate
}

// printStyledHelp renders templ through urfave's own template engine,
// adding a "bold" function resolved from this invocation's --color value
// and the target writer's TTY-ness — the same activation rule every other
// styled render site uses. data is always the *ucli.Command whose help is
// being shown (cmd.Root() for root help); a data of a different shape (only
// possible on the FishCompletionTemplate path, which never calls this
// printer) leaves styling disabled.
func printStyledHelp(w io.Writer, templ string, data any) {
	var s style.Style
	if cmd, ok := data.(*ucli.Command); ok {
		s = helpStyle(cmd)
	}
	ucli.HelpPrinterCustom(w, templ, data, map[string]any{
		"bold":                s.Bold,
		"colonOffsetCommands": colonOffsetCommands,
	})
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

// commandsBlock is the colon-aligned COMMANDS list shared by
// rootCommandHelpTemplate and subcommandHelpTemplate — the two shapes that
// ever show one. It replaces urfave's own comma-joined, colon-less
// visibleCommandCategoryTemplate/visibleCommandTemplate, which cannot be
// overridden (they are unexported template names urfave always re-parses).
const commandsBlock = `{{ $cv := colonOffsetCommands .VisibleCommands 5 }}{{range .VisibleCommands}}
   {{$s := printf "%s:" (join .Names ", ")}}{{$s}}{{ $sp := subtract $cv (offset $s 3) }}{{ indent $sp ""}}{{wrap .Usage $cv}}{{end}}`

// rootCommandHelpTemplate is urfave's RootCommandHelpTemplate with bold
// section headers, the DESCRIPTION block moved above USAGE, and a
// colon-aligned COMMANDS list in place of the stock comma-joined rows.
var rootCommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{template "descriptionTemplate" .}}{{end}}

{{bold "USAGE:"}}
   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}} {{if .VisibleFlags}}[global options]{{end}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Version}}{{if not .HideVersion}}

{{bold "VERSION:"}}
   {{.Version}}{{end}}{{end}}
{{- if len .Authors}}

{{bold "AUTHOR"}}{{template "authorsTemplate" .}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlagCategories}}

{{bold "GLOBAL OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{bold "GLOBAL OPTIONS:"}}{{template "visibleFlagTemplate" .}}{{end}}{{if .Copyright}}

{{bold "COPYRIGHT:"}}
   {{template "copyrightTemplate" .}}{{end}}
`

// commandHelpTemplate is urfave's CommandHelpTemplate (a leaf with no
// visible subcommands) with bold section headers and the DESCRIPTION block
// moved above USAGE.
var commandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{template "descriptionTemplate" .}}{{end}}

{{bold "USAGE:"}}
   {{template "usageTemplate" .}}{{if .Category}}

{{bold "CATEGORY:"}}
   {{.Category}}{{end}}{{if .VisibleFlagCategories}}

{{bold "OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{bold "OPTIONS:"}}{{template "visibleFlagTemplate" .}}{{end}}{{if .VisiblePersistentFlags}}

{{bold "GLOBAL OPTIONS:"}}{{template "visiblePersistentFlagTemplate" .}}{{end}}
`

// subcommandHelpTemplate is urfave's SubcommandHelpTemplate (a group — has
// visible subcommands) with bold section headers, the DESCRIPTION block
// moved above USAGE, and a colon-aligned COMMANDS list in place of the
// stock comma-joined rows.
var subcommandHelpTemplate = `{{bold "NAME:"}}
   {{template "helpNameTemplate" .}}{{if .Description}}

{{bold "DESCRIPTION:"}}
   {{template "descriptionTemplate" .}}{{end}}

{{bold "USAGE:"}}
   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Category}}

{{bold "CATEGORY:"}}
   {{.Category}}{{end}}{{if .VisibleCommands}}

{{bold "COMMANDS:"}}` + commandsBlock + `{{end}}{{if .VisibleFlagCategories}}

{{bold "OPTIONS:"}}{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{bold "OPTIONS:"}}{{template "visibleFlagTemplate" .}}{{end}}{{if .VisiblePersistentFlags}}

{{bold "GLOBAL OPTIONS:"}}{{template "visiblePersistentFlagTemplate" .}}{{end}}
`
