package cli

import (
	"errors"
	"io"
	"strings"
	"text/tabwriter"
	"text/template"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/output/style"
)

// helpPage is the kind of help page to render.
type helpPage int

const (
	// helpPageRoot is the page of the root command.
	helpPageRoot helpPage = iota + 1
	// helpPageGroup is the page of a command that has visible subcommands.
	helpPageGroup
	// helpPageLeaf is the page of a command with no visible subcommands.
	helpPageLeaf
)

// helpNamePartial is the NAME line: the full name and, when the command has a
// Usage (its one-line description), a dash and that text. Later lines of the
// Usage line up under its first line.
const helpNamePartial = `{{wrap .FullName 3}}{{if .Usage}} - {{wrap .Usage (offset .FullName 6)}}{{end}}`

// helpUsagePartial is the USAGE line of a leaf page.
const helpUsagePartial = `{{.FullName}}{{if .VisibleFlags}} [options]{{end}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{end}}`

// renderHelpPage writes the help page of cmd to w, in the style s, and returns
// the first error a write to w returned, else the error of the template, else
// the error of the flush. The page goes through one tab writer, which holds
// the last lines until the end of the page; an error of those writes is
// returned too.
func renderHelpPage(w io.Writer, page helpPage, cmd *Command, s style.Style) error {
	var text string
	switch page {
	case helpPageRoot:
		text = rootCommandHelpTemplate
	case helpPageGroup:
		text = subcommandHelpTemplate
	case helpPageLeaf:
		text = commandHelpTemplate
	default:
		return errors.New("internal error: the help page is not for the root, a group or a single command; no help was written")
	}
	t, err := template.New("help").Funcs(helpFuncs(s)).Parse(text)
	if err != nil {
		return err
	}
	if _, err := t.New("helpNameTemplate").Parse(helpNamePartial); err != nil {
		return err
	}
	if _, err := t.New("usageTemplate").Parse(helpUsagePartial); err != nil {
		return err
	}
	kept := &firstErrorWriter{w: w}
	tw := tabwriter.NewWriter(kept, 1, 8, 2, ' ', 0)
	execErr := t.Execute(tw, cmd)
	flushErr := tw.Flush()
	if kept.err != nil {
		return kept.err
	}
	if execErr != nil {
		return execErr
	}
	return flushErr
}

// helpFuncs returns the functions the help templates use.
func helpFuncs(s style.Style) template.FuncMap {
	return template.FuncMap{
		"join":                strings.Join,
		"subtract":            func(a, b int) int { return a - b },
		"indent":              indentText,
		"offset":              func(text string, fixed int) int { return len(text) + fixed },
		"wrap":                wrapLines,
		"flagRow":             (*Flag).helpRow,
		"bold":                s.Bold,
		"colonOffsetCommands": colonOffsetCommands,
		"sortedCommands":      sortedCommands,
		"sortedFlags":         sortedFlags,
		"leafFlags":           leafFlags,
		"leafGlobalFlags":     leafGlobalFlags,
		"groupGlobalFlags":    groupGlobalFlags,
		"primeMark":           primeMark,
		"colorPrimeMark":      func(cmd *Command, wrapped string) string { return colorPrimeMark(s, cmd, wrapped) },
		"colorPrimeDescription": func(cmd *Command, wrapped string) string {
			return colorPrimeDescription(s, cmd, wrapped)
		},
	}
}

// indentText returns text with n spaces before it and after every newline in
// it.
func indentText(n int, text string) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(text, "\n", "\n"+pad)
}

// wrapLines puts offset spaces before every line of text after the first,
// except empty lines. It never breaks a line, whatever its length: a flag row
// or a usage stays on one line and the terminal wraps it.
func wrapLines(text string, offset int) string {
	lines := strings.Split(text, "\n")
	pad := strings.Repeat(" ", offset)
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// helpRow returns the row of flag f in an options list: the names, a tab
// and the usage text, followed by the environment variable f reads. The
// names are -x for a name of one character and --name for the others, each
// followed by the placeholder; a flag that can be given more than once
// repeats them in brackets. The placeholder is the text inside the first pair
// of backticks of the usage text (the backticks are dropped from the text),
// whether or not the flag takes a value; without a pair, a flag that takes a
// value has the placeholder "string" and any other flag has none. No flag
// shows a default value, and the variable name reads the same on every OS.
func (f *Flag) helpRow() string {
	placeholder, usage := splitPlaceholder(f.Usage)
	if placeholder == "" && f.takesValue() {
		placeholder = "string"
	}
	var names strings.Builder
	all := f.Names()
	for i, name := range all {
		if name == "" {
			continue
		}
		if utf8.RuneCountInString(name) == 1 {
			names.WriteString("-")
		} else {
			names.WriteString("--")
		}
		names.WriteString(name)
		if placeholder != "" {
			names.WriteString(" " + placeholder)
		}
		if i < len(all)-1 {
			names.WriteString(", ")
		}
	}
	row := names.String()
	if f.Kind == sliceFlag {
		row += " [ " + row + " ]"
	}
	row += "\t" + strings.TrimSpace(usage)
	if f.EnvVar != "" {
		row += " [$" + f.EnvVar + "]"
	}
	return row
}

// splitPlaceholder returns the text inside the first pair of backticks of
// usage and usage without those two backticks. A usage text with no pair
// (a lone backtick included) has no placeholder and stays as it is.
func splitPlaceholder(usage string) (placeholder, text string) {
	before, rest, ok := strings.Cut(usage, "`")
	if !ok {
		return "", usage
	}
	inside, after, ok := strings.Cut(rest, "`")
	if !ok {
		return "", usage
	}
	return inside, before + inside + after
}
