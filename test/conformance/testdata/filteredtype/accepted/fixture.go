// Package accepted is a fixture of the filtered-type scan: no line of it
// has a finding.
package accepted

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// filter stands for a builder filter: the config lets it call NewFiltered.
func filter(v string) clierr.Filtered { return clierr.NewFiltered(v, false) }

// sinks hands a Filtered to each sink through Marked.
func sinks(p runtime.Prompter, v string) runtime.Result {
	t := filter(v)
	p.Notice(t.Marked())
	res := runtime.Result{Text: t.Marked(), WideText: t.Marked()}
	res.Text = t.Marked()
	res.Text, res.WideText = t.Marked(), t.Marked()
	return res
}

// styled reads WithoutMark in the StyledText literal of a Result.
func styled(v string) runtime.Result {
	t := filter(v)
	return runtime.Result{Text: t.Marked(), StyledText: func(style.Style) string { return t.WithoutMark() }}
}

// field has the shape of config status's field.
type field struct {
	key clierr.Filtered
	val any
}

// restyle reads WithoutMark in a listed function and returns a StyledText
// builder.
func restyle(t clierr.Filtered) func(style.Style) string {
	return func(style.Style) string { return t.WithoutMark() }
}

// restyled assigns the call of a listed function to the StyledText of a Result.
func restyled(t clierr.Filtered) runtime.Result {
	var res runtime.Result
	res.StyledText = restyle(t)
	return res
}

// jsonProjection reads WithoutMark in a listed function.
func jsonProjection(f field) string { return f.key.WithoutMark() }

// formats writes Filtered arguments through the two format functions and
// builds the two listed field values.
func formats(v string) []field {
	t := filter(v)
	var b clierr.Builder
	b.Printf("%-24s|", t)
	_ = clierr.Sprintf("%s", t)
	_ = clierr.Sprintf("%v %d", t, 1)
	inner := []field{{clierr.Plain("k"), "v"}}
	return []field{{clierr.Plain("a"), t}, {clierr.Plain("b"), inner}}
}

// copies reads Result.Text and WideText in the accepted ways.
func copies(src runtime.Result) runtime.Result {
	var dst runtime.Result
	dst.Text = src.Text
	dst.Text, dst.WideText = src.Text, src.WideText
	if src.Text == "" || src.WideText != "" {
		dst.Text += "\n"
	}
	t := clierr.Plain(src.Text).TrimSuffix("\n")
	dst.Text = t.Marked()
	return dst
}

// methods use the methods of Filtered that return a bool, an int or another
// Filtered.
func methods(v string) bool {
	t := filter(v).Append(clierr.Plain(" x")).AppendString("y")
	parts := []clierr.Filtered{t, clierr.Plain("z")}
	j := clierr.JoinFiltered(parts, ", ")
	return j.IsEmpty() || t.Width() > 3
}

// prompter is the one type the config lets implement runtime.Prompter.
type prompter struct{}

func (prompter) Prompt(context.Context, string) (string, error)       { return "", nil }
func (prompter) PromptSecret(context.Context, string) (string, error) { return "", nil }
func (prompter) ReadLine(context.Context) (string, error)             { return "", nil }
func (prompter) Notice(string)                                        {}
func (prompter) CanPrompt() bool                                      { return false }
func (prompter) NoInput() bool                                        { return true }

var (
	_                  = sinks
	_                  = styled
	_                  = jsonProjection
	_                  = restyled
	_                  = formats
	_                  = copies
	_                  = methods
	_ runtime.Prompter = prompter{}
)

// lines hands a line to a string form: a row lets it call Marked.
func lines(f clierr.Filtered) string { return f.Marked() }

// render reads a Text whole: a row lets it.
func render(r runtime.Result) string { return r.Text }

// keep stores a value whole; a row lets it take a runtime.Result.
func keep[T any](v T) T { return v }

// readVal reads the any field in its listed function, as the tag of a type
// switch with one type per case.
func readVal(f field) int {
	switch v := f.val.(type) {
	case clierr.Filtered:
		return v.Width()
	case []field:
		return len(v)
	case bool, int:
		return 1
	default:
		return 0
	}
}

// withRewrite is the listed caller of WithNoticeRewrite.
func withRewrite() *ui.Prompter {
	var streams runtime.IO
	return ui.New(os.Stderr, nil, false, false).WithNoticeRewrite(streams.ExpandText)
}

var (
	_ = lines
	_ = render
	_ = keep(runtime.Result{})
	_ = readVal
	_ = withRewrite
)

// renderPlain is a renderer row that hands its reads whole to the next step:
// a local the text is assigned to, output.PrintableLines of it, a plain %s,
// and trims whose constant has no character of clierr.RemovalMark.
func renderPlain(r runtime.Result) string {
	text := r.Text
	if r.WideText != "" {
		text = r.WideText
	}
	clean := output.PrintableLines(text)
	return strings.TrimSuffix(clean, "\n") + strings.TrimRight(r.Text, " ") + fmt.Sprintf("%s", text)
}

// renderTwo is a renderer row that returns the text of renderPlain, another
// renderer row, joined with its own read: a renderer row may return its text
// to a renderer row.
func renderTwo(r runtime.Result) string {
	t := r.Text
	return renderPlain(r) + t
}
