package runtime_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// textCmd is a command with a fixed descriptor that returns a fixed result.
type textCmd struct {
	d   runtime.Descriptor
	res runtime.Result
}

func (c textCmd) Describe() runtime.Descriptor { return c.d }

func (c textCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return c.res, nil
}

// resultCommand is a command that returns res under a descriptor of the given
// shape.
func resultCommand(shape runtime.OutputShape, res runtime.Result) runtime.Command {
	return textCmd{d: runtime.Descriptor{Name: "config status", CommandType: runtime.Local, Shape: shape}, res: res}
}

// summaryTable is the tabular data of a result that carries a summary.
type summaryTable struct{}

func (summaryTable) Columns() []string { return []string{"NAME"} }
func (summaryTable) Rows() [][]string  { return [][]string{{"a"}} }

// unprintableDirectory is a config directory whose path holds a newline.
const unprintableDirectory = "/home/u/a\nb"

// textChannel is one text the pipeline writes to standard output: how an
// invocation is set up for it, the command that puts a line in it and how the
// line appears in the output.
type textChannel struct {
	name  string
	setup func(*runtime.IO)
	build func(line string) runtime.Command
	shown func(line string) string
}

func textChannels() []textChannel {
	plainLine := func(line string) string { return line + "\n" }
	return []textChannel{
		{"result text", nil, func(line string) runtime.Command {
			return resultCommand(runtime.ShapeText, runtime.Result{Text: line + "\n"})
		}, plainLine},
		{"styled text", func(io *runtime.IO) { io.Color = "always" }, func(line string) runtime.Command {
			return resultCommand(runtime.ShapeText, runtime.Result{
				Text:       line + "\n",
				StyledText: func(s style.Style) string { return s.Bold(line) + "\n" },
			})
		}, func(line string) string { return style.Style{Enabled: true}.Bold(line) + "\n" }},
		{"wide text", func(io *runtime.IO) { io.Format = "wide" }, func(line string) runtime.Command {
			return resultCommand(runtime.ShapeText, runtime.Result{Text: "narrow\n", WideText: line + "\n"})
		}, plainLine},
		{"summary print", nil, func(line string) runtime.Command {
			return resultCommand(runtime.ShapeTabular, runtime.Result{Data: summaryTable{}, Summary: line})
		}, plainLine},
		{"preview text", func(io *runtime.IO) { io.DryRun = true }, previewCommand, func(line string) string { return "update Cluster/" + line + "\n" }},
	}
}

// textFormats are the formats that write the text channels: the default, table
// and wide.
var textFormats = []struct{ name, format string }{{"default", ""}, {"table", "table"}, {"wide", "wide"}}

// runChannel runs the channel's command with the line, the format and the
// config directory, and returns what it wrote to standard output. A channel
// that needs a format of its own sets it after the format given.
func runChannel(t *testing.T, ch textChannel, line, format, dir string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: dir}
	if ch.setup != nil {
		ch.setup(&streams)
	}
	_, ce := runtime.Run(t.Context(), ch.build(line), siteInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	return out.String()
}

// TestEveryTextChannelNamesTheConfigDirectory asserts the result text, the
// styled text, the wide text, the summary line and the dry-run preview each
// write a remedy command with the invocation's config directory when it can
// be printed, with no character of the remedy marker; with the plain command
// when the invocation names no directory; and, for a directory that cannot
// be printed, with the words up to the clause that holds the command and the
// reason no command is offered; in the default, table and wide formats.
func TestEveryTextChannelNamesTheConfigDirectory(t *testing.T) {
	t.Parallel()
	const before, after = "see the list; then run ", " now"
	line := before + clierr.Rancher + " config status" + after
	for _, ch := range textChannels() {
		for _, f := range textFormats {
			t.Run(ch.name+" "+f.name, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				prefix, printable := runtime.RemedyPrefix(dir)
				require.True(t, printable)

				out := runChannel(t, ch, line, f.format, dir)
				assert.Contains(t, out, ch.shown(before+"rancher"+prefix+" config status"+after))
				assert.False(t, holdsMarkerRune(out), "the output holds a character of the remedy marker: %q", out)

				out = runChannel(t, ch, line, f.format, "")
				assert.Contains(t, out, ch.shown(before+"rancher config status"+after))
				assert.False(t, holdsMarkerRune(out), "%q", out)

				out = runChannel(t, ch, line, f.format, unprintableDirectory)
				assert.Contains(t, out, ch.shown("see the list; "+runtime.UnprintableConfigDirNote))
				assert.NotContains(t, out, "rancher ")
				assert.NotContains(t, out, "--config")
				assert.NotContains(t, out, "/home/u/a")
				assert.False(t, holdsMarkerRune(out), "%q", out)
			})
		}
	}
}

// TestResultTextWithAnUnprintableDirectoryChangesOnlyTheLinesWithACommand
// asserts that a three-line result text with a command in the second line,
// printed for a directory that cannot be printed, keeps the first and the
// third line as they are.
func TestResultTextWithAnUnprintableDirectoryChangesOnlyTheLinesWithACommand(t *testing.T) {
	t.Parallel()
	text := "first line\nrun " + clierr.Rancher + " config status\nthird line\n"
	var out, errBuf bytes.Buffer
	streams := runtime.IO{Out: &out, Err: &errBuf, RemedyDir: unprintableDirectory}
	_, ce := runtime.Run(t.Context(), resultCommand(runtime.ShapeText, runtime.Result{Text: text}), siteInput{}, streams, safety.Binding{})
	require.Nil(t, ce)
	assert.Equal(t, "first line\n"+runtime.UnprintableConfigDirNote+"\nthird line\n", out.String())
}

// TestEnvelopeSummaryNamesTheConfigDirectory asserts the summary of the json
// and yaml envelope, decoded, holds the command with the directory for a
// directory that can be printed, the plain command for an invocation with no
// directory, and the reason no command is offered for one that cannot be
// printed.
func TestEnvelopeSummaryNamesTheConfigDirectory(t *testing.T) {
	t.Parallel()
	summary := "see " + clierr.Rancher + " config status"
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	for _, tc := range []struct{ name, dir, want string }{
		{"a printable directory", dir, "see rancher" + prefix + " config status"},
		{"no directory", "", "see rancher config status"},
		{"an unprintable directory", unprintableDirectory, runtime.UnprintableConfigDirNote},
	} {
		for _, format := range []string{"json", "yaml"} {
			t.Run(tc.name+" "+format, func(t *testing.T) {
				t.Parallel()
				var out, errBuf bytes.Buffer
				streams := runtime.IO{Out: &out, Err: &errBuf, Format: format, RemedyDir: tc.dir}
				cmd := resultCommand(runtime.ShapeTabular, runtime.Result{Kind: "Report", Data: summaryTable{}, Summary: summary})
				_, ce := runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
				require.Nil(t, ce)
				var doc struct {
					Summary string `json:"summary"`
				}
				require.NoError(t, yaml.Unmarshal(out.Bytes(), &doc))
				assert.Equal(t, tc.want, doc.Summary)
				assert.False(t, holdsMarkerRune(out.String()), "%q", out.String())
			})
		}
	}
}

// plainTextResults are three results that carry the same text, one for each
// payload a text command can return: its text alone, with a styled form and
// with a wide form.
func plainTextResults(text string) []struct {
	name   string
	format string
	color  string
	res    runtime.Result
} {
	return []struct {
		name   string
		format string
		color  string
		res    runtime.Result
	}{
		{"text", "", "", runtime.Result{Text: text}},
		{"styled text", "", "always", runtime.Result{Text: text, StyledText: func(s style.Style) string { return s.Bold(text) }}},
		{"wide text", "wide", "", runtime.Result{Text: "narrow\n", WideText: text}},
	}
}

// TestPlainTextCommandPrintsTheCommandWithNoDirectory asserts the text of a
// command whose descriptor sets PlainText, in each of the three payloads a
// text command returns, prints a command of the CLI as the plain command and
// keeps a text from a server (U+2063 followed by the word rancher) as it is,
// for a directory that can be printed and for one that cannot; and that the
// same three results without PlainText name the directory and print the text
// from a server without the U+2063, so it is no command form.
func TestPlainTextCommandPrintsTheCommandWithNoDirectory(t *testing.T) {
	t.Parallel()
	text := "ran " + clierr.Rancher + " get pods\nserver said " + plantedRemedy + "\n"
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	for _, dirCase := range []struct{ name, dir string }{{"printable", dir}, {"unprintable", unprintableDirectory}} {
		remedyDir := dirCase.dir
		for _, tc := range plainTextResults(text) {
			for _, plain := range []bool{true, false} {
				mode := "names the directory"
				if plain {
					mode = "plain text"
				}
				t.Run(tc.name+" "+mode+" "+dirCase.name+" directory", func(t *testing.T) {
					t.Parallel()
					var out, errBuf bytes.Buffer
					streams := runtime.IO{Out: &out, Err: &errBuf, Format: tc.format, Color: tc.color, RemedyDir: remedyDir}
					cmd := textCmd{d: runtime.Descriptor{Name: "logs", CommandType: runtime.Local, Shape: runtime.ShapeText, PlainText: plain}, res: tc.res}
					_, ce := runtime.Run(t.Context(), cmd, siteInput{}, streams, safety.Binding{})
					require.Nil(t, ce)
					got := out.String()
					if plain {
						assert.Contains(t, got, "server said "+plantedRemedy, "a text from a server is written as it is")
					} else {
						assert.Contains(t, got, "server said rancher delete x", "a text from a server loses the U+2063")
						assert.NotContains(t, got, plantedRemedy)
					}
					assert.NotContains(t, got, clierr.RemedyMarker, "no marker is left")
					switch {
					case plain:
						assert.Contains(t, got, "ran rancher get pods")
						assert.NotContains(t, got, "--config")
					case remedyDir == dir:
						assert.Contains(t, got, "ran rancher"+prefix+" get pods")
					default:
						assert.Contains(t, got, runtime.UnprintableConfigDirNote)
						assert.NotContains(t, got, "ran rancher get pods")
					}
				})
			}
		}
	}
}
