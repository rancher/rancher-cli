package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// terminalHostileTail is what a server value can hold to drive a terminal or
// to look like something else: a clear-screen and a conceal sequence, the
// one-byte sequence introducer, bell, backspace, carriage return, NUL, DEL,
// tab, a bidi override, a byte order mark and a bare U+2063.
const terminalHostileTail = "\x1b[2J\x1b[8m\u009b2J\a\b\r\x00\x7f\t\u202e\ufeff\u2063"

// terminalForeignMarker is a remedy marker of the form this process writes,
// from another process: U+2063 followed by 32 runes from U+2060, U+2061,
// U+2062 and U+2064.
var terminalForeignMarker = "\u2063" + strings.Repeat("\u2060\u2061\u2062\u2064", 8)

// terminalHostile is the value planted at the place named p: the name, the
// hostile tail, a foreign marker followed by the word rancher, the line and
// paragraph separators, and a second line that starts with "forged line".
func terminalHostile(p string) string {
	return p + terminalHostileTail + terminalForeignMarker + "rancher delete x\u2028\u2029\nforged line"
}

// terminalFilteredTail is what remains of terminalHostileTail and the text
// after it once every control and format character is removed.
const terminalFilteredTail = "[2J[8m2Jrancher delete x"

// terminalFilteredLines is terminalHostile(p) as a text print or a notice
// writes it: the characters removed, the newline kept.
func terminalFilteredLines(p string) string { return p + terminalFilteredTail + "\nforged line" }

// terminalFilteredLine is terminalHostile(p) as a one-line channel writes it:
// the characters and the newline removed.
func terminalFilteredLine(p string) string { return p + terminalFilteredTail + "forged line" }

// terminalSequence is a character or sequence of terminalHostile that must
// not reach the terminal.
type terminalSequence struct{ name, text string }

// terminalSequences are the planted characters, checked one by one, so a
// check that removes one of them by the path of another does not hide it.
var terminalSequences = []terminalSequence{
	{"escape", "\x1b"},
	{"one-byte sequence introducer", "\u009b"},
	{"bell", "\a"},
	{"backspace", "\b"},
	{"carriage return", "\r"},
	{"NUL", "\x00"},
	{"DEL", "\x7f"},
	{"tab", "\t"},
	{"bidi override", "\u202e"},
	{"byte order mark", "\ufeff"},
	{"U+2060", "\u2060"},
	{"U+2061", "\u2061"},
	{"U+2062", "\u2062"},
	{"U+2063", "\u2063"},
	{"U+2064", "\u2064"},
	{"line separator", "\u2028"},
	{"paragraph separator", "\u2029"},
}

// terminalColorSequences are the planted sequences for a row that runs with
// color on, whose output holds the CLI's own escape codes: the two escape
// sequences the hostile value holds replace the bare escape character.
var terminalColorSequences = slices.Concat(
	[]terminalSequence{{"clear screen", "\x1b[2J"}, {"conceal", "\x1b[8m"}},
	terminalSequences[1:],
)

// terminalInput is the Input of a command that takes no flags and no
// arguments.
type terminalInput struct{}

func (terminalInput) String(string) string    { return "" }
func (terminalInput) Bool(string) bool        { return false }
func (terminalInput) Strings(string) []string { return nil }
func (terminalInput) IsSet(string) bool       { return false }
func (terminalInput) Args() []string          { return nil }

// terminalRead is a read command: it calls act with its prompter, then
// returns its result and error.
type terminalRead struct {
	d   runtime.Descriptor
	res runtime.Result
	err error
	act func(ctx context.Context, p runtime.Prompter)
}

func (c terminalRead) Describe() runtime.Descriptor { return c.d }

func (c terminalRead) Execute(ctx context.Context, _ runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	if c.act != nil {
		c.act(ctx, p)
	}
	return c.res, c.err
}

// terminalMutating is a mutating command with a fixed preview and result.
type terminalMutating struct {
	d   runtime.Descriptor
	eff safety.Effect
	res runtime.Result
}

func (c terminalMutating) Describe() runtime.Descriptor { return c.d }

func (c terminalMutating) Plan(context.Context, runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return c.eff, func(context.Context) (runtime.Result, error) { return c.res, nil }, nil
}

// terminalTable is tabular data with the given columns and rows.
type terminalTable struct {
	cols []string
	rows [][]string
}

func (t terminalTable) Columns() []string { return t.cols }
func (t terminalTable) Rows() [][]string  { return t.rows }

// terminalValue is the data of a structured result: one text value, which
// is also its one table cell.
type terminalValue struct {
	Value string `json:"value"`
}

func (terminalValue) Columns() []string { return []string{"VALUE"} }

func (v terminalValue) Rows() [][]string { return [][]string{{v.Value}} }

// terminalDetails is an error's details payload that holds the given lines.
type terminalDetails struct{ lines []string }

func (d terminalDetails) TextDetails() []string { return d.lines }

// terminalDescriptor is the descriptor of a read command that returns the
// given shape.
func terminalDescriptor(shape runtime.OutputShape) runtime.Descriptor {
	return runtime.Descriptor{Name: "config status", CommandType: runtime.Local, Shape: shape}
}

// terminalEnv is one row's invocation: its config directory, which every
// remedy command of the row names, and the text a remedy command of this
// process expands to there.
type terminalEnv struct {
	t        *testing.T
	dir      string
	expanded string
}

func newTerminalEnv(t *testing.T) terminalEnv {
	t.Helper()
	dir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(dir)
	require.True(t, printable)
	return terminalEnv{t: t, dir: dir, expanded: "rancher" + prefix + " auth status"}
}

// remedyText is a text with a remedy command of this process and the hostile
// value for the place named p: a server value and a command of the CLI in
// one text.
func remedyText(p string) string {
	return "run " + clierr.Rancher + " auth status " + terminalHostile(p)
}

// remedyExpected is remedyText(p) as it is written when every character is
// removed except the newline of a text print, with the command expanded.
func (e terminalEnv) remedyExpected(p string) string {
	return "run " + e.expanded + " " + terminalFilteredLines(p)
}

// remedyExpectedLine is remedyText(p) as a one-line channel writes it.
func (e terminalEnv) remedyExpectedLine(p string) string {
	return "run " + e.expanded + " " + terminalFilteredLine(p)
}

// io is the invocation's IO: color never (so the environment cannot add the
// CLI's own codes) and the row's config directory; set changes it.
func (e terminalEnv) io(out, errBuf *bytes.Buffer, set func(*runtime.IO)) runtime.IO {
	streams := runtime.IO{Out: out, Err: errBuf, Color: "never", RemedyDir: e.dir}
	if set != nil {
		set(&streams)
	}
	return streams
}

// run runs cmd through the pipeline and returns what it wrote to standard
// output and standard error.
func (e terminalEnv) run(cmd runtime.Command, set func(*runtime.IO)) (stdout, stderr string) {
	e.t.Helper()
	stdout, stderr, ce := e.runFailing(cmd, set)
	require.Nil(e.t, ce)
	return stdout, stderr
}

// runFailing is run for a command that may fail.
func (e terminalEnv) runFailing(cmd runtime.Command, set func(*runtime.IO)) (stdout, stderr string, ce *clierr.Error) {
	e.t.Helper()
	var out, errBuf bytes.Buffer
	_, ce = runtime.Run(e.t.Context(), cmd, terminalInput{}, e.io(&out, &errBuf, set), safety.Binding{})
	return out.String(), errBuf.String(), ce
}

// assertClean asserts none of the planted characters is in out, with this
// process's remedy marker followed by the word rancher taken out first (a
// writer that keeps it, as a prompt label does, still writes no other
// marker character).
func assertClean(t *testing.T, where, out string, seqs []terminalSequence) {
	t.Helper()
	rest := strings.ReplaceAll(out, clierr.Rancher, "")
	for _, s := range seqs {
		assert.NotContains(t, rest, s.text, "%s holds the planted %s: %q", where, s.name, out)
	}
}

// assertNoForgedLine asserts no line of out starts, after leading spaces,
// with the text a hostile value puts on its second line.
func assertNoForgedLine(t *testing.T, where, out string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "%s has a line of its own from a value: %q", where, out)
	}
}

// assertBothClean asserts neither stream holds a planted character.
func assertBothClean(t *testing.T, stdout, stderr string, seqs []terminalSequence) {
	t.Helper()
	assertClean(t, "standard output", stdout, seqs)
	assertClean(t, "standard error", stderr, seqs)
}

// terminalRow is one channel the pipeline or the prompter writes to a
// terminal, or writes raw, and the run that plants text in it.
type terminalRow struct {
	name string
	run  func(e terminalEnv)
}

// terminalRowNames are the names of the rows, which the lists of writers in
// outputwrites_test.go name in their reasons.
func terminalRowNames() []string {
	names := make([]string, len(terminalRows))
	for i, r := range terminalRows {
		names[i] = r.name
	}
	return names
}

// previewPrinted are the paths of safety.Effect that the dry-run preview
// prints, previewNotPrinted the ones it does not (the warnings have rows of
// their own) and previewByHand the ones the row sets itself.
var (
	previewPrinted = []string{
		"DisplayKind", "DisplayName", "Server", "Cluster",
		"Diff[].Path", "Diff[].From", "Diff[].To",
		"Objects[].Kind", "Objects[].Name", "Objects[].Outcome", "Objects[].Message",
	}
	previewNotPrinted = []string{"Target", "ResourceVersion", "Objects[].Code", "Warnings[]"}
	previewByHand     = []string{"Action", "PreviewSource"}
	// previewWithCommand are the printed paths that keep this process's
	// marker, so a command of the CLI planted there expands.
	previewWithCommand = []string{"DisplayKind", "DisplayName", "Server", "Cluster"}
)

// plantedEffect is an Effect with the hostile value in every text field, and
// a command of the CLI in the four fields that keep it, with a valid action.
func plantedEffect(t *testing.T) safety.Effect {
	t.Helper()
	typ := reflect.TypeFor[safety.Effect]()
	assert.ElementsMatch(t, remedytest.TextPaths(typ), slices.Concat(previewPrinted, previewNotPrinted, previewByHand),
		"every text field of an Effect is printed, not printed or set by the row")
	planted := remedytest.PlantText(t, typ, func(p string) string {
		if slices.Contains(previewWithCommand, p) {
			return remedyText(p)
		}
		return terminalHostile(p)
	})
	eff, ok := reflect.TypeAssert[safety.Effect](planted)
	require.True(t, ok)
	eff.Action = safety.ActionUpdate
	eff.PreviewSource = safety.PreviewServerDryRun
	return eff
}

// previewDescriptor is the descriptor of a mutating command whose preview a
// row prints.
func previewDescriptor() runtime.Descriptor {
	return runtime.Descriptor{Name: "clusters scale", CommandType: runtime.Mutating}
}

// assertWarning asserts neither stream holds a planted character and that
// standard error is the one warning line of the row: filtered, as one line,
// with the command of this process expanded.
func (e terminalEnv) assertWarning(stdout, stderr, row string) {
	e.t.Helper()
	assertBothClean(e.t, stdout, stderr, terminalSequences)
	assertNoForgedLine(e.t, "standard error", stderr)
	assert.Equal(e.t, "# warning: "+e.remedyExpectedLine(row)+"\n", stderr)
}

// warningText is the warnings of a row: one text with a command of this
// process and the hostile value for the row.
func warningText(row string) []string { return []string{remedyText(row)} }

var terminalRows = []terminalRow{
	{"text", func(e terminalEnv) {
		t := e.t
		res := runtime.Result{Text: remedyText("text") + "\n"}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, nil)
		assertBothClean(t, out, errOut, terminalSequences)
		assert.Equal(t, e.remedyExpected("text")+"\n", out)
	}},
	{"styled text", func(e terminalEnv) {
		t := e.t
		text := "run " + clierr.Rancher + " auth status"
		on := style.Style{Enabled: true}
		res := runtime.Result{Text: text + "\n", StyledText: func(s style.Style) string { return s.Bold(text) + "\n" }}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, func(io *runtime.IO) { io.Color = "always" })
		assertBothClean(t, out, errOut, terminalColorSequences)
		assert.Equal(t, on.Bold("run "+e.expanded)+"\n", out, "the CLI's own codes stay")
	}},
	{"styled text with server text", func(e terminalEnv) {
		t := e.t
		text := remedyText("styled text with server text")
		res := runtime.Result{Text: text + "\n", StyledText: func(s style.Style) string { return s.Bold(text) + "\n" }}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, func(io *runtime.IO) { io.Color = "always" })
		assertBothClean(t, out, errOut, terminalColorSequences)
		assert.Equal(t, e.remedyExpected("styled text with server text")+"\n", out, "a text with server text is written without styling")
	}},
	{"styled text adds conceal", func(e terminalEnv) {
		t := e.t
		text := "run " + clierr.Rancher + " auth status"
		res := runtime.Result{Text: text + "\n", StyledText: func(style.Style) string { return "\x1b[8m" + text + "\n" }}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, func(io *runtime.IO) { io.Color = "always" })
		assertBothClean(t, out, errOut, terminalColorSequences)
		assert.Equal(t, "run "+e.expanded+"\n", out, "a styled text that adds a sequence of its own is written without styling")
	}},
	{"styled text adds clear screen", func(e terminalEnv) {
		t := e.t
		text := "run " + clierr.Rancher + " auth status"
		res := runtime.Result{Text: text + "\n", StyledText: func(style.Style) string { return "\x1b[2J" + text + "\n" }}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, func(io *runtime.IO) { io.Color = "always" })
		assertBothClean(t, out, errOut, terminalColorSequences)
		assert.Equal(t, "run "+e.expanded+"\n", out, "a styled text that adds a sequence of its own is written without styling")
	}},
	{"wide text", func(e terminalEnv) {
		t := e.t
		res := runtime.Result{Text: "narrow\n", WideText: remedyText("wide text") + "\n"}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, func(io *runtime.IO) { io.Format = "wide" })
		assertBothClean(t, out, errOut, terminalSequences)
		assert.Equal(t, e.remedyExpected("wide text")+"\n", out)
	}},
	{"once-only line", func(e terminalEnv) {
		t := e.t
		value := terminalHostile("once-only line")
		line, notice := runtime.OnceOnlyLine("token", value, "token")
		d := runtime.Descriptor{Name: "create widget", CommandType: runtime.Mutating, Delivery: runtime.DeliveryOnceOnly, Shape: runtime.ShapeText}
		res := runtime.Result{
			Kind:             "WidgetCreated",
			Data:             terminalValue{Value: value},
			Text:             line + "\n",
			OnceOnlyNotice:   notice,
			CommittedContext: &runtime.CommittedContext{ResourceKind: "Widget", ResourceName: "w1", Recovery: "get widget w1"},
		}
		eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionCreate, DisplayKind: "widget", DisplayName: "w1"}
		out, errOut := e.run(terminalMutating{d: d, eff: eff, res: res}, func(io *runtime.IO) { io.Apply = true })
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard output", out)
		assert.Contains(t, out, strconv.Quote(value), "the value is complete, in Go's quoted form")
		assert.Contains(t, out, runtime.QuotedValueNote)
		assert.Equal(t, "token "+runtime.QuotedValueNote+": "+strconv.Quote(value)+runtime.ShownOnceSuffix+"\n", out)
	}},
	{"summary line", func(e terminalEnv) {
		t := e.t
		res := runtime.Result{Data: terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}, Summary: remedyText("summary line")}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: res}, nil)
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard output", out)
		assert.True(t, strings.HasPrefix(out, e.remedyExpectedLine("summary line")+"\nNAME\n"), "the summary is one line over the table: %q", out)
	}},
	{"preview", func(e terminalEnv) {
		t := e.t
		eff := plantedEffect(t)
		var shown strings.Builder
		for _, outcome := range []string{eff.Objects[0].Outcome, safety.OutcomeNotDeleted, safety.OutcomeFailed} {
			run := eff
			run.Objects = slices.Clone(eff.Objects)
			run.Objects[0].Outcome = outcome
			out, errOut := e.run(terminalMutating{d: previewDescriptor(), eff: run}, func(io *runtime.IO) { io.DryRun = true })
			assertBothClean(t, out, errOut, terminalSequences)
			assertNoForgedLine(t, "standard output", out)
			assertNoForgedLine(t, "standard error", errOut)
			assert.Equal(t, len(previewWithCommand), strings.Count(out, e.expanded), "each of the four fields expands its command: %q", out)
			shown.WriteString(out)
		}
		for _, p := range previewPrinted {
			assert.Contains(t, shown.String(), p, "the preview prints the text planted at %s", p)
		}
	}},
	{"table", func(e terminalEnv) {
		t := e.t
		data := terminalTable{cols: []string{terminalHostile("table header")}, rows: [][]string{{terminalHostile("table cell")}}}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: data}}, func(io *runtime.IO) { io.Format = "table" })
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard output", out)
		assert.Contains(t, out, terminalFilteredLine("table header"))
		assert.Contains(t, out, terminalFilteredLine("table cell"))
	}},
	{"wide", func(e terminalEnv) {
		t := e.t
		data := terminalTable{cols: []string{terminalHostile("wide header")}, rows: [][]string{{terminalHostile("wide cell")}}}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: data}}, func(io *runtime.IO) { io.Format = "wide" })
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard output", out)
		assert.Contains(t, out, terminalFilteredLine("wide header"))
		assert.Contains(t, out, terminalFilteredLine("wide cell"))
	}},
	{"name", func(e terminalEnv) {
		t := e.t
		data := terminalTable{cols: []string{"NAME"}, rows: [][]string{{terminalHostile("name")}}}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: data}}, func(io *runtime.IO) { io.Format = "name" })
		assertBothClean(t, out, errOut, terminalSequences)
		assert.Equal(t, terminalFilteredLine("name")+"\n", out)
	}},
	{"warnings text", func(e terminalEnv) {
		res := runtime.Result{Text: "ok\n", Warnings: warningText("warnings text")}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, nil)
		e.assertWarning(out, errOut, "warnings text")
	}},
	{"warnings raw bytes", func(e terminalEnv) {
		t := e.t
		res := runtime.Result{RawBytes: []byte("bytes\n"), Warnings: warningText("warnings raw bytes")}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeBytes), res: res}, nil)
		assert.Equal(t, "bytes\n", out)
		e.assertWarning(out, errOut, "warnings raw bytes")
	}},
	{"warnings exec credential", func(e terminalEnv) {
		res := runtime.Result{Raw: true, Data: map[string]string{"kind": "ExecCredential"}, Warnings: warningText("warnings exec credential")}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeExecJSON), res: res}, nil)
		e.assertWarning(out, errOut, "warnings exec credential")
	}},
	{"warnings envelope", func(e terminalEnv) {
		t := e.t
		data := terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}
		res := runtime.Result{Kind: "Report", Data: data, Warnings: warningText("warnings envelope")}
		out, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: res}, func(io *runtime.IO) { io.Format = "name" })
		assert.Equal(t, "a\n", out)
		e.assertWarning(out, errOut, "warnings envelope")
	}},
	{"warnings preview text", func(e terminalEnv) {
		eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: "c", Warnings: warningText("warnings preview text")}
		out, errOut := e.run(terminalMutating{d: previewDescriptor(), eff: eff}, func(io *runtime.IO) { io.DryRun = true })
		e.assertWarning(out, errOut, "warnings preview text")
	}},
	{"warnings preview envelope", func(e terminalEnv) {
		eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: "c", Warnings: warningText("warnings preview envelope")}
		out, errOut := e.run(terminalMutating{d: previewDescriptor(), eff: eff}, func(io *runtime.IO) { io.DryRun = true; io.Format = "name" })
		e.assertWarning(out, errOut, "warnings preview envelope")
	}},
	{"warnings failed run", func(e terminalEnv) {
		t := e.t
		failure := clierr.New(clierr.CodeGeneric, "the command failed").WithWarnings(remedyText("warnings failed run"))
		out, errOut, ce := e.runFailing(terminalRead{d: terminalDescriptor(runtime.ShapeText), err: failure}, nil)
		require.NotNil(t, ce)
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard error", errOut)
		assert.True(t, strings.HasPrefix(errOut, "# warning: "+e.remedyExpectedLine("warnings failed run")+"\n"), "the warning line comes first, as one line: %q", errOut)
	}},
	{"error block", func(e terminalEnv) {
		t := e.t
		failure := clierr.New(clierr.CodeGeneric, remedyText("error message")).
			WithHint(remedyText("error hint")).
			WithCandidates([]clierr.Candidate{{ID: terminalHostile("candidate id"), Name: terminalHostile("candidate name")}}).
			WithDetails(terminalDetails{lines: []string{terminalHostile("error details")}})
		out, errOut, ce := e.runFailing(terminalRead{d: terminalDescriptor(runtime.ShapeText), err: failure}, nil)
		require.NotNil(t, ce)
		assertBothClean(t, out, errOut, terminalSequences)
		assertNoForgedLine(t, "standard error", errOut)
		assert.Equal(t, 2, strings.Count(errOut, e.expanded), "the message and the hint expand their command: %q", errOut)
		for _, p := range []string{"error message", "error hint", "candidate id", "candidate name", "error details"} {
			assert.Contains(t, errOut, terminalFilteredLine(p))
		}
	}},
	{"notice", func(e terminalEnv) {
		t := e.t
		for _, format := range []string{"", "json", "yaml"} {
			cmd := terminalRead{
				d:   terminalDescriptor(runtime.ShapeTabular),
				res: runtime.Result{Kind: "Report", Data: terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}},
				act: func(_ context.Context, p runtime.Prompter) { p.Notice(remedyText("notice")) },
			}
			_, errOut := e.run(cmd, func(io *runtime.IO) { io.Format = format })
			assertClean(t, "standard error under "+strconv.Quote(format), errOut, terminalSequences)
			assert.Equal(t, e.remedyExpected("notice")+"\n", errOut, "format %q", format)
		}
	}},
	{"prompt label", func(e terminalEnv) {
		t := e.t
		for _, format := range []string{"", "json", "yaml"} {
			cmd := terminalRead{
				d:   terminalDescriptor(runtime.ShapeTabular),
				res: runtime.Result{Kind: "Report", Data: terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}},
				act: func(ctx context.Context, p runtime.Prompter) {
					_, err := p.Prompt(ctx, remedyText("prompt label"))
					require.NoError(t, err)
				},
			}
			_, errOut := e.run(cmd, func(io *runtime.IO) { io.Format = format; io.IsTTY = true; io.In = strings.NewReader("answer\n") })
			assertClean(t, "standard error under "+strconv.Quote(format), errOut, terminalSequences)
			assert.Equal(t, "run "+clierr.Rancher+" auth status "+terminalFilteredLines("prompt label"), errOut, "a label is written with this process's marker as it is, format %q", format)
			assert.NotContains(t, errOut, "--config")
		}
	}},
	{"secret prompt label", func(e terminalEnv) {
		t := e.t
		for _, format := range []string{"", "json", "yaml"} {
			cmd := terminalRead{
				d:   terminalDescriptor(runtime.ShapeTabular),
				res: runtime.Result{Kind: "Report", Data: terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}},
				act: func(ctx context.Context, p runtime.Prompter) {
					_, err := p.PromptSecret(ctx, remedyText("secret prompt label"))
					require.NoError(t, err)
				},
			}
			_, errOut := e.run(cmd, func(io *runtime.IO) { io.Format = format; io.IsTTY = true; io.In = strings.NewReader("answer\n") })
			assertClean(t, "standard error under "+strconv.Quote(format), errOut, terminalSequences)
			assert.Equal(t, "run "+clierr.Rancher+" auth status "+terminalFilteredLines("secret prompt label"), errOut, "a label is written with this process's marker as it is, format %q", format)
			assert.NotContains(t, errOut, "--config")
		}
	}},
	{"logs text", func(e terminalEnv) {
		t := e.t
		planted := terminalHostile("logs text")
		d := runtime.Descriptor{Name: "logs", CommandType: runtime.Local, Shape: runtime.ShapeText, PlainText: true}
		out, _ := e.run(terminalRead{d: d, res: runtime.Result{Text: planted + "\n"}}, nil)
		assert.Equal(t, planted+"\n", out, "the output of a program is written as it is")
	}},
	{"json", func(e terminalEnv) {
		t := e.t
		planted := terminalHostile("json")
		out, _ := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: terminalValue{Value: planted}}}, func(io *runtime.IO) { io.Format = "json" })
		var doc struct {
			Data terminalValue `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &doc))
		assert.Equal(t, planted, doc.Data.Value)
	}},
	{"yaml", func(e terminalEnv) {
		t := e.t
		planted := terminalHostile("yaml")
		out, _ := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: terminalValue{Value: planted}}}, func(io *runtime.IO) { io.Format = "yaml" })
		var doc struct {
			Data terminalValue `json:"data"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
		assert.Equal(t, planted, doc.Data.Value)
		assert.NotContains(t, out, "\x7f", "DEL is written as an escape")
		assert.NotContains(t, out, "\u009b", "the one-byte sequence introducer is written as an escape")
	}},
	{"template", func(e terminalEnv) {
		t := e.t
		planted := terminalHostile("template")
		out, _ := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: runtime.Result{Kind: "Report", Data: terminalValue{Value: planted}}}, func(io *runtime.IO) { io.Format = "template={{.value}}" })
		assert.Equal(t, planted+"\n", out)
	}},
	{"raw bytes", func(e terminalEnv) {
		t := e.t
		planted := terminalHostile("raw bytes")
		out, _ := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeBytes), res: runtime.Result{RawBytes: []byte(planted)}}, nil)
		assert.Equal(t, planted, out, "the bytes of a file are written as they are")
	}},
}

// TestServerTextCannotDriveTheTerminal asserts that a server value that holds
// control, escape, format, separator and newline characters, planted in each
// channel the pipeline and the prompter write to a terminal, reaches the
// terminal with none of them (a text print and a notice keep the newline
// between lines; every other channel is one line); that a command of this
// process planted in the same text still expands, and a label keeps it as
// the prompter wrote it; that a styled text is written only when it adds
// nothing but the CLI's own codes; and that json, yaml, template, logs text
// and bytes keep the planted text, with yaml writing DEL, U+0080 to U+009F,
// U+FFFE and U+FFFF as escapes. Each row runs through the exported pipeline with its
// own config directory. The package runs without the race detector, so a row
// starts no goroutine of its own.
func TestServerTextCannotDriveTheTerminal(t *testing.T) {
	t.Parallel()
	for _, row := range terminalRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(newTerminalEnv(t))
		})
	}
}
